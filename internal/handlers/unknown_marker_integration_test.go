//go:build integration

package handlers

// Lockdown readiness, the write side (Fix 3 of the fleet health design, owner approved 2026-11-22), end to end through
// fakesanta and DynamoDB Local:
//
//   - eventupload writes `UnknownSeen#<machine ID>` / `<file SHA-256>` in the events table for every binary with a
//     stored BLOCK_UNKNOWN or ALLOW_UNKNOWN event in the upload, with LastDay (the upload's UTC date), ExpiresAfter
//     (upload + 35 days) and the display fields pkg/counterfields allows. Blocked-by-rule and collected allowed-by-rule
//     executions get none. The item rules are pinned in internal/handlers/eventupload/unknown_marker_test.go.
//   - A failed marker write answers the upload 5xx; Santa keeps the events, uploads them again, and the retry writes the
//     marker although the events are already stored.
//   - Each marker is one PutItem of one write unit (no index entry), even with display fields at their limits: an
//     upload of N unknown binaries costs N write units more than before.
//   - Preflight stores FirstSeenDay on the sync state (UTC date of the Mac's first sync on record), carries it from
//     sync to sync, and the row stays under 1 KB, so a steady-state sync costs what COST2 allows.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func unknownMarker(t *testing.T, machineID, sha string) map[string]awstypes.AttributeValue {
	t.Helper()
	return eventsItem(t, "UnknownSeen#"+machineID, sha)
}

func TestEventupload_WritesUnknownMarkers(t *testing.T) {
	transport := eventsServer(t)
	addGlobalRule(t, teamGoogle, types.RuleTypeTeamID, types.RulePolicyAllowlist)
	host := syncedHost(t, machineA, transport)
	ruleCLI(t, "update", "-t", "teamid", "-i", teamGoogle, "--global", "--collect-allowed-events=true")

	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	collected := strings.Repeat("9e", 32)
	host.PendingEvents = []string{
		execEvent(sha1, "BLOCK_UNKNOWN", 1, at, fmt.Sprintf(`"team_id":%q,"signing_id":%q`, otherTeam, otherTeam+":com.example.tool")),
		execEvent(sha1, "BLOCK_UNKNOWN", 2, at.Add(time.Second), ""),
		execEvent(sha2, "ALLOW_UNKNOWN", 3, at, ""),
		execEvent(sha3, "BLOCK_BINARY", 4, at, ""),
		execEvent(collected, "ALLOW_TEAMID", 5, at, fmt.Sprintf(`"team_id":%q`, teamGoogle)),
	}
	before := time.Now().UTC().Format("2006-01-02")
	host.MustSync(t)
	after := time.Now().UTC().Format("2006-01-02")

	if !storedSHAs(t, machineA)[collected] {
		t.Fatal("precondition: the collected ALLOW_TEAMID execution was not stored")
	}
	for sha, want := range map[string]bool{sha1: true, sha2: true, sha3: false, collected: false} {
		if got := len(unknownMarker(t, machineA, sha)) > 0; got != want {
			t.Errorf("binary %s…: unknown marker %v, want %v", sha[:4], got, want)
		}
	}
	m := unknownMarker(t, machineA, sha1)
	if day, _ := sensorString(t, m, "LastDay"); day != before && day != after {
		t.Errorf("LastDay %q, want today (%s)", day, after)
	}
	expiresAround(t, m, 35, "unknown marker")
	for attr, want := range map[string]string{"FileName": "tool", "TeamID": otherTeam, "SigningID": otherTeam + ":com.example.tool"} {
		if got, _ := sensorString(t, m, attr); got != want {
			t.Errorf("marker %s = %q, want %q", attr, got, want)
		}
	}
	for _, key := range []string{"TS", "Fleet", "Decision"} {
		if _, ok := m[key]; ok {
			t.Errorf("the unknown marker has %s, an index key of the events table", key)
		}
	}
	// The other Mac's partition is untouched.
	if len(unknownMarker(t, machineB, sha1)) != 0 {
		t.Error("a marker was written under another Mac")
	}
}

// A failed marker write answers 5xx; Santa keeps the events and uploads them again; the retry writes the marker,
// although the events themselves are already stored.
func TestUnknownMarker_FailureAnswers500AndRetryRepairs(t *testing.T) {
	transport := eventsServer(t)
	var statuses []int
	var mu sync.Mutex
	recording := func(req events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, error) {
		resp, err := transport(req)
		if req.Resource == "/eventupload/{machine_id}" && resp != nil {
			mu.Lock()
			statuses = append(statuses, resp.StatusCode)
			mu.Unlock()
		}
		return resp, err
	}
	host := syncedHost(t, machineA, recording)
	host.SantaExactEventUpload = true
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	host.PendingEvents = []string{execEvent(sha1, "BLOCK_UNKNOWN", 1, at, "")}

	var failing atomic.Bool
	failing.Store(true)
	var seen atomic.Int32
	ddblocal.AddHook(t, ddblocal.Hook{Before: func(c *ddblocal.Call) *ddblocal.Fault {
		if c.Op != "PutItem" || !strings.Contains(fmt.Sprint(c.Input["Item"]), "UnknownSeen#") {
			return nil
		}
		seen.Add(1)
		if failing.Load() {
			// Not retried by the SDK, like a missing IAM grant.
			f := ddblocal.FaultAccessDenied
			return &f
		}
		return nil
	}})

	host2 := host
	if _, err := host2.Sync(t); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if seen.Load() == 0 {
		t.Fatal("no unknown marker write was attempted")
	}
	mu.Lock()
	first := append([]int(nil), statuses...)
	mu.Unlock()
	if len(first) == 0 || first[len(first)-1] < 500 {
		t.Errorf("eventupload answered %v with the marker write failing, want a 5xx", first)
	}
	if len(host2.PendingEvents) != 1 {
		t.Fatalf("%d events pending after the failed upload, want 1 (Santa keeps them)", len(host2.PendingEvents))
	}
	if len(eventsOf(t, machineA)) != 1 {
		t.Fatalf("precondition: the event itself should already be stored")
	}

	failing.Store(false)
	host2.MustSync(t)
	if len(host2.PendingEvents) != 0 {
		t.Errorf("%d events still pending after the retry", len(host2.PendingEvents))
	}
	if len(unknownMarker(t, machineA, sha1)) == 0 {
		t.Error("Santa's retry did not write the unknown marker for the already stored event")
	}
	if got := len(eventsOf(t, machineA)); got != 1 {
		t.Errorf("%d events stored after the retry, want 1", got)
	}
}

// Each marker is one PutItem of one write unit, display fields at their limits included: no index entry, under 1 KB.
func TestCOST_UnknownMarkersAddOneWritePerBinary(t *testing.T) {
	transport := eventsServer(t)
	host := syncedHost(t, machineA, transport)
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	const binaries = 20
	name := strings.Repeat("n", 255)
	signing := otherTeam + ":" + strings.Repeat("s", 255-len(otherTeam)-1)
	for i := 0; i < binaries; i++ {
		sha := fmt.Sprintf("%064x", 0xc057+i)
		extra := fmt.Sprintf(`"team_id":%q,"signing_id":%q`, otherTeam, signing)
		e := strings.Replace(execEvent(sha, "BLOCK_UNKNOWN", 10+i, at, extra), `"file_name":"tool"`, fmt.Sprintf(`"file_name":%q`, name), 1)
		host.PendingEvents = append(host.PendingEvents, e)
	}

	// The same sync without events, for the PutItem units every sync spends on the store table.
	ddblocal.ResetCounts()
	pending := host.PendingEvents
	host.PendingEvents = nil
	host.MustSync(t)
	base := ddblocal.ConsumedCapacity().ByOp["PutItem"]
	host.PendingEvents = pending

	var markerPuts atomic.Int32
	ddblocal.AddHook(t, ddblocal.Hook{Before: func(c *ddblocal.Call) *ddblocal.Fault {
		if c.Op == "PutItem" && strings.Contains(fmt.Sprint(c.Input["Item"]), "UnknownSeen#") {
			markerPuts.Add(1)
		}
		return nil
	}})
	ddblocal.ResetCounts()
	host.MustSync(t)
	if n := markerPuts.Load(); n != binaries {
		t.Errorf("%d unknown marker PutItem calls, want %d (one per binary)", n, binaries)
	}
	// Each binary's two markers (BinarySeen# and UnknownSeen#) are one PutItem and one write unit each.
	if got, want := ddblocal.ConsumedCapacity().ByOp["PutItem"]-base, float64(2*binaries); got != want {
		t.Errorf("the upload's PutItem calls cost %.1f write units beyond a sync without events, want %.0f (two one-unit markers per binary): %s", got, want, ddblocal.ConsumedCapacity())
	}
	for i := 0; i < binaries; i++ {
		m := unknownMarker(t, machineA, fmt.Sprintf("%064x", 0xc057+i))
		if size := dynamoItemSize(m); size > 1024 {
			t.Errorf("marker %d is %d bytes; above 1 KB it costs more than one write unit", i, size)
		}
		if got, _ := sensorString(t, m, "FileName"); got != name {
			t.Errorf("marker %d FileName has %d bytes, want the 255-byte name", i, len(got))
		}
	}
}

// dynamoItemSize is DynamoDB's size of an item: attribute names plus values (strings by their UTF-8 bytes, numbers about
// one byte per two digits plus one, lists and maps 3 bytes plus 1 per element).
func dynamoItemSize(item map[string]awstypes.AttributeValue) int {
	size := 0
	for name, v := range item {
		size += len(name) + attrSize(v)
	}
	return size
}

func attrSize(v awstypes.AttributeValue) int {
	switch v := v.(type) {
	case *awstypes.AttributeValueMemberS:
		return len(v.Value)
	case *awstypes.AttributeValueMemberN:
		digits := len(strings.TrimLeft(strings.NewReplacer("-", "", ".", "").Replace(v.Value), "0"))
		return (digits+1)/2 + 1
	case *awstypes.AttributeValueMemberB:
		return len(v.Value)
	case *awstypes.AttributeValueMemberBOOL, *awstypes.AttributeValueMemberNULL:
		return 1
	case *awstypes.AttributeValueMemberSS:
		n := 0
		for _, s := range v.Value {
			n += len(s)
		}
		return n
	case *awstypes.AttributeValueMemberL:
		n := 3
		for _, e := range v.Value {
			n += 1 + attrSize(e)
		}
		return n
	case *awstypes.AttributeValueMemberM:
		n := 3
		for k, e := range v.Value {
			n += 1 + len(k) + attrSize(e)
		}
		return n
	}
	return 0
}

// ---------------------------------------------------------------------------------------------------------------
// FirstSeenDay on the sync state
// ---------------------------------------------------------------------------------------------------------------

func firstSeenDayOf(t *testing.T, machineID string) (string, bool) {
	t.Helper()
	return sensorString(t, getItem(t, "Machine#"+machineID, "SyncState"), "FirstSeenDay")
}

func TestSyncState_FirstSeenDayStoredAndCarried(t *testing.T) {
	transport := freshServer(t)
	host := newHost(t, machineA, transport)
	before := time.Now().UTC().Format("2006-01-02")
	host.MustSync(t)
	after := time.Now().UTC().Format("2006-01-02")
	day, ok := firstSeenDayOf(t, machineA)
	if !ok || (day != before && day != after) {
		t.Fatalf("FirstSeenDay after the first sync %q (present %v), want today (%s)", day, ok, after)
	}

	// Carried, never moved, by every later sync.
	updateItem(t, "Machine#"+machineA, "SyncState", map[string]any{"FirstSeenDay": "2026-01-05"})
	host.MustSync(t)
	host.MustSync(t)
	if day, _ := firstSeenDayOf(t, machineA); day != "2026-01-05" {
		t.Errorf("FirstSeenDay %q after two more syncs, want 2026-01-05 kept", day)
	}

	// A row written before FirstSeenDay existed gets today.
	item := getItem(t, "Machine#"+machineA, "SyncState")
	delete(item, "FirstSeenDay")
	putRaw(t, item)
	host.MustSync(t)
	if day, _ := firstSeenDayOf(t, machineA); day != before && day != after {
		t.Errorf("FirstSeenDay %q after a sync from a row without one, want today", day)
	}
}

// A sync state as full as a normal Mac makes it (a missed postflight, a served last page, reported groups, a reported
// rules hash, two completed syncs) stays under 1 KB with FirstSeenDay, so preflight's write is one unit.
func TestSyncState_RowStaysUnderOneKB(t *testing.T) {
	transport := freshServer(t)
	addGlobalRulesFast(t, 100, "")
	failPostflight := atomic.Bool{}
	wrapped := func(req events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, error) {
		if req.Resource == "/postflight/{machine_id}" && failPostflight.Load() {
			return &events.APIGatewayProxyResponse{StatusCode: 500, Body: `{"error":"injected"}`}, nil
		}
		return transport(req)
	}
	host := newHost(t, machineA, wrapped)
	host.IgnorePostflightFailure = true
	host.RulesHash = strings.Repeat("f", 64)
	host.PrimaryUserGroups = []string{"staff", "engineering", "everyone"}
	host.MustSync(t)
	host.MustSync(t)
	host.MustSync(t)
	failPostflight.Store(true)
	host.MustSync(t)
	failPostflight.Store(false)
	host.MustSync(t)
	failPostflight.Store(true)
	host.MustSync(t)

	row := getItem(t, "Machine#"+machineA, "SyncState")
	if _, ok := row["FirstSeenDay"]; !ok {
		t.Error("the sync state has no FirstSeenDay")
	}
	if size := dynamoItemSize(row); size >= 1024 {
		t.Errorf("the sync state is %d bytes; at 1 KB or more every sync's write costs 2 units (keep FirstSeenDay on the Groups item instead)", size)
	}
}

// COST2's steady-state budget (4 write units, 8 read units) holds with FirstSeenDay on the row.
func TestCOST2_SteadyStateBudgetWithFirstSeenDay(t *testing.T) {
	host := costFixture(t)
	host.PrimaryUserGroups = []string{"staff", "engineering"}
	host.RulesHash = strings.Repeat("e", 64)
	host.MustSync(t)
	host.MustSync(t)
	ddblocal.ResetCounts()
	host.MustSync(t)
	c := ddblocal.ConsumedCapacity()
	if c.WriteUnits > 4 || c.ReadUnits > 8 {
		t.Errorf("steady-state sync consumed %s; budget 4 WCU and 8 RCU", c)
	}
	if _, ok := firstSeenDayOf(t, machineA); !ok {
		t.Error("the steady-state sync's row has no FirstSeenDay, so the budget was not measured with it")
	}
}

func putRaw(t *testing.T, item map[string]awstypes.AttributeValue) {
	t.Helper()
	if _, err := ddblocal.Raw(t).PutItem(context.Background(), &awsdynamodb.PutItemInput{TableName: aws.String(ddblocal.TableName), Item: item}); err != nil {
		t.Fatal(err)
	}
}
