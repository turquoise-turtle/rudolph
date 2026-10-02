//go:build integration

package handlers

// RETRY: Santa resends a request whose response it did not get (SNTSyncStage.mm), after the server already handled
// it. Every stage must be safe to replay. SILENTPF: Santa ignores a failed postflight, so a Mac whose postflights never
// arrive must keep syncing correctly at a bounded cost.

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/fakesanta"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-lambda-go/events"
)

// losingResponses returns a transport whose next lose[stage] responses for that stage never reach the Mac: the server
// handles the request, the Mac sees a dropped connection and retries.
func losingResponses(base fakesanta.Transport, lose map[string]int) fakesanta.Transport {
	return func(req events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, error) {
		resp, err := base(req)
		for stage, n := range lose {
			if n > 0 && req.Resource == "/"+stage+"/{machine_id}" {
				lose[stage] = n - 1
				return nil, errors.New("connection lost after the server answered")
			}
		}
		return resp, err
	}
}

func TestRETRY_ReplayedLastRuleDownloadPageKeepsMacAndServerConsistent(t *testing.T) {
	base := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	addMachineRule(t, machineA, sha2, types.RuleTypeBinary, types.RulePolicyAllowlist, time.Now().Add(24*time.Hour))
	lose := map[string]int{}
	host := newHost(t, machineA, losingResponses(base, lose))
	host.Retries = true
	host.MustSync(t)
	host.MustSync(t)

	removeMachineRule(t, machineA, sha2)
	for i := 0; i < 3; i++ {
		lose["ruledownload"] = 1 // the only, and so the last, page of each sync is handled twice
		host.MustSync(t)
	}
	if got := host.Policy("BINARY", sha2); got != "" {
		t.Fatalf("removal lost when the last page was replayed: %q", got)
	}

	addMachineRule(t, machineA, sha2, types.RuleTypeBinary, types.RulePolicyAllowlist, time.Now().Add(24*time.Hour))
	lose["ruledownload"] = 1
	host.MustSync(t)
	if host.Policy("BINARY", sha2) != "ALLOWLIST" {
		t.Fatal("a re-added rule did not reach the Mac after a replayed page")
	}
	removeMachineRule(t, machineA, sha2)
	for i := 0; i < 3; i++ {
		lose["ruledownload"] = 1
		host.MustSync(t)
	}
	if got := host.Policy("BINARY", sha2); got != "" {
		t.Fatalf("the second removal was lost after replayed pages: %q", got)
	}
}

// A replayed preflight must not record a clean sync the Mac never performed: the first preflight decided on the
// periodic clean sync, its response was lost, and the retried preflight answered NORMAL. The clean sync is still due.
func TestRETRY_ReplayedPreflightDoesNotSkipAPeriodicCleanSync(t *testing.T) {
	base := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	lose := map[string]int{}
	host := newHost(t, machineA, losingResponses(base, lose))
	host.Retries = true
	host.MustSync(t)
	host.MustSync(t)
	updateItem(t, "Machine#"+machineA, "SyncState", map[string]any{"LastCleanSync": clock.RFC3339(time.Now().Add(-30 * 24 * time.Hour))})

	lose["preflight"] = 1
	first := host.MustSync(t)
	if first.Requests["preflight"] != 2 {
		t.Fatalf("precondition: the preflight was sent twice, got %d", first.Requests["preflight"])
	}
	second := host.MustSync(t)
	if first.SyncType != "CLEAN" && second.SyncType != "CLEAN" {
		t.Fatalf("the periodic clean sync (last one 30 days ago) never happened: the replayed preflight recorded a clean sync the Mac did not perform (%s, then %s)", first.SyncType, second.SyncType)
	}
}

func TestRETRY_ReplayedPreflightKeepsRemovalsAndFeed(t *testing.T) {
	base := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	addMachineRule(t, machineA, sha2, types.RuleTypeBinary, types.RulePolicyAllowlist, time.Now().Add(24*time.Hour))
	lose := map[string]int{}
	host := newHost(t, machineA, losingResponses(base, lose))
	host.Retries = true
	host.MustSync(t)
	host.MustSync(t)

	removeMachineRule(t, machineA, sha2)
	addGlobalRule(t, sha3, types.RuleTypeBinary, types.RulePolicyBlocklist)
	for i := 0; i < 3; i++ {
		lose["preflight"] = 1
		host.MustSync(t)
	}
	if host.Policy("BINARY", sha2) != "" || host.Policy("BINARY", sha3) != "BLOCKLIST" {
		t.Fatalf("replayed preflights lost a removal or a new rule: %v", host.Rules)
	}
}

func TestRETRY_ReplayedPostflightKeepsRemovalsAndFeed(t *testing.T) {
	base := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	addMachineRule(t, machineA, sha2, types.RuleTypeBinary, types.RulePolicyAllowlist, time.Now().Add(24*time.Hour))
	lose := map[string]int{}
	host := newHost(t, machineA, losingResponses(base, lose))
	host.Retries = true
	host.MustSync(t)
	host.MustSync(t)

	removeMachineRule(t, machineA, sha2)
	for i := 0; i < 3; i++ {
		lose["postflight"] = 1
		host.MustSync(t)
	}
	addGlobalRule(t, sha3, types.RuleTypeBinary, types.RulePolicyBlocklist)
	lose["postflight"] = 1
	host.MustSync(t)
	if host.Policy("BINARY", sha2) != "" || host.Policy("BINARY", sha3) != "BLOCKLIST" {
		t.Fatalf("replayed postflights lost a removal or a new rule: %v", host.Rules)
	}
}

// SILENTPF: a Mac whose postflight never arrives (Santa ignores the failure) keeps getting new rules and removals,
// and its syncs must not re-read an ever-growing part of the rules feed: today its feed cursor never advances, so
// every sync re-sends every feed change since its first sync.
func TestSILENTPF_MacWhosePostflightAlwaysFailsKeepsSyncingAtBoundedCost(t *testing.T) {
	base := freshServer(t)
	toggled := []string{shaN(0xf1), shaN(0xf2), shaN(0xf3), shaN(0xf4), shaN(0xf5)}
	for _, sha := range toggled {
		addGlobalRule(t, sha, types.RuleTypeBinary, types.RulePolicyBlocklist)
	}
	addMachineRule(t, machineA, sha2, types.RuleTypeBinary, types.RulePolicyAllowlist, time.Now().Add(365*24*time.Hour))
	alwaysFail := true
	host := newHost(t, machineA, failingStage(base, "postflight", 500, &alwaysFail))
	host.IgnorePostflightFailure = true
	host.MustSync(t)

	// Three days pass; meanwhile the admin changed these 5 rules 2000 times in total (each change is a feed row).
	ageRows(t, 3*24*time.Hour)
	var feed []map[string]any
	start := time.Now().Add(-3*24*time.Hour + time.Hour)
	for i := 0; i < 2000; i++ {
		sha := toggled[i%len(toggled)]
		policy := 1 + i%2
		at := start.Add(time.Duration(i) * time.Minute)
		feed = append(feed, map[string]any{
			"PK": "RulesFeed", "SK": fmt.Sprintf("%s#Binary#%s", clock.RFC3339(at), sha),
			"RuleType": 1, "Policy": policy, "Identifier": sha, "DataType": "RulesFeed",
			"ExpiresAfter": at.Add(90 * 24 * time.Hour).Unix(),
		})
	}
	batchPut(t, feed)

	var received []int
	for i := 0; i < 6; i++ {
		if i == 2 {
			addGlobalRule(t, sha3, types.RuleTypeBinary, types.RulePolicyBlocklist)
			removeMachineRule(t, machineA, sha2)
		}
		res := host.MustSync(t)
		if res.PostflightError == nil {
			t.Fatal("precondition: the postflight fails")
		}
		received = append(received, res.RulesReceived)
	}
	if host.Policy("BINARY", sha3) != "BLOCKLIST" || host.Policy("BINARY", sha2) != "" {
		t.Fatalf("a Mac without postflights must still get new rules and removals: %v", host.Rules)
	}
	for _, n := range received[4:] {
		if n > 100 {
			t.Fatalf("rules sent per sync %v: a Mac whose postflight always fails re-reads the whole feed since its first sync on every sync", received)
		}
	}
}
