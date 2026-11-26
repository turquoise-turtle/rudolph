//go:build integration

package groups_test

// When a group's rules last changed. Editing a group rule in place (a new policy, say) writes no rules feed row and
// leaves the MachineRulesTracked digest as it is (the digest covers rule sort keys only), so the fleet job cannot tell
// from either which Macs have synced since. Every change to a group's rules therefore records its time on the
// group's registry entry, as groups.Entry.RulesChangedAt (attribute `RulesChangedAt` of `Groups.<name>`, an RFC 3339
// UTC string like CreatedAt), in the same transaction that bumps RulesVersion: PutRule (adding or replacing) and
// RemoveRule. Registry changes that are not rule changes (description, reported membership, client mode, file access
// override, assigned or reported membership, collection of allowed events) leave it as it is.
//
// PutRule and RemoveRule take no clock (rudolph-web calls them as they are), so the stamp is the wall-clock time of
// the write: these tests check it lies between just before and just after the call.

import (
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/model/groups"
	"github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/types"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// rulesChangedAt returns the raw RulesChangedAt of group's registry entry, and whether it is there.
func rulesChangedAt(t *testing.T, group string) (string, bool) {
	t.Helper()
	all, ok := rawItem(t, "GroupRegistry", "Current")["Groups"].(*awstypes.AttributeValueMemberM)
	if !ok {
		t.Fatal("the registry has no Groups map")
	}
	entry, ok := all.Value[group].(*awstypes.AttributeValueMemberM)
	if !ok {
		t.Fatalf("the registry has no entry for %s", group)
	}
	v, ok := entry.Value["RulesChangedAt"].(*awstypes.AttributeValueMemberS)
	if !ok {
		return "", false
	}
	return v.Value, true
}

// stampedDuring runs write and fails unless it left group's RulesChangedAt at a time during the call.
func stampedDuring(t *testing.T, group, what string, write func() error) string {
	t.Helper()
	// RFC 3339 as stored has whole seconds.
	before := time.Now().UTC().Truncate(time.Second)
	if err := write(); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	after := time.Now().UTC()
	raw, ok := rulesChangedAt(t, group)
	if !ok {
		t.Fatalf("%s left %s's registry entry without RulesChangedAt", what, group)
	}
	at, err := clock.ParseRFC3339(raw)
	if err != nil {
		t.Fatalf("%s's RulesChangedAt %q is not RFC 3339: %v", group, raw, err)
	}
	if at.Location() != time.UTC {
		t.Errorf("%s's RulesChangedAt %q is not UTC", group, raw)
	}
	if at.Before(before) || at.After(after) {
		t.Errorf("after %s, %s's RulesChangedAt is %s, want the time of the write (between %s and %s)", what, group, raw, clock.RFC3339(before), clock.RFC3339(after))
	}
	return raw
}

// nextSecond waits until the stored, whole-second stamp of a write would differ from one made before it.
func nextSecond() { time.Sleep(1100 * time.Millisecond) }

func TestGroupRuleChangesStampRulesChangedAt(t *testing.T) {
	ddblocal.Reset(t)
	w := writer(t)
	for _, g := range []string{"eng", "lab"} {
		if err := groups.CreateGroup(clock.ConcreteTimeProvider{}, w, g, "test", true); err != nil {
			t.Fatal(err)
		}
	}
	rule := rules.SantaRule{RuleType: types.RuleTypeTeamID, Policy: types.RulePolicyAllowlist, Identifier: otherTeam}

	labStamp := stampedDuring(t, "lab", "adding a rule to lab", func() error { return groups.PutRule(w, "lab", rule, "test") })
	nextSecond()

	first := stampedDuring(t, "eng", "adding a rule to eng", func() error { return groups.PutRule(w, "eng", rule, "test") })
	nextSecond()

	// Replacing the rule in place: same sort key, so the digest Macs report does not change, but the policy does.
	blocked := rule
	blocked.Policy = types.RulePolicyBlocklist
	second := stampedDuring(t, "eng", "replacing eng's rule with a block", func() error { return groups.PutRule(w, "eng", blocked, "test") })
	if second == first {
		t.Errorf("replacing eng's rule left RulesChangedAt at %s, the time it was added", first)
	}
	nextSecond()

	third := stampedDuring(t, "eng", "removing eng's rule", func() error { return groups.RemoveRule(w, "eng", teamKey) })
	if third == second {
		t.Errorf("removing eng's rule left RulesChangedAt at %s, the time it was replaced", second)
	}

	if got, _ := rulesChangedAt(t, "lab"); got != labStamp {
		t.Errorf("changing eng's rules moved lab's RulesChangedAt from %s to %q", labStamp, got)
	}
}

func TestRegistryChangesThatAreNotRuleChangesKeepRulesChangedAt(t *testing.T) {
	ddblocal.Reset(t)
	w := writer(t)
	tp := clock.ConcreteTimeProvider{}
	if err := groups.CreateGroup(tp, w, "eng", "test", false); err != nil {
		t.Fatal(err)
	}
	rule := rules.SantaRule{RuleType: types.RuleTypeTeamID, Policy: types.RulePolicyAllowlist, Identifier: otherTeam}
	stamp := stampedDuring(t, "eng", "adding a rule to eng", func() error { return groups.PutRule(w, "eng", rule, "test") })
	nextSecond()

	mode := types.Lockdown
	action := types.FileAccessActionAuditOnly
	description := "changed"
	accept := true
	machine := "AAAAAAAA-1111-4111-8111-AAAAAAAAAAAA"
	for _, change := range []struct {
		what  string
		write func() error
	}{
		{"setting the client mode", func() error { return groups.SetClientMode(w, "eng", &mode) }},
		{"removing the client mode", func() error { return groups.SetClientMode(w, "eng", nil) }},
		{"setting the file access override", func() error { return groups.SetFileAccessOverride(w, "eng", &action) }},
		{"removing the file access override", func() error { return groups.SetFileAccessOverride(w, "eng", nil) }},
		{"changing the description and reported membership", func() error { return groups.UpdateGroup(w, "eng", &description, &accept) }},
		{"assigning a Mac", func() error { return groups.AddAssigned(tp, w, machine, "eng") }},
		{"reporting membership", func() error { return groups.SetReported(tp, w, machine, []string{"eng"}) }},
		{"unassigning a Mac", func() error { return groups.RemoveAssigned(tp, w, machine, "eng") }},
		{"collecting the rule's allowed events", func() error { return groups.SetCollectAllowedEvents(w, "eng", teamKey, true) }},
		{"no longer collecting them", func() error { return groups.SetCollectAllowedEvents(w, "eng", teamKey, false) }},
	} {
		if err := change.write(); err != nil {
			t.Fatalf("%s: %v", change.what, err)
		}
		if got, ok := rulesChangedAt(t, "eng"); !ok || got != stamp {
			t.Errorf("%s changed eng's RulesChangedAt from %s to %q; only rule changes may", change.what, stamp, got)
		}
	}
}
