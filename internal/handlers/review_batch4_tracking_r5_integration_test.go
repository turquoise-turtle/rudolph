//go:build integration

package handlers

// Found by the third review of the batch 4 tracking redesign (round 5): a clean sync inside a failure run, and
// recurring rows that leave the hash unchanged.

import (
	"testing"
	"time"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/types"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// R5-1. A rule first reaches a Mac in a periodic clean sync that falls inside a run of failed syncs. A clean sync
// serves no feed row, so it never starts the content run again, and the incremental sync after it does not see the
// rule as new either (it was written before the clean sync started). The rule is lost after the clean sync and the
// five syncs after it fail: 6 served-but-unapplied syncs carried it (the docs say loss needs 8).
func TestReview_R5_RuleFirstServedByACleanSyncInsideAFailureRunIsNotLost(t *testing.T) {
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
	addGlobalRule(t, sha2, types.RuleTypeBinary, types.RulePolicyBlocklist)
	ageRows(t, time.Hour)
	failed := func(wantClean bool) {
		t.Helper()
		lose["ruledownload"] = 1
		if _, err := host.Sync(t); err == nil {
			t.Fatal("precondition: the sync fails")
		}
		clean := getItem(t, "Machine#"+machineA, "SyncState")["CleanSync"]
		isClean := clean != nil && clean.(*awstypes.AttributeValueMemberBOOL).Value
		if isClean != wantClean {
			t.Fatalf("precondition: clean sync %v, got %v", wantClean, isClean)
		}
		ageRows(t, time.Hour)
	}
	failed(false) // run 1 (sha2)
	failed(false) // run 2
	// The periodic clean sync is due, and a rule is written just before it starts
	updateItem(t, "Machine#"+machineA, "SyncState", map[string]any{"LastCleanSync": clock.RFC3339(time.Now().Add(-30 * 24 * time.Hour))})
	addGlobalRule(t, sha3, types.RuleTypeBinary, types.RulePolicyBlocklist)
	ageRows(t, time.Hour)
	failed(true) // run 3, first serves sha3
	for i := 0; i < 5; i++ {
		failed(false)
	}
	for i := 0; i < 3; i++ {
		host.MustSync(t)
		ageRows(t, time.Hour)
	}
	if got := host.Policy("BINARY", sha3); got != "BLOCKLIST" {
		t.Errorf("rule first served by the clean sync lost after 6 failed syncs in a row: %q", got)
	}
}

// R5-2. A Mac that reports a hash, postflights blocked. Every third sync, the feed gets a row that leaves the Mac's
// hash unchanged (here an identical rule written again; in practice e.g. a scheduled `rudolph rules import` of an
// unchanged file, or a custom message edit). Each such row starts the content run again before it reaches the
// fallback, so no sync completes: the feed cursor stays put and every sync re-sends a set that keeps growing.
func TestReview_R5_RecurringHashNeutralRowsDoNotFreezeTheFeedCursor(t *testing.T) {
	base := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	alwaysFail := true
	host := newHost(t, machineA, failingStage(base, "postflight", 500, &alwaysFail))
	host.IgnorePostflightFailure = true
	host.HashRules = true
	for i := 0; i < 6; i++ {
		host.MustSync(t)
		ageRows(t, time.Hour)
	}
	var received []int
	var lags []time.Duration
	for i := 0; i < 15; i++ {
		if i%3 == 0 {
			for j := 0; j < 20; j++ {
				addGlobalRule(t, shaN(0x6000+j), types.RuleTypeBinary, types.RulePolicyBlocklist)
			}
			ageRows(t, time.Minute)
		}
		res := host.MustSync(t)
		received = append(received, res.RulesReceived)
		row := getItem(t, "Machine#"+machineA, "SyncState")
		cursor, _ := clock.ParseRFC3339(row["FeedSyncCursor"].(*awstypes.AttributeValueMemberS).Value)
		preflight, _ := clock.ParseRFC3339(row["PreflightAt"].(*awstypes.AttributeValueMemberS).Value)
		lags = append(lags, preflight.Sub(cursor))
		ageRows(t, time.Hour)
	}
	t.Logf("rules received %v", received)
	t.Logf("feed cursor lag %v", lags)
	if lags[len(lags)-1] > 12*time.Hour {
		t.Errorf("feed cursor lags %v behind the sync and keeps falling behind; rules received per sync %v", lags[len(lags)-1], received)
	}
}

func TestReview_R5_RuleFirstServedByACleanSyncSurvivesFourLaterFailures(t *testing.T) {
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
	addGlobalRule(t, sha2, types.RuleTypeBinary, types.RulePolicyBlocklist)
	ageRows(t, time.Hour)
	failed := func(wantClean bool) {
		t.Helper()
		lose["ruledownload"] = 1
		if _, err := host.Sync(t); err == nil {
			t.Fatal("precondition: the sync fails")
		}
		clean := getItem(t, "Machine#"+machineA, "SyncState")["CleanSync"]
		isClean := clean != nil && clean.(*awstypes.AttributeValueMemberBOOL).Value
		if isClean != wantClean {
			t.Fatalf("precondition: clean sync %v, got %v", wantClean, isClean)
		}
		ageRows(t, time.Hour)
	}
	failed(false) // run 1 (sha2)
	failed(false) // run 2
	// The periodic clean sync is due, and a rule is written just before it starts
	updateItem(t, "Machine#"+machineA, "SyncState", map[string]any{"LastCleanSync": clock.RFC3339(time.Now().Add(-30 * 24 * time.Hour))})
	addGlobalRule(t, sha3, types.RuleTypeBinary, types.RulePolicyBlocklist)
	ageRows(t, time.Hour)
	failed(true) // run 3, first serves sha3
	for i := 0; i < 4; i++ {
		failed(false)
	}
	for i := 0; i < 3; i++ {
		host.MustSync(t)
		ageRows(t, time.Hour)
	}
	if got := host.Policy("BINARY", sha3); got != "BLOCKLIST" {
		t.Errorf("rule first served by the clean sync lost after 6 failed syncs in a row: %q", got)
	}
}

func TestReview_R5_RuleSurvivesEightFailedSyncsWithoutACleanSync(t *testing.T) {
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
	addGlobalRule(t, sha2, types.RuleTypeBinary, types.RulePolicyBlocklist)
	ageRows(t, time.Hour)
	failed := func(wantClean bool) {
		t.Helper()
		lose["ruledownload"] = 1
		if _, err := host.Sync(t); err == nil {
			t.Fatal("precondition: the sync fails")
		}
		clean := getItem(t, "Machine#"+machineA, "SyncState")["CleanSync"]
		isClean := clean != nil && clean.(*awstypes.AttributeValueMemberBOOL).Value
		if isClean != wantClean {
			t.Fatalf("precondition: clean sync %v, got %v", wantClean, isClean)
		}
		ageRows(t, time.Hour)
	}
	failed(false) // run 1 (sha2)
	failed(false) // run 2
	// The periodic clean sync is due, and a rule is written just before it starts
	addGlobalRule(t, sha3, types.RuleTypeBinary, types.RulePolicyBlocklist)
	ageRows(t, time.Hour)
	failed(false) // run 3, first serves sha3
	for i := 0; i < 5; i++ {
		failed(false)
	}
	for i := 0; i < 3; i++ {
		host.MustSync(t)
		ageRows(t, time.Hour)
	}
	if got := host.Policy("BINARY", sha3); got != "BLOCKLIST" {
		t.Errorf("rule first served by the clean sync lost after 6 failed syncs in a row: %q", got)
	}
}
