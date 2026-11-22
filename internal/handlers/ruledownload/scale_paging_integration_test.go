//go:build integration

package ruledownload

// The paged machine-specific tail against DynamoDB Local, with a response budget small enough that a few rules fill
// a page. A simulated Santa 2026.8 (fakesanta) syncs through the real preflight and postflight handlers and a
// ruledownload handler wired as Boot wires it, except for the budget, which only this package can set.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/handlers/postflight"
	"github.com/airbnb/rudolph/internal/handlers/preflight"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/internal/testutil/fakesanta"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	"github.com/airbnb/rudolph/pkg/model/groups"
	"github.com/airbnb/rudolph/pkg/model/machinerules"
	"github.com/airbnb/rudolph/pkg/model/rules"
	rudolphtypes "github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestMain(m *testing.M) { os.Exit(ddblocal.Main(m)) }

const scaleBudget = 400

// scaleMachineIDs are the identifiers (scaleSHA) of the fixture's nine machine rules, spaced so that a rule can be
// added between two of them.
var scaleMachineIDs = []int{0x10, 0x20, 0x30, 0x40, 0x50, 0x60, 0x70, 0x80, 0x90}

// scaleGlobalID is the fixture's one global rule, unrelated to the machine rules.
const scaleGlobalID = 0xf0

func scaleSortKey(i int) string {
	return rules.RuleSortKeyFromTypeIdentifier(scaleSHA(i), rudolphtypes.RuleTypeBinary)
}

// scaleServer routes a simulated Mac's requests to real handlers over DynamoDB Local.
type scaleServer struct {
	t            *testing.T
	preflight    *preflight.PostPreflightHandler
	postflight   *postflight.PostPostflightHandler
	ruledownload *PostRuledownloadHandler
	// dropPostflight answers every postflight 500 without serving it, like a broken route
	dropPostflight bool
	// firstRuledownload, when set, replaces the body of the next sync's first ruledownload request: the cursor an
	// earlier version of Rudolph issued for the sync
	firstRuledownload string
	// afterPage runs after each ruledownload page of a sync is served (page counts from 1)
	afterPage func(page int, resp RuledownloadResponse)

	mu sync.Mutex
	// pages are the ruledownload responses of the current sync
	pages []RuledownloadResponse
	// serving is the page being served, for hooks (0 between pages)
	serving int
}

func newScaleServer(t *testing.T, budget int) *scaleServer {
	t.Helper()
	ddblocal.Reset(t)
	client := ddblocal.Client(t)
	pages := machineRulePage{
		queryer: client, getter: client, batchGetter: client, batchWriter: client, updater: client, deleter: client,
		timer: clock.ConcreteTimeProvider{}, groups: groups.NewCache(clock.ConcreteTimeProvider{}),
		responseBudget: budget,
	}
	s := &scaleServer{
		t:          t,
		preflight:  &preflight.PostPreflightHandler{},
		postflight: &postflight.PostPostflightHandler{},
		ruledownload: &PostRuledownloadHandler{
			booted:        true,
			cursorService: concreteRuledownloadCursorService{timer: clock.ConcreteTimeProvider{}, updater: client, getter: client},
			ghandler:      concreteGlobalRuleDownloader{queryer: client, machineRules: pages},
			fhandler:      concreteFeedRuleDownloader{queryer: client, machineRules: pages},
			mhandler:      concreteMachineRuleDownloader{machineRules: pages},
		},
	}
	if err := s.preflight.Boot(); err != nil {
		t.Fatal(err)
	}
	if err := s.postflight.Boot(); err != nil {
		t.Fatal(err)
	}
	return s
}

func (s *scaleServer) transport(req events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, error) {
	switch req.Resource {
	case "/preflight/{machine_id}":
		s.mu.Lock()
		s.pages = nil
		s.mu.Unlock()
		return s.preflight.Handle(req)
	case "/postflight/{machine_id}":
		if s.dropPostflight {
			return &events.APIGatewayProxyResponse{StatusCode: 500, Body: `{"error":"dropped"}`}, nil
		}
		return s.postflight.Handle(req)
	case "/ruledownload/{machine_id}":
		s.mu.Lock()
		if len(s.pages) == 0 && s.firstRuledownload != "" {
			req.Body = s.firstRuledownload
			delete(req.Headers, "Content-Encoding")
			req.IsBase64Encoded = false
			s.firstRuledownload = ""
		}
		s.serving = len(s.pages) + 1
		s.mu.Unlock()
		resp, err := s.ruledownload.Handle(req)
		s.mu.Lock()
		s.serving = 0
		s.mu.Unlock()
		if err != nil || resp == nil || resp.StatusCode != 200 {
			return resp, err
		}
		var page RuledownloadResponse
		if err := json.Unmarshal([]byte(resp.Body), &page); err != nil {
			s.t.Fatal(err)
		}
		s.mu.Lock()
		s.pages = append(s.pages, page)
		n := len(s.pages)
		s.mu.Unlock()
		if s.afterPage != nil {
			s.afterPage(n, page)
		}
		return resp, nil
	}
	s.t.Fatalf("unexpected request %s", req.Resource)
	return nil, nil
}

func (s *scaleServer) host() *fakesanta.Host {
	h := fakesanta.NewHost(inputHandlingMachineID, s.transport)
	h.Encoding = fakesanta.EncodingNone
	return h
}

func scaleAddMachineRule(t *testing.T, i int) {
	t.Helper()
	if err := machinerules.AddNewMachineRule(ddblocal.Client(t), inputHandlingMachineID, scaleSHA(i), rudolphtypes.RuleTypeBinary, rudolphtypes.RulePolicyAllowlist, "test", time.Now().Add(24*time.Hour)); err != nil {
		t.Fatalf("add machine rule: %v", err)
	}
}

// scaleDeleteMachineRow deletes a machine rule's row, as the table's TTL does once it expired
func scaleDeleteMachineRow(t *testing.T, i int) {
	t.Helper()
	_, err := ddblocal.Raw(t).DeleteItem(context.Background(), &awsdynamodb.DeleteItemInput{
		TableName: aws.String(ddblocal.TableName),
		Key: map[string]awstypes.AttributeValue{
			"PK": &awstypes.AttributeValueMemberS{Value: machinerules.MachineRulesPK(inputHandlingMachineID)},
			"SK": &awstypes.AttributeValueMemberS{Value: scaleSortKey(i)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

// scaleFixture writes the global rule and the nine machine rules and syncs the Mac once, so that it holds them all
// and their delivery is tracked.
func scaleFixture(t *testing.T, s *scaleServer, host *fakesanta.Host) {
	t.Helper()
	if err := globalrules.AddNewGlobalRule(clock.ConcreteTimeProvider{}, ddblocal.Client(t), scaleSHA(scaleGlobalID), rudolphtypes.RuleTypeBinary, rudolphtypes.RulePolicyBlocklist, "test"); err != nil {
		t.Fatal(err)
	}
	for _, i := range scaleMachineIDs {
		scaleAddMachineRule(t, i)
	}
	// Feed rows are stamped to the second; the first sync starts after the global rule's second, so that whether a
	// later sync serves it first does not depend on when in a second the test ran
	time.Sleep(1100 * time.Millisecond)
	host.MustSync(t)
	for _, i := range append([]int{scaleGlobalID}, scaleMachineIDs...) {
		if host.Policy("BINARY", scaleSHA(i)) == "" {
			t.Fatalf("after the first sync the Mac lacks rule %#x", i)
		}
	}
}

func scaleSyncState(t *testing.T) map[string]awstypes.AttributeValue {
	t.Helper()
	out, err := ddblocal.Raw(t).GetItem(context.Background(), &awsdynamodb.GetItemInput{
		TableName: aws.String(ddblocal.TableName),
		Key: map[string]awstypes.AttributeValue{
			"PK": &awstypes.AttributeValueMemberS{Value: "Machine#" + inputHandlingMachineID},
			"SK": &awstypes.AttributeValueMemberS{Value: "SyncState"},
		},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.Item
}

func scaleAttr(item map[string]awstypes.AttributeValue, name string) string {
	switch v := item[name].(type) {
	case *awstypes.AttributeValueMemberS:
		return v.Value
	case *awstypes.AttributeValueMemberN:
		return v.Value
	case *awstypes.AttributeValueMemberBOOL:
		return fmt.Sprint(v.Value)
	case nil:
		return "<absent>"
	}
	return fmt.Sprintf("%v", item[name])
}

// scaleMachineSent is what the sync sent of the fixture's machine rules, in order, as "<id> <policy>"; and whether the
// global rule, when sent, came before all of them.
func scaleMachineSent(res *fakesanta.SyncResult) (sent []string, globalFirst bool) {
	globalFirst = true
	for _, d := range res.Downloaded {
		if d.Key.Identifier == scaleSHA(scaleGlobalID) {
			if len(sent) > 0 {
				globalFirst = false
			}
			continue
		}
		sent = append(sent, d.Key.Identifier[62:]+" "+d.Rule.Policy)
	}
	return sent, globalFirst
}

// checkPagesAreFull checks that every page is within the budget (or a single rule) and every page but the last is
// full: the first rule of the next page would not have fitted. Pages with a cursor must pass validate.
func checkPagesAreFull(t *testing.T, pages []RuledownloadResponse, budget int) {
	t.Helper()
	for i, page := range pages {
		size := 0
		for _, rule := range page.Rules {
			size += encodedRuleBytes(rule)
		}
		if size > budget && len(page.Rules) > 1 {
			t.Errorf("page %d holds %d bytes of rules, over the %d byte budget", i+1, size, budget)
		}
		if i+1 < len(pages) {
			if page.Cursor == "" {
				t.Errorf("page %d has no cursor but page %d followed", i+1, i+2)
			}
			var cursor ruledownloadCursor
			if err := json.Unmarshal([]byte(page.Cursor), &cursor); err != nil || cursor.validate() != nil {
				t.Errorf("page %d issued cursor %s, which does not pass validate", i+1, page.Cursor)
			}
			if next := pages[i+1].Rules; len(next) > 0 && len(page.Rules) > 0 && size+encodedRuleBytes(next[0]) <= budget {
				t.Errorf("page %d (%d bytes) had room for the first rule of page %d, which was left to it", i+1, size, i+2)
			}
		} else if page.Cursor != "" {
			t.Errorf("the last page has cursor %s", page.Cursor)
		}
	}
}

// Seven machine rules that apply and two tracked ones whose rows are gone take several pages at a 400 byte budget
// (two rules a page). Whatever the sync's strategy, every rule is sent exactly once, the machine rules in key order
// and then the two REMOVEs, every page is full, and every cursor issued is accepted.
func TestScale_TailPagingDeliversEverythingOnceInOrder(t *testing.T) {
	want := []string{"10 ALLOWLIST", "20 ALLOWLIST", "40 ALLOWLIST", "50 ALLOWLIST", "70 ALLOWLIST", "80 ALLOWLIST", "90 ALLOWLIST", "30 REMOVE", "60 REMOVE"}
	for _, variant := range []struct {
		name     string
		strategy ruledownloadStrategy
		prepare  func(s *scaleServer, host *fakesanta.Host)
	}{
		{"feed sync", ruledownloadStrategyIncremental, func(*scaleServer, *fakesanta.Host) {}},
		{"clean sync", ruledownloadStrategyClean, func(_ *scaleServer, host *fakesanta.Host) { host.RequestCleanSync = true }},
		{"legacy strategy 3 start", ruledownloadStrategyMachine, func(s *scaleServer, _ *fakesanta.Host) {
			s.firstRuledownload = `{"cursor":"{\"strategy\":3,\"page\":2,\"batch_size\":50}"}`
		}},
	} {
		t.Run(variant.name, func(t *testing.T) {
			s := newScaleServer(t, scaleBudget)
			host := s.host()
			scaleFixture(t, s, host)
			// One row gone (TTL), one marked for deletion (rudolph rule remove --machine)
			scaleDeleteMachineRow(t, 0x30)
			if err := machinerules.RemoveMachineRule(ddblocal.Client(t), ddblocal.Client(t), inputHandlingMachineID, scaleSortKey(0x60)); err != nil {
				t.Fatal(err)
			}
			variant.prepare(s, host)
			writes := scaleWritesPerPage(t, s)

			res := host.MustSync(t)
			sent, globalFirst := scaleMachineSent(res)
			if fmt.Sprint(sent) != fmt.Sprint(want) {
				t.Errorf("the machine-specific rules arrived as\n%v\nwant\n%v", sent, want)
			}
			if !globalFirst {
				t.Error("the global rule came after machine-specific rules")
			}
			checkPagesAreFull(t, s.pages, scaleBudget)
			// About 136 bytes a rule, so two a page: 9 machine-specific rules (and the global rule on a feed or
			// clean sync) take at least 5 pages
			if len(s.pages) < 5 || len(s.pages) > 6 {
				t.Errorf("the sync took %d pages, want 5 or 6 at two rules a page", len(s.pages))
			}
			t.Logf("%d pages", len(s.pages))
			// The first page brings tracking up to date and deletes the marked row; later pages find it up to date.
			// The last page of a clean sync records it was served.
			for page := 2; page <= len(s.pages); page++ {
				allowed := 0
				if page == len(s.pages) && variant.strategy == ruledownloadStrategyClean {
					allowed = 1
				}
				if writes[page] > allowed {
					t.Errorf("page %d made %d writes, want %d: tracking was already up to date", page, writes[page], allowed)
				}
			}
			if row, err := machinerules.GetMachineRuleConsistently(ddblocal.Client(t), inputHandlingMachineID, scaleSortKey(0x60)); err != nil || row != nil {
				t.Errorf("the row marked for deletion is still there after the sync (%v)", err)
			}
			if writes[1] == 0 {
				t.Error("the first page wrote nothing, so the fixture tests nothing")
			}
			var first ruledownloadCursor
			if err := json.Unmarshal([]byte(s.pages[0].Cursor), &first); err == nil && first.Strategy != variant.strategy {
				t.Errorf("the first page's cursor has strategy %d, want %d: the variant does not test what it says", first.Strategy, variant.strategy)
			}
			for _, i := range []int{0x30, 0x60} {
				if host.Policy("BINARY", scaleSHA(i)) != "" {
					t.Errorf("the Mac still holds removed rule %#x", i)
				}
			}
			for _, i := range []int{0x10, 0x20, 0x40, 0x50, 0x70, 0x80, 0x90, scaleGlobalID} {
				if host.Policy("BINARY", scaleSHA(i)) == "" {
					t.Errorf("the Mac lacks rule %#x", i)
				}
			}
		})
	}
}

// scaleWritesPerPage counts the DynamoDB writes made while each ruledownload page is served, by page.
func scaleWritesPerPage(t *testing.T, s *scaleServer) map[int]int {
	writes := map[int]int{}
	ddblocal.AddHook(t, ddblocal.Hook{Before: func(c *ddblocal.Call) *ddblocal.Fault {
		switch c.Op {
		case "PutItem", "UpdateItem", "DeleteItem", "BatchWriteItem", "TransactWriteItems":
			s.mu.Lock()
			if s.serving != 0 {
				writes[s.serving]++
			}
			s.mu.Unlock()
		}
		return nil
	}})
	return writes
}

// scaleLastPageServed runs the fixture on a Mac whose postflights never arrive, then two syncs after two rows were
// deleted, and returns for each of those syncs the pages during which LastPageServedAt was written, the number of
// pages, and what was recorded.
func scaleLastPageServed(t *testing.T, budget int) (writtenOn [][]int, pages []int, recorded []string) {
	s := newScaleServer(t, budget)
	s.dropPostflight = true
	host := s.host()
	host.IgnorePostflightFailure = true
	host.HashRules = true
	scaleFixture(t, s, host)
	scaleDeleteMachineRow(t, 0x30)
	scaleDeleteMachineRow(t, 0x60)

	var mu sync.Mutex
	var current []int
	ddblocal.AddHook(t, ddblocal.Hook{Before: func(c *ddblocal.Call) *ddblocal.Fault {
		if c.Op == "UpdateItem" && strings.Contains(fmt.Sprint(c.Input), "LastPageServedAt") {
			s.mu.Lock()
			page := s.serving
			s.mu.Unlock()
			mu.Lock()
			current = append(current, page)
			mu.Unlock()
		}
		return nil
	}})
	for sync := 2; sync <= 3; sync++ {
		mu.Lock()
		current = nil
		mu.Unlock()
		host.MustSync(t)
		state := scaleSyncState(t)
		if scaleAttr(state, "SyncsWithoutPostflight") == "0" || scaleAttr(state, "SyncsWithoutPostflight") == "<absent>" {
			t.Fatalf("sync %d has SyncsWithoutPostflight %s, so the fixture tests nothing", sync, scaleAttr(state, "SyncsWithoutPostflight"))
		}
		mu.Lock()
		writtenOn = append(writtenOn, current)
		mu.Unlock()
		pages = append(pages, len(s.pages))
		recorded = append(recorded, fmt.Sprintf("nothing new %s, run %s, clean %s",
			scaleAttr(state, "LastPageServedNothingNew"), scaleAttr(state, "LastPageServedRun"), scaleAttr(state, "CleanSync")))
	}
	return writtenOn, pages, recorded
}

// After syncs without postflight, the last page served is recorded once per sync, by the page without a cursor, and
// records the same as when the whole tail fits one page.
func TestScale_OnlyTheFinalTailPageRecordsTheLastPageServed(t *testing.T) {
	paged, pages, pagedRecorded := scaleLastPageServed(t, scaleBudget)
	single, singlePages, singleRecorded := scaleLastPageServed(t, 0)
	for i := range paged {
		if pages[i] < 3 {
			t.Errorf("paged sync %d took %d pages, want several", i+2, pages[i])
		}
		if singlePages[i] != 1 {
			t.Errorf("sync %d at the default budget took %d pages, want 1", i+2, singlePages[i])
		}
		if len(paged[i]) != 1 || paged[i][0] != pages[i] {
			t.Errorf("paged sync %d of %d pages wrote LastPageServedAt during pages %v, want once, on page %d", i+2, pages[i], paged[i], pages[i])
		}
		if len(single[i]) != 1 {
			t.Errorf("single-page sync %d wrote LastPageServedAt %d times, want once", i+2, len(single[i]))
		}
		if pagedRecorded[i] != singleRecorded[i] {
			t.Errorf("sync %d recorded %q over %d pages, but %q on one page", i+2, pagedRecorded[i], pages[i], singleRecorded[i])
		}
	}
	t.Logf("recorded: %v", pagedRecorded)
}

// The tail is built again for every page and goes on from a sort key, so rules that change between two pages are
// neither skipped nor lost.
func TestScale_TailToleratesChangesBetweenPages(t *testing.T) {
	t.Run("a machine rule added before the resume key", func(t *testing.T) {
		s := newScaleServer(t, scaleBudget)
		host := s.host()
		scaleFixture(t, s, host)
		const added = 0x11 // between the first and second machine rules
		addedAfter := 0
		s.afterPage = func(page int, resp RuledownloadResponse) {
			var cursor ruledownloadCursor
			if addedAfter != 0 || resp.Cursor == "" || json.Unmarshal([]byte(resp.Cursor), &cursor) != nil {
				return
			}
			if cursor.TailSection == tailMachineRules && cursor.TailSortKey >= scaleSortKey(scaleMachineIDs[1]) {
				scaleAddMachineRule(t, added)
				addedAfter = page
			}
		}
		res := host.MustSync(t)
		if addedAfter == 0 {
			t.Fatal("no page ended past the second machine rule with more to come, so the fixture tests nothing")
		}
		for _, d := range res.Downloaded {
			if d.Key.Identifier == scaleSHA(added) {
				t.Errorf("the rule added before the resume key after page %d was sent by the same sync", addedAfter)
			}
		}
		sent, _ := scaleMachineSent(res)
		if len(sent) != len(scaleMachineIDs) {
			t.Errorf("the sync sent %v, want the nine machine rules once each", sent)
		}
		deliveries, err := machinerules.GetDeliveries(ddblocal.Client(t), inputHandlingMachineID)
		if err != nil {
			t.Fatal(err)
		}
		if delivery, ok := deliveries[scaleSortKey(added)]; !ok || delivery.RemovalFirstSentInSync != 0 {
			t.Errorf("after the sync the added rule's tracking is %+v (present %t), want tracked as applying", delivery, ok)
		}

		s.afterPage = nil
		res = host.MustSync(t)
		found := false
		for _, d := range res.Downloaded {
			found = found || d.Key.Identifier == scaleSHA(added)
		}
		if !found || host.Policy("BINARY", scaleSHA(added)) != "ALLOWLIST" {
			t.Errorf("the next sync did not deliver the added rule (sent %t, the Mac has %q)", found, host.Policy("BINARY", scaleSHA(added)))
		}
	})

	t.Run("an already sent machine rule removed", func(t *testing.T) {
		s := newScaleServer(t, scaleBudget)
		host := s.host()
		scaleFixture(t, s, host)
		removed := scaleMachineIDs[0]
		removedAfter := 0
		s.afterPage = func(page int, resp RuledownloadResponse) {
			var cursor ruledownloadCursor
			if removedAfter != 0 || resp.Cursor == "" || json.Unmarshal([]byte(resp.Cursor), &cursor) != nil {
				return
			}
			if cursor.TailSection == tailMachineRules && cursor.TailSortKey >= scaleSortKey(removed) {
				scaleDeleteMachineRow(t, removed)
				removedAfter = page
			}
		}
		res := host.MustSync(t)
		if removedAfter == 0 {
			t.Fatal("no page ended past the first machine rule with more to come, so the fixture tests nothing")
		}
		var policies []string
		for _, d := range res.Downloaded {
			if d.Key.Identifier == scaleSHA(removed) {
				policies = append(policies, d.Rule.Policy)
			}
		}
		if fmt.Sprint(policies) != fmt.Sprint([]string{"ALLOWLIST", "REMOVE"}) {
			t.Errorf("the rule removed after page %d was sent as %v during the sync, want ALLOWLIST and then its REMOVE", removedAfter, policies)
		}
		if got := host.Policy("BINARY", scaleSHA(removed)); got != "" {
			t.Errorf("after the sync the Mac still has %q for the removed rule", got)
		}
		deliveries, err := machinerules.GetDeliveries(ddblocal.Client(t), inputHandlingMachineID)
		if err != nil {
			t.Fatal(err)
		}
		if deliveries[scaleSortKey(removed)].RemovalFirstSentInSync == 0 {
			t.Errorf("the removed rule's tracking is %+v, want its removal recorded as sent", deliveries[scaleSortKey(removed)])
		}
	})
}
