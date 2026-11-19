//go:build integration

package handlers

// Key confinement: nothing a Santa client sends (the machine ID in the path, the ruledownload cursor) may address
// a DynamoDB partition other than the ones its request is for. Every client holds the same fleet-wide sync token,
// so the keys are the only thing keeping one Mac out of another Mac's rows.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-lambda-go/events"
)

const confinementPreflightBody = `{"serial_num":"C02ATTACKER","hostname":"attacker","os_version":"15.0","os_build":"x","santa_version":"2026.8","primary_user":"u","client_mode":"MONITOR"}`

func syncRequest(stage, machineID, body string) events.APIGatewayProxyRequest {
	return events.APIGatewayProxyRequest{
		HTTPMethod:     http.MethodPost,
		Resource:       "/" + stage + "/{machine_id}",
		Path:           "/" + stage + "/" + machineID,
		PathParameters: map[string]string{"machine_id": machineID},
		Headers:        map[string]string{"Content-Type": "application/json"},
		Body:           body,
	}
}

// ruledownloadWithCursor sends a ruledownload page request carrying a cursor the client wrote itself.
func ruledownloadWithCursor(t *testing.T, machineID string, cursor map[string]any) *events.APIGatewayProxyResponse {
	t.Helper()
	rawCursor, err := json.Marshal(cursor)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]string{"cursor": string(rawCursor)})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := ApiRouter(syncRequest("ruledownload", machineID, string(body)))
	if err != nil {
		t.Fatalf("ruledownload: %v", err)
	}
	return resp
}

// The ruledownload cursor is handed to the client and comes back verbatim, so a client can write its own. Its pk
// and sk become the query's ExclusiveStartKey. A cursor naming another partition must never make the page come
// from that partition: machine A must not be able to list machine B's rules, sensor data or sync state by paging
// "after" a key in B's partitions.
func TestForgedRuledownloadCursorCannotReadAnotherPartition(t *testing.T) {
	transport := freshServer(t)

	globalSHA := sha1
	victimRuleSHA := sha2
	victimTeamID := "VICTIMTEAM"
	const victimSerial = "C02VICTIMSERIAL"
	const victimHostname = "victim-hostname.example"

	addGlobalRule(t, globalSHA, types.RuleTypeBinary, types.RulePolicyBlocklist)
	addMachineRule(t, machineB, victimRuleSHA, types.RuleTypeBinary, types.RulePolicyAllowlist, time.Now().Add(24*time.Hour))
	addMachineRule(t, machineB, victimTeamID, types.RuleTypeTeamID, types.RulePolicyAllowlist, time.Now().Add(24*time.Hour))

	victim := newHost(t, machineB, transport)
	victim.SerialNumber = victimSerial
	victim.Hostname = victimHostname
	victim.MustSync(t)

	attacker := newHost(t, machineA, transport)
	attacker.MustSync(t)

	// The fixtures the forged cursors aim at must exist, or the test would pass against an empty partition.
	if len(getItem(t, "MachineRules#"+machineB, "Binary#"+victimRuleSHA)) == 0 {
		t.Fatal("fixture: machine B has no machine rule row")
	}
	if len(getItem(t, "Machine#"+machineB, "Current")) == 0 {
		t.Fatal("fixture: machine B has no sensor data row")
	}

	// Every cursor carries the attacker's current sync number, as the cursors the server issues do: a last page of a
	// numbered sync without it is refused 409 before the key would ever matter.
	seq := numberAttr(t, getItem(t, "Machine#"+machineA, "SyncState"), "SyncSeq")

	// A cursor of the shape the server issues is honoured: a client-written cursor inside the partition the
	// strategy reads does page through it. Without this, the forged cursors below could be "safe" only because
	// client cursors were ignored altogether.
	legit := ruledownloadWithCursor(t, machineA, map[string]any{
		"strategy": 1, "pk": "GlobalRules", "sk": "Binary#", "page": 2, "batch_size": 50, "seq": seq,
	})
	if legit.StatusCode != http.StatusOK || !strings.Contains(legit.Body, globalSHA) {
		t.Fatalf("a cursor inside GlobalRules should page the global rules, got %d %s", legit.StatusCode, legit.Body)
	}

	secrets := []string{victimRuleSHA, victimTeamID, victimSerial, victimHostname, machineB}

	type forged struct {
		pk, sk string
	}
	targets := []forged{
		// Sort keys that sort before every row of the partition, so a query that started there would return them all
		{"MachineRules#" + machineB, "Binary#"},
		{"MachineRules#" + machineB, "A"},
		{"Machine#" + machineB, "A"},
		{"MachineRuleDelivery#" + machineB, "A"},
		{"GlobalConfig", "A"},
		{"GroupRegistry", "A"},
		{"EventCollection", "A"},
	}
	for _, strategy := range []int{1, 2} {
		for _, target := range targets {
			name := fmt.Sprintf("strategy=%d/pk=%s/sk=%s", strategy, target.pk, target.sk)
			t.Run(name, func(t *testing.T) {
				resp := ruledownloadWithCursor(t, machineA, map[string]any{
					"strategy": strategy, "pk": target.pk, "sk": target.sk, "page": 2, "batch_size": 50, "seq": seq,
				})
				for _, secret := range secrets {
					if strings.Contains(resp.Body, secret) {
						t.Errorf("a forged cursor returned data of another partition (%q) to machine A: %d %s", secret, resp.StatusCode, resp.Body)
					}
				}
				// The next cursor is given back to the client; it must not carry the foreign partition on either.
				if strings.Contains(resp.Body, target.pk) {
					t.Errorf("the response repeats the forged partition key, e.g. in a next cursor: %s", resp.Body)
				}
				// Rudolph never issues a cursor outside the partition its strategy reads, so such a cursor is refused
				// before any query rather than answered from a page of some other partition.
				if resp.StatusCode != http.StatusBadRequest {
					t.Errorf("a forged cursor naming %s was answered %d, want 400: %s", target.pk, resp.StatusCode, resp.Body)
				}
			})
		}
	}

	// The forged requests are machine A's: none of them may have changed what machine B is sent.
	victim.MustSync(t)
	if got := victim.Policy("BINARY", victimRuleSHA); got != "ALLOWLIST" {
		t.Errorf("machine B's own rule after machine A's forged cursors: got %q, want ALLOWLIST", got)
	}
}

// The machine ID is the only part of a request that picks the partitions a sync reads and writes
// ("Machine#<id>", "MachineRules#<id>", "Exec#<id>", ...). Anything but a canonical UUID is refused by every stage
// before DynamoDB is called at all: an ID such as "<uuid of B>#x" or "../GlobalConfig" must not reach a key, and
// the other spellings of a UUID must not create a second set of rows for the same Mac.
func TestMachineIDsThatAreNotCanonicalUUIDsNeverReachDynamoDB(t *testing.T) {
	freshServer(t)

	stageBodies := map[string]string{
		"preflight":    confinementPreflightBody,
		"eventupload":  fmt.Sprintf(`{"events":[{"file_sha256":%q,"file_name":"x","execution_time":%d,"decision":"BLOCK_BINARY","pid":1}]}`, sha1, time.Now().Unix()),
		"ruledownload": `{}`,
		"postflight":   `{"rules_received":0,"rules_processed":0}`,
		"xsrf":         ``,
	}

	// Handlers boot on their first request, which may itself call DynamoDB; do that with a valid ID first so the
	// counters below only see what a refused request does. It also shows that the counters do see a sync's calls.
	for _, stage := range []string{"preflight", "eventupload", "ruledownload", "postflight", "xsrf"} {
		resp, err := ApiRouter(syncRequest(stage, machineB, stageBodies[stage]))
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("%s with a valid machine ID: %v %+v", stage, err, resp)
		}
	}
	if len(ddblocal.Counts()) == 0 {
		t.Fatal("the operation counters saw no DynamoDB call of a valid sync; the test cannot tell a refused request apart")
	}

	lower := strings.ToLower(machineB)
	ids := map[string]string{
		"other machine with a key suffix": machineB + "#SyncState",
		"other machine as a key prefix":   machineB + "#" + machineA,
		"prefixed partition":              "Machine#" + machineB,
		"global partition":                "GlobalRules",
		"parent directory":                "../GlobalConfig",
		"parent directory then a UUID":    "../" + machineB,
		"UUID then a path":                machineB + "/../" + machineA,
		"encoded slash":                   machineB + "%2F..",
		"braces":                          "{" + machineB + "}",
		"URN":                             "urn:uuid:" + lower,
		"no dashes":                       strings.ReplaceAll(machineB, "-", ""),
		"trailing newline":                machineB + "\n",
		"leading space":                   " " + machineB,
		"trailing NUL":                    machineB + "\x00",
		"wildcard":                        "*",
		"fullwidth digit":                 "１" + machineB[1:],
		"two UUIDs":                       machineB + "," + machineA,
		"empty":                           "",
	}

	before := len(scanAll(t)) + len(scanEvents(t))
	for stage, body := range stageBodies {
		for name, id := range ids {
			t.Run(stage+"/"+name, func(t *testing.T) {
				ddblocal.ResetCounts()
				resp, err := ApiRouter(syncRequest(stage, id, body))
				if err != nil {
					t.Fatalf("handler error: %v", err)
				}
				if resp.StatusCode != http.StatusBadRequest {
					t.Errorf("machine ID %q: got status %d, want 400", id, resp.StatusCode)
				}
				if counts := ddblocal.Counts(); len(counts) != 0 {
					t.Errorf("machine ID %q reached DynamoDB: %v", id, counts)
				}
			})
		}
	}
	if after := len(scanAll(t)) + len(scanEvents(t)); after != before {
		t.Errorf("refused requests changed the tables: %d items before, %d after", before, after)
	}
}
