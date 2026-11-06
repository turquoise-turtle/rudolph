//go:build integration

package handlers

// R6 and R7 of rudolph-web's docs/data-layer-design.md ("D+"): execution events go to a second DynamoDB table
// (deployments/.../modules/store/events.tf, resource "events"; in tests ddblocal.EventsTableName, named to the Lambda
// by EVENTS_DYNAMODB_NAME), whatever event sink (Firehose, Kinesis, Lambda or none) is configured.
//
//   - Event item: PK "Exec#<machine ID>", SK "<TS>#<file_sha256>#<pid>", where TS is the execution time in
//     microseconds (rounded), zero-padded to 20 digits. Attributes MachineID, TS, Fleet ("Exec"), Decision (Santa's
//     name, e.g. "BLOCK_BINARY"), FileSHA256, FilePath, FileName, ExecutingUser, TeamID, SigningID, CDHash (empty ones
//     left out), ExpiresAfter (upload time + 90 days, Unix seconds). Written only if it does not exist yet, so a
//     retried upload stores it once.
//   - GSIs Fleet_TS (Fleet, TS), Decision_TS (Decision, TS) and TeamID_TS (TeamID, TS; sparse).
//   - Day counter: PK "BinaryDay#<UTC date of the execution>", SK "<file_sha256>", numbers Blocked and Allowed (BLOCK_*
//     count as blocked, ALLOW_UNKNOWN as allowed), FileName, SigningID and TeamID for display, ExpiresAfter (upload
//     time + 100 days). Machine marker: PK "BinarySeen#<file_sha256>", SK "<machine ID>", ExpiresAfter (upload time +
//     100 days). Only newly stored events count.
//   - Stored for now: every BLOCK_* decision and ALLOW_UNKNOWN. Allowed-by-rule decisions wait for per-rule collection
//     (R7b); ALLOW_SCOPE, ALLOW_PLATFORM, BUNDLE_BINARY, file access and audit events are not stored in v1.
//   - An event without a SHA-256 or an execution time cannot be stored: it is dropped and logged, and the upload still
//     succeeds. A failed write to the events table answers 5xx, so Santa keeps the events and retries.

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/internal/testutil/fakesanta"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const teamGoogle = "EQHXZ8M8AV"

// execEvent is one execution event as Santa 2026.8 uploads it (protojson), executed at `at` (millisecond precision).
func execEvent(sha, decision string, pid int, at time.Time, extra string) string {
	e := fmt.Sprintf(`{"file_sha256":%q,"file_path":"/Applications/Tool.app/Contents/MacOS","file_name":"tool",`+
		`"executing_user":"alice","decision":%q,"pid":%d,"execution_time":%.3f`,
		sha, decision, pid, float64(at.UnixMilli())/1000)
	if extra != "" {
		e += "," + extra
	}
	return e + "}"
}

func tsOf(at time.Time) string { return fmt.Sprintf("%020d", at.UnixMilli()*1000) }

// eventsServer is freshServer with one unrelated global rule: Rudolph clean syncs a Mac that holds no rules at all,
// and Santa uploads no events during a clean sync.
func eventsServer(t *testing.T) fakesanta.Transport {
	t.Helper()
	transport := freshServer(t)
	addGlobalRule(t, strings.Repeat("e5", 32), types.RuleTypeBinary, types.RulePolicyBlocklist)
	return transport
}

// syncedHost returns a Mac past its first (clean) sync.
func syncedHost(t *testing.T, id string, transport fakesanta.Transport) *fakesanta.Host {
	t.Helper()
	h := newHost(t, id, transport)
	for i := 0; i < 3 && h.MustSync(t).SyncType != "NORMAL"; i++ {
	}
	return h
}

func eventsQuery(t *testing.T, in *awsdynamodb.QueryInput) []map[string]awstypes.AttributeValue {
	t.Helper()
	in.TableName = aws.String(ddblocal.EventsTableName)
	out, err := ddblocal.Raw(t).Query(context.Background(), in)
	if err != nil {
		t.Fatalf("query the events table: %v", err)
	}
	return out.Items
}

func eventsOf(t *testing.T, machineID string) []map[string]awstypes.AttributeValue {
	t.Helper()
	return eventsQuery(t, &awsdynamodb.QueryInput{
		KeyConditionExpression:    aws.String("PK = :pk"),
		ExpressionAttributeValues: map[string]awstypes.AttributeValue{":pk": &awstypes.AttributeValueMemberS{Value: "Exec#" + machineID}},
		ConsistentRead:            aws.Bool(true),
	})
}

func eventsItem(t *testing.T, pk, sk string) map[string]awstypes.AttributeValue {
	t.Helper()
	out, err := ddblocal.Raw(t).GetItem(context.Background(), &awsdynamodb.GetItemInput{
		TableName: aws.String(ddblocal.EventsTableName),
		Key: map[string]awstypes.AttributeValue{
			"PK": &awstypes.AttributeValueMemberS{Value: pk},
			"SK": &awstypes.AttributeValueMemberS{Value: sk},
		},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.Item
}

func numAttr(t *testing.T, item map[string]awstypes.AttributeValue, attr string) (float64, bool) {
	t.Helper()
	v, ok := item[attr].(*awstypes.AttributeValueMemberN)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseFloat(v.Value, 64)
	if err != nil {
		t.Fatalf("%s = %q is not a number", attr, v.Value)
	}
	return n, true
}

func expiresAround(t *testing.T, item map[string]awstypes.AttributeValue, days int, what string) {
	t.Helper()
	got, ok := numAttr(t, item, "ExpiresAfter")
	want := float64(time.Now().Add(time.Duration(days) * 24 * time.Hour).Unix())
	if !ok || math.Abs(got-want) > 600 {
		t.Errorf("%s ExpiresAfter = %v (present %v), want about now + %d days (%v)", what, got, ok, days, want)
	}
}

func TestR7_BlockedAndUnknownExecutionsAreStored(t *testing.T) {
	transport := eventsServer(t)
	host := syncedHost(t, machineA, transport)
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	host.PendingEvents = []string{
		execEvent(sha1, "BLOCK_BINARY", 4242, at, fmt.Sprintf(`"team_id":%q,"signing_id":%q,"cdhash":%q`, teamGoogle, chromeSigningID, cdhash)),
		execEvent(sha2, "ALLOW_UNKNOWN", 77, at.Add(time.Second), ""),
	}
	host.MustSync(t)

	items := eventsOf(t, machineA)
	if len(items) != 2 {
		t.Fatalf("%d events stored for the Mac, want 2", len(items))
	}
	blocked := eventsItem(t, "Exec#"+machineA, tsOf(at)+"#"+sha1+"#4242")
	if len(blocked) == 0 {
		t.Fatalf("no event item at SK %s#%s#4242; stored: %v", tsOf(at), sha1, items)
	}
	for attr, want := range map[string]string{
		"MachineID": machineA, "TS": tsOf(at), "Fleet": "Exec", "Decision": "BLOCK_BINARY", "FileSHA256": sha1,
		"FilePath": "/Applications/Tool.app/Contents/MacOS", "FileName": "tool", "ExecutingUser": "alice",
		"TeamID": teamGoogle, "SigningID": chromeSigningID, "CDHash": cdhash,
	} {
		if got, _ := sensorString(t, blocked, attr); got != want {
			t.Errorf("blocked event %s = %q, want %q", attr, got, want)
		}
	}
	expiresAround(t, blocked, 90, "event")

	unknown := eventsItem(t, "Exec#"+machineA, tsOf(at.Add(time.Second))+"#"+sha2+"#77")
	if got, _ := sensorString(t, unknown, "Decision"); got != "ALLOW_UNKNOWN" {
		t.Errorf("unknown event Decision = %q", got)
	}
	// Empty attributes are left out: TeamID is an index key, and DynamoDB rejects an empty string there.
	for _, attr := range []string{"TeamID", "SigningID", "CDHash"} {
		if v, ok := unknown[attr]; ok {
			t.Errorf("unknown event stores an empty %s: %#v", attr, v)
		}
	}
}

func TestR7_OnlyBlockedAndUnknownDecisionsAreStoredForNow(t *testing.T) {
	transport := eventsServer(t)
	host := syncedHost(t, machineA, transport)
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	stored := map[string]bool{
		"BLOCK_UNKNOWN": true, "BLOCK_BINARY": true, "BLOCK_CERTIFICATE": true, "BLOCK_SCOPE": true,
		"BLOCK_TEAMID": true, "BLOCK_SIGNINGID": true, "BLOCK_CDHASH": true, "BLOCK_BINARY_MISMATCH": true,
		"ALLOW_UNKNOWN": true,
		"ALLOW_BINARY":  false, "ALLOW_CERTIFICATE": false, "ALLOW_TEAMID": false, "ALLOW_SIGNINGID": false,
		"ALLOW_CDHASH": false, "ALLOW_SCOPE": false, "ALLOW_PLATFORM": false, "BUNDLE_BINARY": false,
	}
	pid := 100
	pids := map[string]int{}
	for decision := range stored {
		pid++
		pids[decision] = pid
		host.PendingEvents = append(host.PendingEvents, execEvent(sha1, decision, pid, at, ""))
	}
	host.MustSync(t)

	for decision, want := range stored {
		item := eventsItem(t, "Exec#"+machineA, fmt.Sprintf("%s#%s#%d", tsOf(at), sha1, pids[decision]))
		if got := len(item) > 0; got != want {
			t.Errorf("%s: stored %v, want %v", decision, got, want)
		}
	}
}

func TestR7_FileAccessAndAuditEventsAreNotStored(t *testing.T) {
	transport := eventsServer(t)
	host := syncedHost(t, machineA, transport)
	host.PendingFileAccessEvents = []string{`{"rule_version":"v1","rule_name":"ssh keys","target":"/Users/alice/.ssh/id_ed25519","decision":"FILE_ACCESS_DECISION_DENIED"}`}
	host.MustSync(t)

	out, err := ddblocal.Raw(t).Scan(context.Background(), &awsdynamodb.ScanInput{TableName: aws.String(ddblocal.EventsTableName)})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 0 {
		t.Errorf("the events table holds %d items after a file access upload, want none: %v", len(out.Items), out.Items)
	}
}

func TestR7_EventsAreStoredWithoutAnyEventSink(t *testing.T) {
	t.Setenv("HANDLER", "NONE")
	t.Setenv("LAMBDA_NAME", "")
	transport := eventsServer(t)
	host := syncedHost(t, machineA, transport)
	host.PendingEvents = []string{execEvent(sha1, "BLOCK_UNKNOWN", 1, time.Now().Add(-time.Minute), "")}
	host.MustSync(t)
	if got := len(eventsOf(t, machineA)); got != 1 {
		t.Errorf("%d events stored with no sink configured, want 1", got)
	}
}

func TestR7_DayCountersAndMachineMarkers(t *testing.T) {
	transport := eventsServer(t)
	a := syncedHost(t, machineA, transport)
	b := syncedHost(t, machineB, transport)
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	day := at.UTC().Format("2006-01-02")

	a.PendingEvents = []string{
		execEvent(sha1, "BLOCK_BINARY", 1, at, fmt.Sprintf(`"team_id":%q,"signing_id":%q`, teamGoogle, chromeSigningID)),
		execEvent(sha1, "BLOCK_BINARY", 2, at.Add(time.Second), fmt.Sprintf(`"team_id":%q,"signing_id":%q`, teamGoogle, chromeSigningID)),
		execEvent(sha2, "ALLOW_UNKNOWN", 3, at, ""),
	}
	a.MustSync(t)
	b.PendingEvents = []string{execEvent(sha1, "BLOCK_BINARY", 9, at, "")}
	b.MustSync(t)

	counter := eventsItem(t, "BinaryDay#"+day, sha1)
	if n, _ := numAttr(t, counter, "Blocked"); n != 3 {
		t.Errorf("sha1 Blocked = %v on %s, want 3 (counter %v)", n, day, counter)
	}
	if n, ok := numAttr(t, counter, "Allowed"); ok && n != 0 {
		t.Errorf("sha1 Allowed = %v, want 0 or absent", n)
	}
	if got, _ := sensorString(t, counter, "FileName"); got != "tool" {
		t.Errorf("counter FileName = %q, want tool", got)
	}
	if got, _ := sensorString(t, counter, "TeamID"); got != teamGoogle {
		t.Errorf("counter TeamID = %q, want %s", got, teamGoogle)
	}
	expiresAround(t, counter, 100, "day counter")

	unknown := eventsItem(t, "BinaryDay#"+day, sha2)
	if n, _ := numAttr(t, unknown, "Allowed"); n != 1 {
		t.Errorf("sha2 Allowed = %v, want 1 (ALLOW_UNKNOWN counts as allowed)", n)
	}

	for _, m := range []string{machineA, machineB} {
		marker := eventsItem(t, "BinarySeen#"+sha1, m)
		if len(marker) == 0 {
			t.Errorf("no machine marker for %s running sha1", m)
			continue
		}
		expiresAround(t, marker, 100, "machine marker")
	}
	if len(eventsItem(t, "BinarySeen#"+sha2, machineB)) != 0 {
		t.Error("machine B has a marker for a binary it never ran")
	}
}

// Santa uploads again whatever it could not confirm. A repeated event is not stored or counted twice.
func TestR7_RepeatedUploadIsStoredAndCountedOnce(t *testing.T) {
	transport := eventsServer(t)
	host := syncedHost(t, machineA, transport)
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	events := []string{execEvent(sha1, "BLOCK_BINARY", 1, at, ""), execEvent(sha1, "BLOCK_BINARY", 2, at, "")}
	host.PendingEvents = append([]string(nil), events...)
	host.MustSync(t)
	host.PendingEvents = append([]string(nil), events...)
	host.MustSync(t)

	if got := len(eventsOf(t, machineA)); got != 2 {
		t.Errorf("%d events stored after the same upload twice, want 2", got)
	}
	if n, _ := numAttr(t, eventsItem(t, "BinaryDay#"+at.UTC().Format("2006-01-02"), sha1), "Blocked"); n != 2 {
		t.Errorf("Blocked = %v after the same upload twice, want 2", n)
	}
}

func TestR7_UnstorableEventsAreDroppedAndTheRestStored(t *testing.T) {
	transport := eventsServer(t)
	host := syncedHost(t, machineA, transport)
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	host.PendingEvents = []string{
		`{"file_path":"/tmp/x","file_name":"x","decision":"BLOCK_UNKNOWN","pid":5,"execution_time":` + fmt.Sprintf("%.3f", float64(at.UnixMilli())/1000) + `}`,
		`{"file_sha256":"` + sha2 + `","file_name":"y","decision":"BLOCK_UNKNOWN","pid":6}`,
		execEvent(sha3, "BLOCK_UNKNOWN", 7, at, ""),
	}
	res := host.MustSync(t)
	if len(host.PendingEvents) != 0 {
		t.Errorf("%d events still pending: an unstorable event must not fail the upload (%+v)", len(host.PendingEvents), res)
	}
	items := eventsOf(t, machineA)
	if len(items) != 1 {
		t.Fatalf("%d events stored, want only the complete one", len(items))
	}
	if got, _ := sensorString(t, items[0], "FileSHA256"); got != sha3 {
		t.Errorf("stored %s, want %s", got, sha3)
	}
}

// A failed write answers 5xx: Santa keeps the events and uploads them again at the next sync.
func TestR7_EventsTableFailureIsRetriedBySanta(t *testing.T) {
	transport := eventsServer(t)
	host := syncedHost(t, machineA, transport)
	host.SantaExactEventUpload = true // like Santa: a failed batch stays pending and the sync goes on
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	host.PendingEvents = []string{execEvent(sha1, "BLOCK_UNKNOWN", 1, at, "")}

	failing := true
	ddblocal.AddHook(t, ddblocal.Hook{Before: func(c *ddblocal.Call) *ddblocal.Fault {
		if !failing {
			return nil
		}
		if name, _ := c.Input["TableName"].(string); name == ddblocal.EventsTableName {
			return &ddblocal.Fault{Status: 500, Type: "InternalServerError", Message: "injected"}
		}
		// Writes to the events table may also be batched or transactional.
		if strings.Contains(fmt.Sprint(c.Input["RequestItems"]), ddblocal.EventsTableName) || strings.Contains(fmt.Sprint(c.Input["TransactItems"]), ddblocal.EventsTableName) {
			return &ddblocal.Fault{Status: 500, Type: "InternalServerError", Message: "injected"}
		}
		return nil
	}})
	res, err := host.Sync(t)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.Requests["eventupload"] == 0 || len(host.PendingEvents) != 1 {
		t.Fatalf("the upload must fail with the events table down and the Mac keep its event; pending %d, result %+v", len(host.PendingEvents), res)
	}

	failing = false
	host.MustSync(t)
	if got := len(eventsOf(t, machineA)); got != 1 || len(host.PendingEvents) != 0 {
		t.Errorf("after the table recovered: %d stored, %d pending; want 1 and 0", got, len(host.PendingEvents))
	}
}

func TestR6_IndexesFindEventsByFleetDecisionAndTeam(t *testing.T) {
	transport := eventsServer(t)
	a := syncedHost(t, machineA, transport)
	b := syncedHost(t, machineB, transport)
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	a.PendingEvents = []string{
		execEvent(sha1, "BLOCK_BINARY", 1, at, fmt.Sprintf(`"team_id":%q`, teamGoogle)),
		execEvent(sha2, "ALLOW_UNKNOWN", 2, at.Add(2*time.Second), ""),
	}
	a.MustSync(t)
	b.PendingEvents = []string{execEvent(sha3, "BLOCK_TEAMID", 3, at.Add(time.Second), fmt.Sprintf(`"team_id":%q`, teamGoogle))}
	b.MustSync(t)

	newestFirst := func(index, cond string, values map[string]awstypes.AttributeValue) string {
		items := eventsQuery(t, &awsdynamodb.QueryInput{
			IndexName: aws.String(index), KeyConditionExpression: aws.String(cond),
			ExpressionAttributeValues: values, ScanIndexForward: aws.Bool(false),
		})
		var shas []string
		for _, it := range items {
			s, _ := sensorString(t, it, "FileSHA256")
			shas = append(shas, s[:2])
		}
		return strings.Join(shas, ",")
	}
	s := func(v string) awstypes.AttributeValue { return &awstypes.AttributeValueMemberS{Value: v} }

	if got := newestFirst("Fleet_TS", "Fleet = :f", map[string]awstypes.AttributeValue{":f": s("Exec")}); got != "b2,c3,a1" {
		t.Errorf("fleet, newest first: %s, want b2,c3,a1", got)
	}
	if got := newestFirst("Decision_TS", "Decision = :d", map[string]awstypes.AttributeValue{":d": s("BLOCK_BINARY")}); got != "a1" {
		t.Errorf("BLOCK_BINARY: %s, want a1", got)
	}
	if got := newestFirst("TeamID_TS", "TeamID = :t", map[string]awstypes.AttributeValue{":t": s(teamGoogle)}); got != "c3,a1" {
		t.Errorf("team %s: %s, want c3,a1", teamGoogle, got)
	}
	// A time window is a range on TS.
	window := map[string]awstypes.AttributeValue{":f": s("Exec"), ":from": s(tsOf(at.Add(time.Second))), ":to": s(tsOf(at.Add(time.Hour)))}
	if got := newestFirst("Fleet_TS", "Fleet = :f AND TS BETWEEN :from AND :to", window); got != "b2,c3" {
		t.Errorf("fleet in a window: %s, want b2,c3", got)
	}
}
