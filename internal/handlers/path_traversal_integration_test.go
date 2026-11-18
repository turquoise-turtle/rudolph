//go:build integration

package handlers

// Path traversal review: values a Mac sends must not end up as keys outside the partitions and index values its
// upload is for. Every Mac holds the same sync token, so one Mac's upload reaching a fleet-wide key or index
// partition affects every admin's view of the fleet.

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/ranker"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// topRules returns the rows of the current Top#<kind>#<window> snapshot as SHA-256 to GlobalRule.
func topRules(t *testing.T, kind string, window int) map[string]string {
	t.Helper()
	pointer := eventsItem(t, fmt.Sprintf("Top#%s#%d", kind, window), "Current")
	snapshot, ok := sensorString(t, pointer, "Snapshot")
	if !ok {
		t.Fatalf("no pointer for %s %d", kind, window)
	}
	rows := map[string]string{}
	for _, item := range eventsQuery(t, &awsdynamodb.QueryInput{
		KeyConditionExpression:    aws.String("PK = :pk"),
		ExpressionAttributeValues: map[string]awstypes.AttributeValue{":pk": &awstypes.AttributeValueMemberS{Value: fmt.Sprintf("TopSnapshot#%s#%d#%s", kind, window, snapshot)}},
		ConsistentRead:            aws.Bool(true),
	}) {
		sha, _ := sensorString(t, item, "FileSHA256")
		rule, _ := sensorString(t, item, "GlobalRule")
		rows[sha] = rule
	}
	return rows
}

// Review finding R1, end to end: the event writer copies an execution's signing ID and team ID onto the fleet's day
// counter, and the ranker builds global rule keys from them. A Mac uploading BLOCK_UNKNOWN executions whose
// identifier is too long for a sort key must not stop the ranker for everyone (see
// internal/ranker/ranker_rule_key_limit_integration_test.go).
func TestPathTraversal_R1_LongIdentifierUploadDoesNotStopTheRanker(t *testing.T) {
	for _, c := range []struct {
		name, extra string
	}{
		{"signing ID", fmt.Sprintf(`"signing_id":%q,"team_id":"TEAMLONG"`, "TEAMLONG:"+strings.Repeat("s", 1015))},
		{"team ID", fmt.Sprintf(`"team_id":%q`, strings.Repeat("T", 1018))},
	} {
		t.Run(c.name, func(t *testing.T) {
			transport := eventsServer(t)
			addGlobalRule(t, sha2, types.RuleTypeBinary, types.RulePolicyBlocklist)
			at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)

			attacker := syncedHost(t, machineA, transport)
			attacker.PendingEvents = []string{execEvent(sha1, "BLOCK_UNKNOWN", 1, at, c.extra)}
			attacker.MustSync(t)
			// Another Mac's unknown binary that a global rule now covers: the run must still mark it.
			other := syncedHost(t, machineB, transport)
			other.PendingEvents = []string{execEvent(sha2, "BLOCK_UNKNOWN", 2, at, `"team_id":"TEAMOK"`)}
			other.MustSync(t)
			if len(eventsItem(t, "BinaryDay#"+at.UTC().Format("2006-01-02"), sha1)) == 0 {
				t.Fatal("fixture: the long-identifier execution was not counted")
			}

			err := ranker.Run(context.Background(), ranker.Options{Client: ddblocal.Raw(t), Table: ddblocal.EventsTableName, StoreTable: ddblocal.TableName})
			if err != nil {
				t.Fatalf("one Mac's upload stopped the ranker run: %v", err)
			}
			rows := topRules(t, "Unknown", 7)
			if rule, ok := rows[sha1]; !ok || rule != "" {
				t.Errorf("Unknown 7 days: the long-identifier binary present %v with GlobalRule %q, want present without one", ok, rule)
			}
			if rows[sha2] != "Binary#"+sha2 {
				t.Errorf("Unknown 7 days: the other binary has GlobalRule %q, want Binary#%s", rows[sha2], sha2)
			}
		})
	}
}

// rawUpload posts the body as given: fakesanta refuses decisions outside Santa's enums, as Santa's protojson would,
// but nothing stops a client from sending them.
func rawUpload(t *testing.T, machineID string, execs []string, fileAccess []string) *events.APIGatewayProxyResponse {
	t.Helper()
	body := fmt.Sprintf(`{"events":[%s],"file_access_events":[%s]}`, strings.Join(execs, ","), strings.Join(fileAccess, ","))
	resp, err := ApiRouter(syncRequest("eventupload", machineID, body))
	if err != nil {
		t.Fatalf("eventupload: %v", err)
	}
	return resp
}

// Review finding R2: a file access event's decision is the Decision_TS index's partition key, and Santa only ever
// sends FILE_ACCESS_DECISION_* values there (sync_v1.proto's FileAccessDecision). A client that sends an execution
// decision instead would put a FileAccess#<machine> item into the fleet's BLOCK_BINARY (or ALLOW_UNKNOWN) partition
// of Decision_TS, which lists executions, so it must be dropped like the other file access events the table can never
// store; the rest of the upload is still stored.
func TestPathTraversal_R2_FileAccessEventsWithAnExecutionDecisionAreDropped(t *testing.T) {
	logs := captureLog(t)
	transport := eventsServer(t)
	syncedHost(t, machineA, transport)
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)

	foreign := []string{"BLOCK_BINARY", "BLOCK_UNKNOWN", "ALLOW_UNKNOWN", "ALLOW_BINARY", "Exec", "block_binary"}
	var fileAccess []string
	for i, decision := range foreign {
		fileAccess = append(fileAccess, faaEvent(decision, at, fmt.Sprintf("/forged/%d", i), faaProcess("/bin/rm", 10+i, "")))
	}
	genuine := []string{"FILE_ACCESS_DECISION_DENIED", "FILE_ACCESS_DECISION_DENIED_INVALID_SIGNATURE", "FILE_ACCESS_DECISION_AUDIT_ONLY"}
	for i, decision := range genuine {
		fileAccess = append(fileAccess, faaEvent(decision, at, fmt.Sprintf("/real/%d", i), faaProcess("/bin/rm", 20+i, "")))
	}
	resp := rawUpload(t, machineA, []string{execEvent(sha1, "BLOCK_BINARY", 1, at, "")}, fileAccess)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("eventupload answered %d: %s", resp.StatusCode, resp.Body)
	}

	var stored []string
	for _, item := range faaItems(t, machineA) {
		d, _ := sensorString(t, item, "Decision")
		stored = append(stored, d)
	}
	if len(stored) != len(genuine) {
		t.Errorf("file access items stored with decisions %v, want only the %d real ones %v", stored, len(genuine), genuine)
	}
	for _, decision := range append(foreign, genuine...) {
		for _, item := range eventsQuery(t, &awsdynamodb.QueryInput{
			IndexName:                 aws.String("Decision_TS"),
			KeyConditionExpression:    aws.String("#d = :d"),
			ExpressionAttributeNames:  map[string]string{"#d": "Decision"},
			ExpressionAttributeValues: map[string]awstypes.AttributeValue{":d": &awstypes.AttributeValueMemberS{Value: decision}},
		}) {
			pk, _ := sensorString(t, item, "PK")
			isFileAccess := strings.HasPrefix(pk, "FileAccess#")
			if isFileAccess != strings.HasPrefix(decision, "FILE_ACCESS_DECISION_") {
				t.Errorf("Decision_TS %s holds %s: only FILE_ACCESS_DECISION_ values may list file access events, and only they", decision, pk)
			}
		}
	}
	if len(eventsOf(t, machineA)) != 1 {
		t.Errorf("%d execution items, want the one execution of the upload", len(eventsOf(t, machineA)))
	}
	out := logs.String()
	if !strings.Contains(out, fmt.Sprintf("dropped %d event(s) from machine %s", len(foreign), machineA)) {
		t.Errorf("no log line about the %d dropped file access events:\n%s", len(foreign), out)
	}
	if strings.Contains(out, "/forged/") {
		t.Errorf("the log names an event's path:\n%s", out)
	}
}
