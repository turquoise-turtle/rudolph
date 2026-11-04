//go:build integration

package handlers

import (
	"testing"
	"time"

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
