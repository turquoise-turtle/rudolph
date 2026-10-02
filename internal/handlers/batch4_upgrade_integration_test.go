//go:build integration

package handlers

// UPG (batch 4): rows written by earlier versions keep working. The rows are written exactly as the code at 85048d7
// (before batch 3) and the original Rudolph (airbnb/rudolph 34f5ebb) wrote them, and the simulated Mac holds what
// those versions sent it: every machine rule row on every sync, whether it still applied or not.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/fakesanta"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/aws/aws-lambda-go/events"
)

const sha4 = "d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4d4"

type oldVersion struct {
	name string
	// syncState returns the sync state row the old preflight and postflight left for a completed sync.
	syncState func(now time.Time, completed bool) map[string]any
	// globalRule is the old row of the global BLOCKLIST rule for sha1.
	globalRule map[string]any
}

var oldVersions = []oldVersion{
	{
		name: "85048d7",
		syncState: func(now time.Time, completed bool) map[string]any {
			row := map[string]any{
				"PK": "Machine#" + machineA, "SK": "SyncState", "MachineID": machineA, "DataType": "SyncState",
				"BatchSize": 50, "CleanSync": false, "LastCleanSync": clock.RFC3339(now.Add(-24 * time.Hour)),
				"FeedSyncCursor": clock.RFC3339(now.Add(-30 * time.Minute)), "PreflightAt": clock.RFC3339(now.Add(-20 * time.Minute)),
				"RuledownloadStartedAt": "", "RuledownloadFinishedAt": "", "PostflightAt": "",
				"RulesReceived": 0, "RulesProcessed": 0, "ExpiresAfter": now.Add(90 * 24 * time.Hour).Unix(),
			}
			if completed {
				row["RuledownloadStartedAt"] = clock.RFC3339(now.Add(-20 * time.Minute))
				row["RuledownloadFinishedAt"] = clock.RFC3339(now.Add(-20 * time.Minute))
				row["PostflightAt"] = clock.RFC3339(now.Add(-19 * time.Minute))
				row["LastSyncType"] = "NORMAL"
				row["RulesReceived"], row["RulesProcessed"] = 5, 5
				row["RulesHashAfterSync"] = "0123abcd"
			}
			return row
		},
		globalRule: map[string]any{"PK": "GlobalRules", "SK": "Binary#" + sha1, "RuleType": 1, "Policy": 2, "Identifier": sha1, "Description": "old"},
	},
	{
		name: "original Rudolph",
		syncState: func(now time.Time, completed bool) map[string]any {
			row := map[string]any{
				"PK": "Machine#" + machineA, "SK": "SyncState", "MachineID": machineA, "DataType": "SyncState",
				"BatchSize": 50, "CleanSync": false, "LastCleanSync": clock.RFC3339(now.Add(-24 * time.Hour)),
				"FeedSyncCursor": clock.RFC3339(now.Add(-30 * time.Minute)), "PreflightAt": clock.RFC3339(now.Add(-20 * time.Minute)),
				"RuledownloadStartedAt": "", "RuledownloadFinishedAt": "", "PostflightAt": "",
				"ExpiresAfter": now.Add(90 * 24 * time.Hour).Unix(),
			}
			if completed {
				row["RuledownloadStartedAt"] = clock.RFC3339(now.Add(-20 * time.Minute))
				row["RuledownloadFinishedAt"] = clock.RFC3339(now.Add(-20 * time.Minute))
				row["PostflightAt"] = clock.RFC3339(now.Add(-19 * time.Minute))
			}
			return row
		},
		// The original Rudolph stored binary rules with SHA256 instead of Identifier.
		globalRule: map[string]any{"PK": "GlobalRules", "SK": "Binary#" + sha1, "RuleType": 1, "Policy": 2, "SHA256": sha1, "Description": "old"},
	},
}

// seedOldVersion writes what an old version left after its last completed sync, and returns a Mac holding what it was
// sent.
func seedOldVersion(t *testing.T, v oldVersion, transport fakesanta.Transport) *fakesanta.Host {
	t.Helper()
	now := time.Now().UTC()
	putItem(t, v.globalRule)
	putItem(t, map[string]any{"PK": "GlobalRules", "SK": "TeamID#" + chromeTeamID, "RuleType": 4, "Policy": 1, "Identifier": chromeTeamID})
	putItem(t, v.syncState(now, true))
	putItem(t, map[string]any{
		"PK": "Machine#" + machineA, "SK": "Current", "MachineID": machineA, "DataType": "SensorData", "SerialNum": "C02OLDSERIAL",
		"OSVersion": "15.6", "OSBuild": "24G84", "SantaVersion": "2026.8.0", "ClientMode": 1, "PrimaryUser": "alice",
		"RuleCount": 5, "BinaryRuleCount": 4, "TeamIDRuleCount": 1, "Time": clock.RFC3339(now.Add(-20 * time.Minute)),
		"ExpiresAfter": now.Add(90 * 24 * time.Hour).Unix(),
	})
	machineRule := func(sha string, policy int, extra map[string]any) {
		row := map[string]any{"PK": "MachineRules#" + machineA, "SK": "Binary#" + sha, "RuleType": 1, "Policy": policy, "Identifier": sha, "Description": "old"}
		for k, val := range extra {
			row[k] = val
		}
		putItem(t, row)
	}
	machineRule(sha2, 1, map[string]any{"ExpiresAfter": now.Add(24 * time.Hour).Unix()})                     // applies
	machineRule(sha3, 1, map[string]any{"ExpiresAfter": now.Add(-time.Hour).Unix()})                         // expired, not yet deleted by TTL
	machineRule(sha1, 2, map[string]any{"DeleteOnNextSync": true, "ExpiresAfter": now.Add(time.Hour).Unix()}) // removed override: inherits the global BLOCKLIST
	machineRule(sha4, 4, map[string]any{"DeleteOnNextSync": true, "ExpiresAfter": now.Add(time.Hour).Unix()}) // removed, no global rule: REMOVE

	host := newHost(t, machineA, transport)
	for _, sha := range []string{sha1, sha2, sha3, sha4} {
		host.Rules[fakesanta.RuleKey{Type: "BINARY", Identifier: sha}] = fakesanta.Rule{Policy: "ALLOWLIST"}
	}
	host.Rules[fakesanta.RuleKey{Type: "TEAMID", Identifier: chromeTeamID}] = fakesanta.Rule{Policy: "ALLOWLIST"}
	return host
}

// oldInFlight makes the first sync after the upgrade one that the old version started: its preflight and first rule
// download page were answered by the old Lambda, the rest by the new one.
func oldInFlight(t *testing.T, base fakesanta.Transport, v oldVersion, preflight, firstPage string, cleanSync bool) fakesanta.Transport {
	served := map[string]bool{}
	return func(req events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, error) {
		switch {
		case req.Resource == "/preflight/{machine_id}" && !served["preflight"]:
			served["preflight"] = true
			row := v.syncState(time.Now().UTC(), false)
			row["PreflightAt"] = clock.RFC3339(time.Now())
			row["CleanSync"] = cleanSync
			putItem(t, row)
			return &events.APIGatewayProxyResponse{StatusCode: 200, Body: preflight}, nil
		case req.Resource == "/ruledownload/{machine_id}" && !served["ruledownload"]:
			served["ruledownload"] = true
			updateItem(t, "Machine#"+machineA, "SyncState", map[string]any{"RuledownloadStartedAt": clock.RFC3339(time.Now())})
			return &events.APIGatewayProxyResponse{StatusCode: 200, Body: firstPage}, nil
		}
		return base(req)
	}
}

func cursorJSON(t *testing.T, cursor map[string]any) string {
	t.Helper()
	c, _ := json.Marshal(cursor)
	page, _ := json.Marshal(map[string]any{"cursor": string(c)})
	return string(page)
}

func TestUPG_RowsFromEarlierVersionsKeepWorking(t *testing.T) {
	oldPreflight := `{"client_mode":"MONITOR","blocked_path_regex":"","allowed_path_regex":"","batch_size":50,"enable_bundles":false,"enable_transitive_rules":false,"full_sync_interval":600,"sync_type":"normal"}`
	oldCleanPreflight := strings.Replace(oldPreflight, `"normal"`, `"clean"`, 1)
	for _, v := range oldVersions {
		for _, inFlight := range []string{"", "incremental sync on its machine page (strategy 3)", "clean sync between global pages (strategy 1)"} {
			name := v.name
			if inFlight != "" {
				name += ", in flight: " + inFlight
			}
			t.Run(name, func(t *testing.T) {
				base := freshServer(t)
				transport := base
				switch inFlight {
				case "incremental sync on its machine page (strategy 3)":
					transport = oldInFlight(t, base, v, oldPreflight, cursorJSON(t, map[string]any{"strategy": 3, "page": 2, "batch_size": 50}), false)
				case "clean sync between global pages (strategy 1)":
					first := `{"rules":[{"rule_type":"BINARY","policy":"BLOCKLIST","identifier":"` + sha1 + `"}],"cursor":`
					c, _ := json.Marshal(map[string]any{"strategy": 1, "page": 2, "batch_size": 1, "pk": "GlobalRules", "sk": "Binary#" + sha1})
					cs, _ := json.Marshal(string(c))
					transport = oldInFlight(t, base, v, oldCleanPreflight, first+string(cs)+"}", true)
				}
				host := seedOldVersion(t, v, transport)

				for i := 0; i < 3; i++ {
					if _, err := host.Sync(t); err != nil {
						t.Fatalf("sync %d after the upgrade failed: %v", i+1, err)
					}
				}
				for sha, want := range map[string]string{sha1: "BLOCKLIST", sha2: "ALLOWLIST", sha3: "", sha4: ""} {
					if got := host.Policy("BINARY", sha); got != want {
						t.Errorf("after 3 syncs the Mac has %q for %s, want %q", got, sha[:4], want)
					}
				}
				if host.Policy("TEAMID", chromeTeamID) != "ALLOWLIST" {
					t.Error("the global TeamID rule is missing")
				}
				for _, item := range scanAll(t) {
					if b, ok := item["DeleteOnNextSync"]; ok && strings.Contains(stringOf(b), "true") {
						t.Errorf("row marked for deletion still there after 3 syncs: %s", stringAttr(item, "SK"))
					}
				}
				state := getItem(t, "Machine#"+machineA, "SyncState")
				if numberAttr(t, state, "SyncSeq") == 0 {
					t.Error("the sync state has no SyncSeq after 3 syncs")
				}

				// Tracking converges to the rules that apply.
				var tracked map[string]bool
				for i := 0; i < 3; i++ {
					tracked = map[string]bool{}
					for sk := range trackingItems(t, machineA) {
						tracked[sk] = true
					}
					if len(tracked) == 1 && tracked["Binary#"+sha2] {
						break
					}
					host.MustSync(t)
				}
				if len(tracked) != 1 || !tracked["Binary#"+sha2] {
					t.Errorf("tracking never converged to the one applying machine rule: %v", tracked)
				}
				if s := stringAttr(getItem(t, "Machine#"+machineA, "SyncState"), "MachineRulesTracked"); s == "" || s == "dirty" {
					t.Errorf("the tracking summary is %q after the upgrade settled", s)
				}
			})
		}
	}
}

func stringOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
