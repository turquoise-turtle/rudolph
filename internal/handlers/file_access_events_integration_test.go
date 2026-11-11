//go:build integration

package handlers

// File access events (Santa's File Access Authorisation, FAA) are stored in the events table, next to the execution
// events, for the web view (owner, 2026-11-11: "file access events sounds alright"). Santa uploads them to any
// configured sync server: blocks at once, audit-only matches with the next upload (Santa 2026.8 Santad.mm and
// SNTSyncEventUpload.mm). With sync v1 an event carries rule_version, rule_name, target (the path accessed),
// access_time, decision (FILE_ACCESS_DECISION_DENIED, _DENIED_INVALID_SIGNATURE or _AUDIT_ONLY) and process_chain
// (the process that accessed the path first, then each parent).
//
//   - Item: PK "FileAccess#<machine ID>", SK "<TS>#<pid>#<16 hex digits>", where TS is access_time in microseconds
//     (rounded), zero-padded to 20 digits as for executions, pid is the first process's (0 when there is none), and
//     the hex digits are the start of the SHA-256 of the rule name, the target and the decision, so that two accesses
//     by one process at the same instant are two items and the same event uploaded again is the same item.
//   - Attributes: MachineID, TS, Fleet ("FileAccess"), Decision (Santa's name), RuleName, RuleVersion, Target, and of
//     the first process ProcessPath (its file_path), ProcessName (the last element of ProcessPath), ProcessSHA256,
//     ProcessSigningID, ProcessTeamID, ProcessCDHash and PID (a number), ParentPath (the second process's file_path),
//     and ProcessChain (a list of every process's file_path, first to last, at most 8). Empty ones are left out.
//     ExpiresAfter is the upload time + 90 days. The process's team ID is ProcessTeamID, not TeamID, so these items
//     stay out of the TeamID_TS index of executions; Fleet and Decision put them in Fleet_TS and Decision_TS under
//     values no execution has.
//   - Written only if it does not exist yet. No day counter or machine marker is touched, and nothing is written to
//     the Exec# partitions.
//   - Dropped, logged with "dropped" and the count (never the event), the rest of the upload stored and the upload
//     answered 200: no access_time or an access_time outside the window kept for executions (more than a day ahead,
//     more than 90 days ago), no target, or a decision that is missing or FILE_ACCESS_DECISION_UNKNOWN.
//   - Strings are cut to 4,096 bytes (Decision to 1,024) on a UTF-8 boundary, as for executions. File access events
//     share the limit of 1,000 stored events an upload with executions.
//   - A failed write answers 5xx, so Santa keeps the events and uploads them again; they are then stored once.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	faaRule   = "Protect Jamf"
	faaTarget = "/usr/local/jamf/bin/jamf"
)

// faaEvent is one file access event as Santa 2026.8 uploads it (protojson), at `at` (millisecond precision).
func faaEvent(decision string, at time.Time, target string, chain string) string {
	return fmt.Sprintf(`{"rule_version":"v7","rule_name":%q,"target":%q,"access_time":%.3f,"decision":%q,"process_chain":[%s]}`,
		faaRule, target, float64(at.UnixMilli())/1000, decision, chain)
}

func faaProcess(path string, pid int, extra string) string {
	p := fmt.Sprintf(`{"file_path":%q,"pid":%d`, path, pid)
	if extra != "" {
		p += "," + extra
	}
	return p + "}"
}

func faaItems(t *testing.T, machineID string) []map[string]awstypes.AttributeValue {
	t.Helper()
	return eventsQuery(t, &awsdynamodb.QueryInput{
		KeyConditionExpression:    aws.String("PK = :pk"),
		ExpressionAttributeValues: map[string]awstypes.AttributeValue{":pk": &awstypes.AttributeValueMemberS{Value: "FileAccess#" + machineID}},
		ConsistentRead:            aws.Bool(true),
	})
}

func scanEvents(t *testing.T) []map[string]awstypes.AttributeValue {
	t.Helper()
	out, err := ddblocal.Raw(t).Scan(context.Background(), &awsdynamodb.ScanInput{TableName: aws.String(ddblocal.EventsTableName), ConsistentRead: aws.Bool(true)})
	if err != nil {
		t.Fatal(err)
	}
	return out.Items
}

func TestFileAccess_EventsAreStored(t *testing.T) {
	transport := eventsServer(t)
	host := syncedHost(t, machineA, transport)
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	chain := faaProcess("/bin/rm", 4242, fmt.Sprintf(`"file_sha256":%q,"signing_id":"platform:com.apple.rm","cdhash":%q,"team_id":""`, sha1, cdhash)) +
		"," + faaProcess("/bin/zsh", 4000, "") + "," + faaProcess("/usr/bin/sudo", 3900, "") + "," + faaProcess("/usr/libexec/sshd-session", 3800, "")
	host.PendingFileAccessEvents = []string{
		faaEvent("FILE_ACCESS_DECISION_DENIED", at, faaTarget, chain),
		faaEvent("FILE_ACCESS_DECISION_AUDIT_ONLY", at.Add(time.Second), "/Library/Application Support/JAMF/Receipts/x.pkg",
			faaProcess("/Applications/Tool.app/Contents/MacOS/Tool", 77, fmt.Sprintf(`"team_id":%q,"signing_id":%q`, teamGoogle, chromeSigningID))),
		faaEvent("FILE_ACCESS_DECISION_DENIED_INVALID_SIGNATURE", at.Add(2*time.Second), faaTarget, ""),
	}
	host.MustSync(t)

	items := faaItems(t, machineA)
	if len(items) != 3 {
		t.Fatalf("%d file access items for the Mac, want 3: %v", len(items), items)
	}
	byDecision := map[string]map[string]awstypes.AttributeValue{}
	for _, item := range items {
		d, _ := sensorString(t, item, "Decision")
		byDecision[d] = item
	}

	denied := byDecision["FILE_ACCESS_DECISION_DENIED"]
	if denied == nil {
		t.Fatalf("no denied item: %v", items)
	}
	sk, _ := sensorString(t, denied, "SK")
	if parts := strings.Split(sk, "#"); len(parts) != 3 || parts[0] != tsOf(at) || parts[1] != "4242" || len(parts[2]) != 16 || strings.Trim(parts[2], "0123456789abcdef") != "" {
		t.Errorf("denied SK = %q, want %s#4242#<16 lower-case hex digits>", sk, tsOf(at))
	}
	for attr, want := range map[string]string{
		"MachineID": machineA, "TS": tsOf(at), "Fleet": "FileAccess", "RuleName": faaRule, "RuleVersion": "v7",
		"Target": faaTarget, "ProcessPath": "/bin/rm", "ProcessName": "rm", "ProcessSHA256": sha1,
		"ProcessSigningID": "platform:com.apple.rm", "ProcessCDHash": cdhash, "ParentPath": "/bin/zsh",
	} {
		if got, _ := sensorString(t, denied, attr); got != want {
			t.Errorf("denied %s = %q, want %q", attr, got, want)
		}
	}
	if n, ok := numAttr(t, denied, "PID"); !ok || n != 4242 {
		t.Errorf("denied PID = %v (present %v), want 4242", n, ok)
	}
	list, ok := denied["ProcessChain"].(*awstypes.AttributeValueMemberL)
	var paths []string
	if ok {
		for _, v := range list.Value {
			if s, ok := v.(*awstypes.AttributeValueMemberS); ok {
				paths = append(paths, s.Value)
			}
		}
	}
	if strings.Join(paths, " ") != "/bin/rm /bin/zsh /usr/bin/sudo /usr/libexec/sshd-session" {
		t.Errorf("denied ProcessChain = %v, want the four paths in order", paths)
	}
	for _, attr := range []string{"ProcessTeamID", "TeamID", "FileSHA256", "SigningID"} {
		if v, ok := denied[attr]; ok {
			t.Errorf("denied stores %s: %#v", attr, v)
		}
	}
	expiresAround(t, denied, 90, "file access event")

	audit := byDecision["FILE_ACCESS_DECISION_AUDIT_ONLY"]
	for attr, want := range map[string]string{"ProcessTeamID": teamGoogle, "ProcessSigningID": chromeSigningID, "ProcessName": "Tool"} {
		if got, _ := sensorString(t, audit, attr); got != want {
			t.Errorf("audit-only %s = %q, want %q", attr, got, want)
		}
	}
	if _, ok := audit["ParentPath"]; ok {
		t.Error("audit-only event with one process stores a ParentPath")
	}

	unsigned := byDecision["FILE_ACCESS_DECISION_DENIED_INVALID_SIGNATURE"]
	if sk, _ := sensorString(t, unsigned, "SK"); !strings.HasPrefix(sk, tsOf(at.Add(2*time.Second))+"#0#") {
		t.Errorf("an event without a process chain has SK %q, want pid 0", sk)
	}
	for _, attr := range []string{"ProcessPath", "ProcessName", "PID", "ProcessChain"} {
		if v, ok := unsigned[attr]; ok {
			t.Errorf("an event without a process chain stores %s: %#v", attr, v)
		}
	}

	// Nothing else: no execution items, counters or markers.
	for _, item := range scanEvents(t) {
		if pk, _ := sensorString(t, item, "PK"); !strings.HasPrefix(pk, "FileAccess#") {
			t.Errorf("a file access upload wrote %s", pk)
		}
	}
}

func TestFileAccess_IndexesHoldThemApartFromExecutions(t *testing.T) {
	transport := eventsServer(t)
	host := syncedHost(t, machineA, transport)
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	host.PendingEvents = []string{execEvent(sha1, "BLOCK_BINARY", 1, at, fmt.Sprintf(`"team_id":%q`, teamGoogle))}
	host.PendingFileAccessEvents = []string{faaEvent("FILE_ACCESS_DECISION_DENIED", at, faaTarget,
		faaProcess("/Applications/Tool.app/Contents/MacOS/Tool", 9, fmt.Sprintf(`"team_id":%q`, teamGoogle)))}
	host.MustSync(t)

	query := func(index, attr, value string) int {
		return len(eventsQuery(t, &awsdynamodb.QueryInput{
			IndexName:                 aws.String(index),
			KeyConditionExpression:    aws.String("#k = :v"),
			ExpressionAttributeNames:  map[string]string{"#k": attr},
			ExpressionAttributeValues: map[string]awstypes.AttributeValue{":v": &awstypes.AttributeValueMemberS{Value: value}},
		}))
	}
	for _, c := range []struct {
		index, attr, value string
		want               int
	}{
		{"Fleet_TS", "Fleet", "FileAccess", 1},
		{"Fleet_TS", "Fleet", "Exec", 1},
		{"Decision_TS", "Decision", "FILE_ACCESS_DECISION_DENIED", 1},
		{"TeamID_TS", "TeamID", teamGoogle, 1},
	} {
		if got := query(c.index, c.attr, c.value); got != c.want {
			t.Errorf("%s %s=%s: %d items, want %d", c.index, c.attr, c.value, got, c.want)
		}
	}
}

func TestFileAccess_SameEventTwiceIsOneItem(t *testing.T) {
	transport := eventsServer(t)
	host := syncedHost(t, machineA, transport)
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	proc := faaProcess("/bin/rm", 5, "")
	event := faaEvent("FILE_ACCESS_DECISION_DENIED", at, faaTarget, proc)
	host.PendingFileAccessEvents = []string{
		event,
		event,
		faaEvent("FILE_ACCESS_DECISION_DENIED", at, "/usr/local/jamf/bin/jamfAgent", proc), // same process and instant, another path
	}
	host.MustSync(t)
	host.PendingFileAccessEvents = []string{event}
	host.MustSync(t)
	if got := len(faaItems(t, machineA)); got != 2 {
		t.Errorf("%d items, want 2 (one per path)", got)
	}
}

func TestFileAccess_UnusableEventsAreDropped(t *testing.T) {
	logs := captureLog(t)
	transport := eventsServer(t)
	host := syncedHost(t, machineA, transport)
	host.SantaExactEventUpload = true
	now := time.Now().Truncate(time.Millisecond)
	ok := faaEvent("FILE_ACCESS_DECISION_DENIED", now.Add(-time.Minute), faaTarget, faaProcess("/bin/rm", 1, ""))
	host.PendingFileAccessEvents = []string{
		`{"rule_name":"r","target":"/x","decision":"FILE_ACCESS_DECISION_DENIED"}`, // no access_time
		faaEvent("FILE_ACCESS_DECISION_DENIED", now.Add(48*time.Hour), faaTarget, ""),
		faaEvent("FILE_ACCESS_DECISION_DENIED", now.Add(-100*24*time.Hour), faaTarget, ""),
		faaEvent("FILE_ACCESS_DECISION_DENIED", now, "", ""),
		faaEvent("FILE_ACCESS_DECISION_UNKNOWN", now, faaTarget, ""),
		fmt.Sprintf(`{"rule_name":"r","target":"/x","access_time":%.3f}`, float64(now.UnixMilli())/1000), // no decision
		ok,
	}
	host.MustSync(t)
	if got := len(faaItems(t, machineA)); got != 1 {
		t.Errorf("%d items stored, want only the usable event", got)
	}
	if len(host.PendingFileAccessEvents) != 0 {
		t.Errorf("Santa kept %d file access events: the upload did not succeed", len(host.PendingFileAccessEvents))
	}
	out := logs.String()
	if !strings.Contains(out, "dropped") || !strings.Contains(out, machineA) {
		t.Errorf("no log line about the dropped file access events:\n%s", out)
	}
	if strings.Contains(out, faaTarget) {
		t.Errorf("the log names an event's path:\n%s", out)
	}
}

func TestFileAccess_LongFieldsAreCut(t *testing.T) {
	transport := eventsServer(t)
	host := syncedHost(t, machineA, transport)
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	long := "/Users/alice/" + strings.Repeat("é", 6000)
	host.PendingFileAccessEvents = []string{faaEvent("FILE_ACCESS_DECISION_AUDIT_ONLY", at, long, faaProcess(long, 3, ""))}
	host.MustSync(t)
	items := faaItems(t, machineA)
	if len(items) != 1 {
		t.Fatalf("%d items, want 1", len(items))
	}
	for _, attr := range []string{"Target", "ProcessPath"} {
		got, _ := sensorString(t, items[0], attr)
		if len(got) == 0 || len(got) > 4096 || !utf8.ValidString(got) || !strings.HasPrefix(long, got) {
			t.Errorf("%s: %d bytes, valid UTF-8 %v; want a prefix of the path of at most 4,096 bytes", attr, len(got), utf8.ValidString(got))
		}
	}
}

func TestFileAccess_AFailedWriteIsRetriedBySanta(t *testing.T) {
	transport := eventsServer(t)
	host := syncedHost(t, machineA, transport)
	host.SantaExactEventUpload = true
	failing := true
	ddblocal.AddHook(t, ddblocal.Hook{Before: func(c *ddblocal.Call) *ddblocal.Fault {
		if failing && c.Op != "Query" && c.Op != "GetItem" && c.Op != "Scan" && strings.Contains(c.InputJSON(), "FileAccess#") {
			f := ddblocal.FaultAccessDenied
			return &f
		}
		return nil
	}})
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	host.PendingFileAccessEvents = []string{faaEvent("FILE_ACCESS_DECISION_DENIED", at, faaTarget, faaProcess("/bin/rm", 8, ""))}
	host.MustSync(t) // with SantaExactEventUpload a failed upload does not fail the sync
	if len(host.PendingFileAccessEvents) != 1 {
		t.Fatalf("after a failed write Santa kept %d events, want 1: the upload must not succeed", len(host.PendingFileAccessEvents))
	}
	failing = false
	host.MustSync(t)
	if len(host.PendingFileAccessEvents) != 0 || len(faaItems(t, machineA)) != 1 {
		t.Errorf("after the retry: %d pending, %d stored; want 0 and 1", len(host.PendingFileAccessEvents), len(faaItems(t, machineA)))
	}
}
