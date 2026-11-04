//go:build integration

package handlers

// Found by the second review of the batch 4 tracking redesign (round 4): loss sequences shorter than the documented
// bounds. R4-3 and R4-4 lose nothing with one failed sync fewer; each sits exactly on the bound it tests.

import (
	"testing"
	"time"

	"github.com/airbnb/rudolph/pkg/types"
)

// R4-1. A machine rule the Mac received in a sync that was proven applied by nothing: the Mac applied it, but its
// postflight was the first one to fail, so ruledownload did not record serving its last page and the changed hash is
// not taken as proof. Its later removal is then treated as changing nothing on the Mac, and two lost last pages
// confirm it.
func TestReview_R4_RemovalOfARuleReceivedInTheFirstSyncWithoutPostflightIsNotLost(t *testing.T) {
	base := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	failPostflight := false
	lose := map[string]int{}
	host := newHost(t, machineA, losingResponses(failingStage(base, "postflight", 500, &failPostflight), lose))
	host.IgnorePostflightFailure = true
	host.HashRules = true
	for i := 0; i < 3; i++ {
		host.MustSync(t)
		ageRows(t, time.Hour)
	}
	addMachineRule(t, machineA, sha2, types.RuleTypeBinary, types.RulePolicyAllowlist, time.Now().Add(240*time.Hour))
	failPostflight = true // the postflight route breaks from this sync on
	host.MustSync(t)
	if host.Policy("BINARY", sha2) != "ALLOWLIST" {
		t.Fatal("precondition: the Mac holds the machine rule")
	}
	ageRows(t, time.Hour)
	for i := 0; i < 3; i++ {
		host.MustSync(t)
		ageRows(t, time.Hour)
	}
	removeMachineRule(t, machineA, sha2)
	for i := 0; i < 2; i++ {
		lose["ruledownload"] = 1
		if _, err := host.Sync(t); err == nil {
			t.Fatal("precondition: the sync fails")
		}
		ageRows(t, time.Hour)
	}
	for i := 0; i < 4; i++ {
		host.MustSync(t)
		ageRows(t, time.Hour)
	}
	if got := host.Policy("BINARY", sha2); got != "" {
		t.Errorf("removal lost after two failed syncs: the Mac still holds %q", got)
	}
}

// R4-2. A machine rule added while the Mac already held an identical global rule (the hash does not change), then the
// global rule changes to BLOCKLIST (the machine rule still overrides it, the hash still does not change). When the
// machine rule is removed, the replacement BLOCKLIST does change the Mac, but is treated as changing nothing.
func TestReview_R4_ReplacementOfAMachineRuleThatShadowedAnIdenticalGlobalRuleIsNotLost(t *testing.T) {
	base := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	addGlobalRule(t, sha3, types.RuleTypeBinary, types.RulePolicyAllowlist)
	alwaysFail := true
	lose := map[string]int{}
	host := newHost(t, machineA, losingResponses(failingStage(base, "postflight", 500, &alwaysFail), lose))
	host.IgnorePostflightFailure = true
	host.HashRules = true
	for i := 0; i < 4; i++ {
		host.MustSync(t)
		ageRows(t, time.Hour)
	}
	addMachineRule(t, machineA, sha3, types.RuleTypeBinary, types.RulePolicyAllowlist, time.Now().Add(240*time.Hour))
	ageRows(t, time.Hour)
	host.MustSync(t)
	ageRows(t, time.Hour)
	addGlobalRule(t, sha3, types.RuleTypeBinary, types.RulePolicyBlocklist)
	ageRows(t, time.Hour)
	for i := 0; i < 12; i++ { // the global change leaves the Mac as it is; let the fallback move the cursor past it
		host.MustSync(t)
		ageRows(t, time.Hour)
	}
	if host.Policy("BINARY", sha3) != "ALLOWLIST" {
		t.Fatal("precondition: the machine rule overrides the global rule")
	}
	removeMachineRule(t, machineA, sha3)
	for i := 0; i < 2; i++ {
		lose["ruledownload"] = 1
		if _, err := host.Sync(t); err == nil {
			t.Fatal("precondition: the sync fails")
		}
		ageRows(t, time.Hour)
	}
	for i := 0; i < 4; i++ {
		host.MustSync(t)
		ageRows(t, time.Hour)
	}
	if got := host.Policy("BINARY", sha3); got != "BLOCKLIST" {
		t.Errorf("after two failed syncs the Mac keeps %q instead of the fleet's BLOCKLIST", got)
	}
}

// R4-3. A Mac without rules hash and blocked postflights, while feed rules keep being written: every fourth sync
// counts through the fallback. A rule first sent by a sync that counts is lost once that sync and the next four
// fail (5 failed syncs, the docs say 8).
func TestReview_R4_NoHashMacSurvivesFiveFailedSyncsWhileRulesKeepChanging(t *testing.T) {
	base := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	alwaysFail := true
	lose := map[string]int{}
	host := newHost(t, machineA, losingResponses(failingStage(base, "postflight", 500, &alwaysFail), lose))
	host.IgnorePostflightFailure = true
	for i := 0; i < 6; i++ { // steady state: syncs serve nothing new and count
		host.MustSync(t)
		ageRows(t, time.Hour)
	}
	for i := 0; i < 3; i++ { // applied, but nothing shows it: SyncsSinceCompleted 0, 1, 2
		addGlobalRule(t, shaN(0x9000+i), types.RuleTypeBinary, types.RulePolicyBlocklist)
		ageRows(t, time.Hour)
		host.MustSync(t)
		ageRows(t, time.Hour)
	}
	addGlobalRule(t, sha3, types.RuleTypeBinary, types.RulePolicyBlocklist)
	ageRows(t, time.Hour)
	for i := 0; i < 5; i++ {
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
		t.Errorf("feed rule lost after 5 failed syncs in a row: %q", got)
	}
}

// R4-4. The same for a Mac that reports a hash: four applied syncs whose content leaves the hash unchanged (a rule it
// already holds, written again) let the next sync count through the fallback. A rule first sent by it is lost once
// it and the next five fail (6 failed syncs, the docs say 10).
func TestReview_R4_HashingMacSurvivesSixFailedSyncsAfterHashNeutralSyncs(t *testing.T) {
	base := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	alwaysFail := true
	lose := map[string]int{}
	host := newHost(t, machineA, losingResponses(failingStage(base, "postflight", 500, &alwaysFail), lose))
	host.IgnorePostflightFailure = true
	host.HashRules = true
	for i := 0; i < 6; i++ {
		host.MustSync(t)
		ageRows(t, time.Hour)
	}
	for i := 0; i < 4; i++ { // applied, hash unchanged: SyncsSinceCompleted 0..3
		addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
		ageRows(t, time.Hour)
		host.MustSync(t)
		ageRows(t, time.Hour)
	}
	addGlobalRule(t, sha3, types.RuleTypeBinary, types.RulePolicyBlocklist)
	ageRows(t, time.Hour)
	for i := 0; i < 6; i++ {
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
		t.Errorf("feed rule lost after 6 failed syncs in a row: %q", got)
	}
}

// R4-5. A Mac whose sync state expired returns with the rules it held then; a rule was written while it was away. Its
// postflights never arrive and its clean syncs fail after the last page was served. The fallback counts the fifth
// unapplied clean sync as completed, the next syncs are incremental from the clean syncs' cursor, and the rule written
// while the Mac was away never reaches it (until the next periodic clean sync).
func TestReview_R4_UnappliedFirstCleanSyncAfterExpiryIsNotCountedByTheFallback(t *testing.T) {
	base := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	alwaysFail := true
	lose := map[string]int{}
	host := newHost(t, machineA, losingResponses(failingStage(base, "postflight", 500, &alwaysFail), lose))
	host.IgnorePostflightFailure = true
	host.HashRules = true
	for i := 0; i < 3; i++ {
		host.MustSync(t)
		ageRows(t, time.Hour)
	}
	deleteItem(t, "Machine#"+machineA, "SyncState") // expired by TTL while the Mac was away
	addGlobalRule(t, sha3, types.RuleTypeBinary, types.RulePolicyBlocklist)
	ageRows(t, time.Hour)
	for i := 0; i < 5; i++ {
		lose["ruledownload"] = 1
		res, err := host.Sync(t)
		if err == nil {
			t.Fatal("precondition: the sync fails")
		}
		if res != nil && res.SyncType != "" && res.SyncType != "CLEAN" && res.SyncType != "CLEAN_ALL" {
			t.Fatalf("precondition: a clean sync, got %s", res.SyncType)
		}
		ageRows(t, time.Hour)
	}
	var types_ []string
	for i := 0; i < 3; i++ {
		res := host.MustSync(t)
		types_ = append(types_, res.SyncType)
		ageRows(t, time.Hour)
	}
	if got := host.Policy("BINARY", sha3); got != "BLOCKLIST" {
		t.Errorf("sync types after the failed clean syncs %v: the rule written while the Mac was away never reached it: %q", types_, got)
	}
}
