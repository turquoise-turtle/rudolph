//go:build integration

package handlers

import (
	"testing"
	"time"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/types"
)

// Three syncs in a row fail after the server served their last page (the response is lost, or santad fails to
// apply). None applied anything. Does the Mac still get the feed rule and the removal once it recovers?
// Found by the batch 4 tracking review: LastPageServedAt must not count a served last page as an applied one.
func TestReview_ServedButUnappliedLastPagesKeepFeedRulesAndRemovals(t *testing.T) {
	base := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	addMachineRule(t, machineA, sha2, types.RuleTypeBinary, types.RulePolicyAllowlist, time.Now().Add(24*time.Hour))
	lose := map[string]int{}
	host := newHost(t, machineA, losingResponses(base, lose))
	host.MustSync(t)
	host.MustSync(t)
	ageRows(t, time.Hour)

	addGlobalRule(t, sha3, types.RuleTypeBinary, types.RulePolicyBlocklist)
	removeMachineRule(t, machineA, sha2)
	ageRows(t, time.Hour)

	for i := 0; i < 3; i++ {
		lose["ruledownload"] = 1
		if _, err := host.Sync(t); err == nil {
			t.Fatal("precondition: the sync fails")
		}
		ageRows(t, time.Hour)
	}
	for i := 0; i < 3; i++ {
		res := host.MustSync(t)
		t.Logf("recovered sync %d: type %s, %d rules", i, res.SyncType, res.RulesReceived)
	}
	if got := host.Policy("BINARY", sha3); got != "BLOCKLIST" {
		t.Errorf("feed rule added before the failed syncs never reached the Mac: %q", got)
	}
	if got := host.Policy("BINARY", sha2); got != "" {
		t.Errorf("removal never reached the Mac: %q", got)
	}
}

// The same failures on a Mac that reports a real rules_hash (Santa 2026.8): an unchanged hash shows the served last
// pages were not applied.
func TestReview_HashingMacServedButUnappliedLastPagesKeepFeedRulesAndRemovals(t *testing.T) {
	base := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	addMachineRule(t, machineA, sha2, types.RuleTypeBinary, types.RulePolicyAllowlist, time.Now().Add(24*time.Hour))
	lose := map[string]int{}
	host := newHost(t, machineA, losingResponses(base, lose))
	host.HashRules = true
	host.MustSync(t)
	host.MustSync(t)
	ageRows(t, time.Hour)

	addGlobalRule(t, sha3, types.RuleTypeBinary, types.RulePolicyBlocklist)
	removeMachineRule(t, machineA, sha2)
	ageRows(t, time.Hour)

	for i := 0; i < 8; i++ {
		lose["ruledownload"] = 1
		if _, err := host.Sync(t); err == nil {
			t.Fatal("precondition: the sync fails")
		}
		ageRows(t, time.Hour)
	}
	for i := 0; i < 3; i++ {
		host.MustSync(t)
	}
	if got := host.Policy("BINARY", sha3); got != "BLOCKLIST" {
		t.Errorf("feed rule added before the failed syncs never reached the Mac: %q", got)
	}
	if got := host.Policy("BINARY", sha2); got != "" {
		t.Errorf("removal never reached the Mac: %q", got)
	}
}

// A Mac whose postflight always fails and whose rules then stop changing: its rules_hash stays the same because there
// is nothing new to apply, not because it failed to apply. Its syncs must not keep re-sending rules it already has,
// and above all must not repeat a periodic clean sync (every rule) on every sync.
func TestReview_HashingMacWhosePostflightAlwaysFailsDoesNotRepeatCleanSyncs(t *testing.T) {
	base := freshServer(t)
	for i := 0; i < 300; i++ {
		addGlobalRule(t, shaN(0x5000+i), types.RuleTypeBinary, types.RulePolicyBlocklist)
	}
	alwaysFail := true
	host := newHost(t, machineA, failingStage(base, "postflight", 500, &alwaysFail))
	host.IgnorePostflightFailure = true
	host.HashRules = true
	// An hour between syncs, so the rules leave the feed cursor's overlap and only re-sending would bring them back.
	for i := 0; i < 4; i++ {
		host.MustSync(t)
		ageRows(t, time.Hour)
	}
	updateItem(t, "Machine#"+machineA, "SyncState", map[string]any{"LastCleanSync": clock.RFC3339(time.Now().Add(-30 * 24 * time.Hour))})

	var syncTypes []string
	var received []int
	for i := 0; i < 8; i++ {
		res := host.MustSync(t)
		if res.PostflightError == nil {
			t.Fatal("precondition: the postflight fails")
		}
		syncTypes = append(syncTypes, res.SyncType)
		received = append(received, res.RulesReceived)
		ageRows(t, time.Hour)
	}
	clean := 0
	for _, st := range syncTypes {
		if st == "CLEAN" || st == "CLEAN_ALL" {
			clean++
		}
	}
	if clean == 0 {
		t.Fatalf("precondition: the periodic clean sync happens (%v)", syncTypes)
	}
	if clean > 4 {
		t.Errorf("sync types %v, rules received %v: the periodic clean sync is repeated on every sync", syncTypes, received)
	}
	for _, n := range received[len(received)-3:] {
		if n > 50 {
			t.Errorf("rules received per sync %v: the last syncs keep re-sending rules the Mac already has", received)
			break
		}
	}
}
