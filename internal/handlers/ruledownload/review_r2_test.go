package ruledownload

// Second review of the input handling: what /ruledownload answers for cursors and machines that the fixes in
// 9580b58 and 9ff86ca did not consider, and the cursors validate must keep accepting.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/feedrules"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	"github.com/airbnb/rudolph/pkg/model/machinerules"
	"github.com/airbnb/rudolph/pkg/model/syncstate"
	"github.com/airbnb/rudolph/pkg/response"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

// r2CurrentSync is the SyncSeq of the machine's current sync in r2Table.
const r2CurrentSync = 1_790_000_000_000

// r2Table stands in for the table: GetItem returns the machine's sync state (none when syncState is nil) and every
// Query is answered with an empty last page, which is what a fleet without rules returns. It has no writer, so a
// request that tries to write panics, which the tests below report.
type r2Table struct {
	t         *testing.T
	syncState *syncstate.SyncStateRow
	queries   int
	gets      int
}

func (r *r2Table) GetItem(key dynamodb.PrimaryKey, _ bool) (*awsdynamodb.GetItemOutput, error) {
	r.gets++
	if r.syncState == nil || key.PartitionKey != "Machine#"+inputHandlingMachineID || key.SortKey != "SyncState" {
		return &awsdynamodb.GetItemOutput{}, nil
	}
	item, err := attributevalue.MarshalMap(r.syncState)
	if err != nil {
		r.t.Fatal(err)
	}
	return &awsdynamodb.GetItemOutput{Item: item}, nil
}

func (r *r2Table) Query(*awsdynamodb.QueryInput) (*awsdynamodb.QueryOutput, error) {
	r.queries++
	return &awsdynamodb.QueryOutput{}, nil
}

// r2SyncState is a current incremental sync whose last page writes nothing: machine rule delivery is tracked and
// matches the (no) machine rules, its expiry was just extended, and the previous sync received its postflight.
func r2SyncState() *syncstate.SyncStateRow {
	row := &syncstate.SyncStateRow{}
	row.MachineID = inputHandlingMachineID
	row.DataType = syncstate.GetDataType()
	row.BatchSize = 50
	row.FeedSyncCursor = "2000-01-01T00:00:00Z"
	row.SyncSeq = r2CurrentSync
	row.MachineRulesTracked = machinerules.DeliveryDigest(map[string]machinerules.Delivery{})
	row.MachineRulesTrackingRefreshedAt = "2000-01-01T00:00:00Z"
	return row
}

// r2Handler is the handler as Boot wires it, with every part real, over table.
func r2Handler(table *r2Table) *PostRuledownloadHandler {
	pages := machineRulePage{queryer: table, getter: table, timer: mockTimeProvider{}}
	return &PostRuledownloadHandler{
		booted:        true,
		cursorService: concreteRuledownloadCursorService{getter: table},
		ghandler:      concreteGlobalRuleDownloader{queryer: table, machineRules: pages},
		fhandler:      concreteFeedRuleDownloader{queryer: table, machineRules: pages},
		mhandler:      concreteMachineRuleDownloader{machineRules: pages},
	}
}

// r2Body is a ruledownload body carrying cursor, or an empty body (a first page) when cursor is "".
func r2Body(t *testing.T, cursor string) string {
	if cursor == "" {
		return `{}`
	}
	body, err := json.Marshal(map[string]string{"cursor": cursor})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// r2Handle sends body and fails the test if the handler panics or gives no response.
func r2Handle(t *testing.T, h *PostRuledownloadHandler, body string) *events.APIGatewayProxyResponse {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("the handler panicked: %v", r)
		}
	}()
	resp, err := h.Handle(inputHandlingRequest(body))
	if resp == nil {
		t.Fatalf("ruledownload gave no response: %v", err)
	}
	return resp
}

// r2WantError checks the status and that the body is an error response like the others in pkg/response; the
// wording is left to the fix.
func r2WantError(t *testing.T, resp *events.APIGatewayProxyResponse, status int, what string) {
	t.Helper()
	if resp.StatusCode != status {
		t.Errorf("%s was answered %d (%s), want %d", what, resp.StatusCode, resp.Body, status)
		return
	}
	var body response.ErrorResponse
	if err := json.Unmarshal([]byte(resp.Body), &body); err != nil || body.Error == "" {
		t.Errorf("%s was answered %d with body %q, want an error response", what, status, resp.Body)
	}
}

// The last page of a sync that a newer sync of the machine has overtaken (Santa gave up on it and preflighted
// again), or a cursor whose seq was forged, is refused before anything is recorded (ErrSyncSuperseded). That is no
// server failure: answered 500, the api_5xx alarm counts it and Santa retries a request that can never succeed. The
// owner decided it is a 409 Conflict, for the real Mac and the forged cursor alike.
func TestReviewR2_CursorOfASupersededSyncIsAConflict(t *testing.T) {
	older := r2CurrentSync - 1
	for name, cursor := range map[string]ruledownloadCursor{
		"clean sync, last page": {
			Strategy: ruledownloadStrategyClean, BatchSize: 50, PageNumber: 2, SyncSeq: older,
			ruledownloadCursorDDBLastEvaluatedKey: ruledownloadCursorDDBLastEvaluatedKey{PartitionKey: globalrules.Partition, SortKey: "Binary#aa"},
		},
		"feed sync, last page": {
			Strategy: ruledownloadStrategyIncremental, BatchSize: 50, PageNumber: 2, SyncSeq: older,
			ruledownloadCursorDDBLastEvaluatedKey: ruledownloadCursorDDBLastEvaluatedKey{PartitionKey: feedrules.Partition, SortKey: "2000-01-01T00:00:00Z#Binary#aa"},
		},
		"forged seq from the future": {
			Strategy: ruledownloadStrategyClean, BatchSize: 50, PageNumber: 2, SyncSeq: r2CurrentSync + 1,
			ruledownloadCursorDDBLastEvaluatedKey: ruledownloadCursorDDBLastEvaluatedKey{PartitionKey: globalrules.Partition, SortKey: "Binary#aa"},
		},
		"machine page (strategy 3)": {Strategy: ruledownloadStrategyMachine, SyncSeq: older},
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(cursor)
			if err != nil {
				t.Fatal(err)
			}
			table := &r2Table{t: t, syncState: r2SyncState()}
			resp := r2Handle(t, r2Handler(table), r2Body(t, string(raw)))
			r2WantError(t, resp, 409, "the last page of a superseded sync (cursor "+string(raw)+")")
		})
	}
}

// The control for the test above: the same last pages with the current sync's seq are served, so the fixture is not
// what fails them.
func TestReviewR2_CursorOfTheCurrentSyncIsServed(t *testing.T) {
	for name, cursor := range map[string]string{
		"clean sync, last page":     `{"strategy":1,"batch_size":50,"page":2,"pk":"GlobalRules","sk":"Binary#aa","seq":1790000000000}`,
		"feed sync, last page":      `{"strategy":2,"batch_size":50,"page":2,"pk":"RulesFeed","sk":"2000-01-01T00:00:00Z#Binary#aa","seq":1790000000000}`,
		"machine page (strategy 3)": `{"strategy":3,"seq":1790000000000}`,
		"first page":                ``,
	} {
		t.Run(name, func(t *testing.T) {
			table := &r2Table{t: t, syncState: r2SyncState()}
			resp := r2Handle(t, r2Handler(table), r2Body(t, cursor))
			if resp.StatusCode != 200 {
				t.Errorf("cursor %q of the current sync was answered %d (%s), want 200", cursor, resp.StatusCode, resp.Body)
			}
		})
	}
}

// A machine with no sync state has not preflighted (or its sync state expired): its ruledownload is the client's
// mistake, not the server's. Answered 500, the api_5xx alarm would count it and Santa would retry a request that cannot
// succeed; the owner decided 400.
func TestReviewR2_RuledownloadWithoutAPreflightIsABadRequest(t *testing.T) {
	for name, cursor := range map[string]string{
		"first page":                ``,
		"clean sync, last page":     `{"strategy":1,"batch_size":50,"page":2,"pk":"GlobalRules","sk":"Binary#aa","seq":1790000000000}`,
		"machine page (strategy 3)": `{"strategy":3}`,
	} {
		t.Run(name, func(t *testing.T) {
			table := &r2Table{t: t}
			resp := r2Handle(t, r2Handler(table), r2Body(t, cursor))
			r2WantError(t, resp, 400, "a ruledownload without a sync state")
		})
	}
}

// Rudolph never issues a negative page number or seq (pages count from 1, seq is the preflight time in
// milliseconds), so such a cursor is forged and is refused like the others in validate, before anything is read.
func TestReviewR2_CursorWithANegativeSeqOrPageIsABadRequest(t *testing.T) {
	for name, cursor := range map[string]string{
		"clean page, negative page":   `{"strategy":1,"batch_size":50,"page":-1,"pk":"GlobalRules","sk":"Binary#aa"}`,
		"clean page, negative seq":    `{"strategy":1,"batch_size":50,"page":2,"pk":"GlobalRules","sk":"Binary#aa","seq":-5}`,
		"feed page, negative page":    `{"strategy":2,"batch_size":50,"page":-3,"pk":"RulesFeed","sk":"2000-01-01T00:00:00Z#Binary#aa"}`,
		"feed page, negative seq":     `{"strategy":2,"batch_size":50,"page":2,"pk":"RulesFeed","sk":"2000-01-01T00:00:00Z#Binary#aa","seq":-1}`,
		"machine page, negative seq":  `{"strategy":3,"seq":-1}`,
		"machine page, negative page": `{"strategy":3,"page":-2}`,
	} {
		t.Run(name, func(t *testing.T) {
			table := &r2Table{t: t, syncState: r2SyncState()}
			resp := r2Handle(t, r2Handler(table), r2Body(t, cursor))
			if resp.StatusCode != 400 || table.queries != 0 || table.gets != 0 {
				t.Errorf("cursor %s was answered %d after %d queries and %d reads, want 400 without reading the table",
					cursor, resp.StatusCode, table.queries, table.gets)
			}
		})
	}
}

// The accept side of validate: every cursor the server issues must come back through it. Checked on cursors made by
// the code that issues them (lastPageOrNext), so a validate that drifts from what is issued fails here rather than
// on every Mac's second page.
func TestReviewR2_CursorsTheServerIssuesAreAccepted(t *testing.T) {
	issued := func(t *testing.T, first ruledownloadCursor, key dynamodb.PrimaryKey) string {
		t.Helper()
		resp, err := lastPageOrNext(inputHandlingMachineID, first, nil, &key, machineRulePage{}, nil)
		if err != nil || resp == nil || resp.StatusCode != 200 {
			t.Fatalf("issuing the next page's cursor failed: %v %+v", err, resp)
		}
		var page RuledownloadResponse
		if err := json.Unmarshal([]byte(resp.Body), &page); err != nil || page.Cursor == "" {
			t.Fatalf("no cursor issued: %v %s", err, resp.Body)
		}
		return page.Cursor
	}
	for name, c := range map[string]struct {
		first ruledownloadCursor
		key   dynamodb.PrimaryKey
	}{
		"clean sync, second page": {
			ruledownloadCursor{Strategy: ruledownloadStrategyClean, BatchSize: 50, PageNumber: 1, SyncSeq: r2CurrentSync},
			dynamodb.PrimaryKey{PartitionKey: globalrules.Partition, SortKey: "Binary#" + strings.Repeat("ab", 32)},
		},
		"clean sync, silent only, batch size 1": {
			ruledownloadCursor{Strategy: ruledownloadStrategyClean, BatchSize: 1, PageNumber: 7, SyncSeq: r2CurrentSync, SilentBlocklistOnly: true},
			dynamodb.PrimaryKey{PartitionKey: globalrules.Partition, SortKey: "TeamID#ABCDE12345"},
		},
		"feed sync, second page, batch size 500": {
			ruledownloadCursor{Strategy: ruledownloadStrategyIncremental, BatchSize: 500, PageNumber: 1, SyncSeq: r2CurrentSync},
			dynamodb.PrimaryKey{PartitionKey: feedrules.Partition, SortKey: "2026-11-19T00:00:00Z#Binary#" + strings.Repeat("ab", 32)},
		},
		"sync numbered by an older preflight (no seq)": {
			ruledownloadCursor{Strategy: ruledownloadStrategyIncremental, BatchSize: 50, PageNumber: 1},
			dynamodb.PrimaryKey{PartitionKey: feedrules.Partition, SortKey: "2026-11-19T00:00:00Z#Binary#aa"},
		},
		"sort key of exactly 1024 bytes": {
			ruledownloadCursor{Strategy: ruledownloadStrategyClean, BatchSize: 50, PageNumber: 1, SyncSeq: r2CurrentSync},
			dynamodb.PrimaryKey{PartitionKey: globalrules.Partition, SortKey: "Binary#" + strings.Repeat("a", 1024-len("Binary#"))},
		},
	} {
		t.Run(name, func(t *testing.T) {
			raw := issued(t, c.first, c.key)
			var cursor ruledownloadCursor
			if err := json.Unmarshal([]byte(raw), &cursor); err != nil {
				t.Fatal(err)
			}
			if err := cursor.validate(); err != nil {
				t.Errorf("the server issued cursor %.120s and then refused it: %v", raw, err)
			}
			// And end to end: the issued cursor sent back as Santa sends it is served (its seq matches the sync state).
			state := r2SyncState()
			state.SyncSeq = c.first.SyncSeq
			table := &r2Table{t: t, syncState: state}
			resp := r2Handle(t, r2Handler(table), r2Body(t, raw))
			if resp.StatusCode != 200 {
				t.Errorf("the issued cursor %.120s was answered %d (%s), want 200", raw, resp.StatusCode, resp.Body)
			}
		})
	}
}

// Cursors written by hand at validate's boundaries, and the strategy 3 cursors earlier versions of Rudolph issued
// (still in flight on Macs mid-sync during an upgrade), which carry no key and often no batch size or page.
func TestReviewR2_CursorsAtTheBoundariesAndLegacyMachineCursorsAreAccepted(t *testing.T) {
	for name, cursor := range map[string]string{
		"batch size 1":                     `{"strategy":1,"batch_size":1,"page":2,"pk":"GlobalRules","sk":"Binary#aa"}`,
		"batch size 500":                   `{"strategy":2,"batch_size":500,"page":2,"pk":"RulesFeed","sk":"2000-01-01T00:00:00Z#Binary#aa"}`,
		"sort key of exactly 1024 bytes":   `{"strategy":1,"batch_size":50,"page":2,"pk":"GlobalRules","sk":"` + strings.Repeat("a", 1024) + `"}`,
		"legacy machine cursor":            `{"strategy":3}`,
		"legacy machine cursor with pages": `{"strategy":3,"batch_size":50,"page":3}`,
		"legacy machine cursor with a key": `{"strategy":3,"batch_size":50,"page":3,"pk":"MachineRules#` + inputHandlingMachineID + `","sk":"Binary#aa"}`,
	} {
		t.Run(name, func(t *testing.T) {
			var parsed ruledownloadCursor
			if err := json.Unmarshal([]byte(cursor), &parsed); err != nil {
				t.Fatal(err)
			}
			if err := parsed.validate(); err != nil {
				t.Errorf("cursor %.120s was refused: %v", cursor, err)
			}
		})
	}
}
