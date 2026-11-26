//go:build integration

package fleet_test

// Rules hash outliers per set of groups. MachineRulesTracked (machinerules.DeliveryDigest) is a digest of the sort
// keys of the machine and group rules a Mac is sent, not of their policies: two groups with a rule for the same
// identifier but different policies give their Macs the same digest and different hashes. So the job compares a Mac
// only with Macs that have the same digest and the same effective groups (groups.Effective, as for its readiness
// scopes). Within each such set the rules are as before: at least three Macs, the expected hash is the one more than
// half report, NotApplied Macs are not listed twice.
//
// A group rule edited in place writes no rules feed row and does not change the digest, so each change to a group's
// rules is stamped on its registry entry as RulesChangedAt. A set's change time is the latest of the newest RulesFeed
// stamp and the RulesChangedAt of each of its groups; only Macs whose postflight is more than
// syncstate.FeedSyncCursorOverlap after it take part. A group without RulesChangedAt (registered before the stamp
// existed, rules unchanged since) adds no change time.

import (
	"testing"
	"time"

	"github.com/airbnb/rudolph/pkg/model/syncstate"
)

// stampedGroup is a registry entry with, when changed is not zero, the time its rules last changed.
type stampedGroup struct {
	name    string
	changed time.Time
}

// stampedRegistry writes the group registry as Rudolph stores it, with RulesChangedAt where given.
func stampedRegistry(t *testing.T, groups ...stampedGroup) {
	t.Helper()
	entries := map[string]any{}
	for i, g := range groups {
		e := map[string]any{"Description": g.name, "AcceptReported": false, "RulesVersion": i + 1, "CreatedAt": rfc(now.AddDate(0, -1, 0))}
		if !g.changed.IsZero() {
			e["RulesChangedAt"] = rfc(g.changed)
		}
		entries[g.name] = e
	}
	putStore(t, map[string]any{"PK": "GroupRegistry", "SK": "Current", "DataType": "GroupRegistry", "Groups": entries,
		"MembershipVersion": 1, "RulesVersionCounter": len(groups)})
}

// groupRuleWithPolicy stores a group rule on binary n with the given policy.
func groupRuleWithPolicy(t *testing.T, group string, n, policy int) {
	t.Helper()
	putStore(t, map[string]any{"PK": "GroupRules#" + group, "SK": "Binary#" + sha(n), "RuleType": 1, "Policy": policy,
		"Identifier": sha(n), "Group": group, "DataType": "GroupRule", "Description": "fleet test"})
}

// inGroup adds a Mac assigned to group.
func (h *hashMacs) inGroup(group, digest, hash string, postflight time.Time) mac {
	m := h.add(digest, hash, postflight)
	h.macs[len(h.macs)-1].assigned = []string{group}
	m.assigned = []string{group}
	return m
}

// twoGroupsOneIdentifier seeds groups A and B, each with a rule for binary 1 (A allows it, B blocks it), so that
// their Macs share a digest but not a hash. A's five Macs report HA, B's three HB; all synced two hours ago, with no
// rule change since.
func twoGroupsOneIdentifier(t *testing.T) *hashMacs {
	t.Helper()
	reset(t)
	stampedRegistry(t, stampedGroup{name: "A"}, stampedGroup{name: "B"})
	groupRuleWithPolicy(t, "A", 1, 1)
	groupRuleWithPolicy(t, "B", 1, 2)
	base := now.Add(-2 * time.Hour)
	h := &hashMacs{t: t}
	for i := 0; i < 5; i++ {
		h.inGroup("A", "d1", "HA", base.Add(time.Duration(i)*time.Minute))
	}
	for i := 0; i < 3; i++ {
		h.inGroup("B", "d1", "HB", base.Add(time.Duration(10+i)*time.Minute))
	}
	return h
}

// Same digest, different groups, different policies: the smaller group's Macs are not outliers of the larger one.
func TestFleet_OutliersCompareOnlyMacsInTheSameGroups(t *testing.T) {
	h := twoGroupsOneIdentifier(t)
	h.seed()

	if got, _ := outliersOfRun(t); len(got) != 0 {
		t.Errorf("outliers %v; want none: group B's Macs have the same digest as A's but a different rule policy, so a different hash", sortedKeys(got))
	}
}

// Within group B's set, the odd one out is listed against B's majority, not A's.
func TestFleet_OutlierWithinItsOwnGroups(t *testing.T) {
	h := twoGroupsOneIdentifier(t)
	odd := h.inGroup("B", "d1", "HX", now.Add(-90*time.Minute))
	h.seed()

	got, _ := outliersOfRun(t)
	if len(got) != 1 || got[odd.id] == nil {
		t.Fatalf("outliers %v, want only %s (the one Mac of group B whose hash differs from B's)", sortedKeys(got), odd.id)
	}
	if got[odd.id]["Hash"] != "HX" || got[odd.id]["ExpectedHash"] != "HB" {
		t.Errorf("outlier Hash %v ExpectedHash %v, want HX and HB (the hash most of group B's Macs report)", got[odd.id]["Hash"], got[odd.id]["ExpectedHash"])
	}
}

// groupChange is when group G's rule was edited in place: four hours before the run.
var groupChange = now.Add(-4 * time.Hour)

// groupRuleEdited seeds group G with RulesChangedAt groupChange and only an older rules feed row: four Macs synced
// before the edit and report the old hash, three synced since and report the new one.
func groupRuleEdited(t *testing.T) (*hashMacs, time.Time) {
	t.Helper()
	reset(t)
	stampedRegistry(t, stampedGroup{name: "G", changed: groupChange})
	groupRuleWithPolicy(t, "G", 1, 2)
	feedRow(t, groupChange.AddDate(0, 0, -3), 5)
	after := groupChange.Add(syncstate.FeedSyncCursorOverlap + time.Minute)
	h := &hashMacs{t: t}
	for i := 0; i < 4; i++ {
		h.inGroup("G", "d1", "old", groupChange.Add(-3*time.Hour+time.Duration(i)*time.Minute))
	}
	for i := 0; i < 3; i++ {
		h.inGroup("G", "d1", "new", after.Add(time.Duration(i)*20*time.Minute))
	}
	return h, after
}

// The reported case for group rules: a group rule's policy changed late in the day, most of the group's Macs have
// not synced since. The Macs that have are not outliers.
func TestFleet_OutliersIgnoreMacsThatHaveNotSyncedSinceTheGroupRuleChange(t *testing.T) {
	h, _ := groupRuleEdited(t)
	h.seed()

	if got, _ := outliersOfRun(t); len(got) != 0 {
		t.Errorf("outliers %v after a group rule edit that most of the group's Macs have not synced since; want none (RulesChangedAt counts as a rule change)", sortedKeys(got))
	}
}

// Among the Macs synced since the group rule change, the odd one out is listed, with the new hash expected.
func TestFleet_OutlierAmongMacsSyncedSinceTheGroupRuleChange(t *testing.T) {
	h, after := groupRuleEdited(t)
	odd := h.inGroup("G", "d1", "third", after.Add(5*time.Minute))
	h.seed()

	got, _ := outliersOfRun(t)
	if len(got) != 1 || got[odd.id] == nil {
		t.Fatalf("outliers %v, want only %s (the one Mac synced since the group rule change whose hash differs)", sortedKeys(got), odd.id)
	}
	if got[odd.id]["Hash"] != "third" || got[odd.id]["ExpectedHash"] != "new" {
		t.Errorf("outlier Hash %v ExpectedHash %v, want third and new", got[odd.id]["Hash"], got[odd.id]["ExpectedHash"])
	}
}

// A group's rule change only moves the change time of the sets that include the group: Macs of another group, all
// synced before it, are still compared.
func TestFleet_OtherGroupsRuleChangesDoNotHideOutliers(t *testing.T) {
	reset(t)
	stampedRegistry(t, stampedGroup{name: "G", changed: now.Add(-time.Hour)}, stampedGroup{name: "K"})
	base := now.Add(-2 * time.Hour)
	h := &hashMacs{t: t}
	for i := 0; i < 3; i++ {
		h.inGroup("K", "d2", "k1", base.Add(time.Duration(i)*time.Minute))
	}
	odd := h.inGroup("K", "d2", "k2", base.Add(5*time.Minute))
	h.seed()

	got, _ := outliersOfRun(t)
	if len(got) != 1 || got[odd.id] == nil {
		t.Fatalf("outliers %v, want only %s (group G's rule change an hour ago says nothing about group K's Macs)", sortedKeys(got), odd.id)
	}
	if got[odd.id]["ExpectedHash"] != "k1" {
		t.Errorf("ExpectedHash %v, want k1", got[odd.id]["ExpectedHash"])
	}
}
