//go:build integration

package handlers

// Found by the review of the batch 4 tracking redesign (SILENTPF with rules_hash): cost and loss for Macs whose
// postflights never arrive, and the periodic clean sync of Macs whose postflights do.

import (
	"testing"
	"time"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/types"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// A Mac whose postflight always fails, reporting a real rules hash, applies a sync that brings new feed rules (its
// hash changes, so that sync counts). The fleet's rules then stop changing. Later syncs must not keep re-sending the
// rules the Mac applied.
func TestReview_HashingMacDoesNotKeepResendingRulesItApplied(t *testing.T) {
	base := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	alwaysFail := true
	host := newHost(t, machineA, failingStage(base, "postflight", 500, &alwaysFail))
	host.IgnorePostflightFailure = true
	host.HashRules = true
	for i := 0; i < 4; i++ {
		host.MustSync(t)
		ageRows(t, time.Hour)
	}
	for i := 0; i < 200; i++ {
		addGlobalRule(t, shaN(0x7000+i), types.RuleTypeBinary, types.RulePolicyBlocklist)
	}
	ageRows(t, time.Hour)
	res := host.MustSync(t)
	t.Logf("rule-changing sync: %s, %d rules", res.SyncType, res.RulesReceived)
	ageRows(t, time.Hour)

	var received []int
	for i := 0; i < 6; i++ {
		res := host.MustSync(t)
		received = append(received, res.RulesReceived)
		ageRows(t, time.Hour)
	}
	t.Logf("rules received by the syncs after it: %v", received)
	for _, n := range received[2:] {
		if n > 50 {
			t.Fatalf("rules received per sync %v: the rules the Mac applied are re-sent on every sync while the fleet's rules do not change", received)
		}
	}
}

// A Mac that reports no rules hash and whose postflights never arrive: two lost last pages in a row must not lose a
// feed rule or a removal (SyncsWithoutPostflight stays at 3 or more for such a Mac, so it cannot be the only guard).
func TestReview_NoHashMacWithBlockedPostflightsSurvivesTwoFailedSyncs(t *testing.T) {
	base := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	addMachineRule(t, machineA, sha2, types.RuleTypeBinary, types.RulePolicyAllowlist, time.Now().Add(24*time.Hour))
	alwaysFail := true
	lose := map[string]int{}
	host := newHost(t, machineA, losingResponses(failingStage(base, "postflight", 500, &alwaysFail), lose))
	host.IgnorePostflightFailure = true
	for i := 0; i < 5; i++ {
		host.MustSync(t)
		ageRows(t, time.Hour)
	}
	addGlobalRule(t, sha3, types.RuleTypeBinary, types.RulePolicyBlocklist)
	removeMachineRule(t, machineA, sha2)
	ageRows(t, time.Hour)
	for i := 0; i < 2; i++ {
		lose["ruledownload"] = 1
		if _, err := host.Sync(t); err == nil {
			t.Fatal("precondition: the sync fails")
		}
		ageRows(t, time.Hour)
	}
	for i := 0; i < 3; i++ {
		host.MustSync(t)
		ageRows(t, time.Hour)
	}
	if got := host.Policy("BINARY", sha3); got != "BLOCKLIST" {
		t.Errorf("feed rule lost after two failed syncs: %q", got)
	}
	if got := host.Policy("BINARY", sha2); got != "" {
		t.Errorf("removal lost after two failed syncs: %q", got)
	}
}

// A Mac whose postflights arrive: its periodic clean sync fails after the server served the last page (the response
// is lost). Nothing shows the Mac performed it, so the next sync should be a clean sync again.
func TestReview_HealthyMacRetriesALostPeriodicCleanSync(t *testing.T) {
	base := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	lose := map[string]int{}
	host := newHost(t, machineA, losingResponses(base, lose))
	host.HashRules = true
	for i := 0; i < 3; i++ {
		host.MustSync(t)
		ageRows(t, time.Hour)
	}
	updateItem(t, "Machine#"+machineA, "SyncState", map[string]any{"LastCleanSync": clock.RFC3339(time.Now().Add(-30 * 24 * time.Hour))})
	lose["ruledownload"] = 1
	if _, err := host.Sync(t); err == nil {
		t.Fatal("precondition: the sync fails")
	}
	if !getItem(t, "Machine#"+machineA, "SyncState")["CleanSync"].(*awstypes.AttributeValueMemberBOOL).Value {
		t.Fatal("precondition: the failed sync was the periodic clean sync")
	}
	res := host.MustSync(t)
	if res.SyncType != "CLEAN" && res.SyncType != "CLEAN_ALL" {
		t.Errorf("sync after a lost periodic clean sync is %s: the clean sync is not retried until the next period", res.SyncType)
	}
}

// A Mac whose postflight always fails, reporting a real rules hash. A machine rule is sent by a sync whose response
// is lost, then removed: the REMOVE that follows changes nothing on the Mac, so its hash never changes. With the
// fleet's rules otherwise unchanged, the Mac's feed cursor must still keep up, and the removal must stop being sent.
func TestReview_HashingMacIsNotStuckOnARemovalThatChangesNothing(t *testing.T) {
	base := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	alwaysFail := true
	lose := map[string]int{}
	host := newHost(t, machineA, losingResponses(failingStage(base, "postflight", 500, &alwaysFail), lose))
	host.IgnorePostflightFailure = true
	host.HashRules = true
	for i := 0; i < 4; i++ {
		host.MustSync(t)
		ageRows(t, time.Hour)
	}
	addMachineRule(t, machineA, sha2, types.RuleTypeBinary, types.RulePolicyAllowlist, time.Now().Add(24*time.Hour))
	lose["ruledownload"] = 1
	if _, err := host.Sync(t); err == nil {
		t.Fatal("precondition: the sync fails")
	}
	removeMachineRule(t, machineA, sha2)
	ageRows(t, time.Hour)
	var lags []time.Duration
	var received []int
	for i := 0; i < 12; i++ {
		res := host.MustSync(t)
		received = append(received, res.RulesReceived)
		row := getItem(t, "Machine#"+machineA, "SyncState")
		cursor, _ := clock.ParseRFC3339(row["FeedSyncCursor"].(*awstypes.AttributeValueMemberS).Value)
		preflight, _ := clock.ParseRFC3339(row["PreflightAt"].(*awstypes.AttributeValueMemberS).Value)
		lags = append(lags, preflight.Sub(cursor))
		ageRows(t, time.Hour)
	}
	t.Logf("rules received %v; feed cursor lag behind each sync's start %v", received, lags)
	// Nothing proves a REMOVE changes nothing on the Mac (it may hold an identical rule from elsewhere), so only the
	// unchanged-hash fallback lets these syncs count: the lag may grow for a few syncs, but not without bound.
	if lags[len(lags)-1] > 8*time.Hour {
		t.Errorf("feed cursor lags %v behind the sync, growing by the time between syncs: no sync counts as completed while a REMOVE that changes nothing on the Mac is pending", lags[len(lags)-1])
	}
	if received[len(received)-1] != 0 {
		t.Errorf("rules received %v: the removal is re-sent on every sync and never confirmed", received)
	}
}
