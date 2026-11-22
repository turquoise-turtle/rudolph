//go:build integration

package group

// Collection of allowed executions for group rules (owner approved 2026-11-23):
//
//	rudolph rule update -t <type> -i <identifier> (--group <name> | --ungrouped) --collect-allowed-events=true|false
//
// sets or clears CollectAllowedEvents on the group rule row (`GroupRules#<group>` / `<rule sort key>`) and keeps the
// collection index (`EventCollection` / `Rules`, string set `Group#<group>`) in step in the same transaction, as for
// global and machine rules. The group name is normalised as for every --group. It is audited (rule.update, target
// group:<name> or ungrouped, summary ending "collect allowed events true|false"), shows CollectAllowedEvents, and warns
// when EnableAllEventUpload is off in the global configuration. Refused, writing nothing: a group that does not exist,
// a rule the group does not have, flagging a rule that is not an allow (ALLOWLIST or ALLOWLIST_COMPILER). Every other
// change of a group rule is still made by writing the rule again ("rule <policy> ... --group"), so -p, --expires and the
// message flags stay refused with --group. Removing the rule, writing it again, or deleting the group with --force
// takes it out of the index.

import (
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	modelaudit "github.com/airbnb/rudolph/pkg/model/audit"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	collectTeam    = "ABCDE12345"
	collectTeamKey = "TeamID#" + collectTeam
)

func groupIndexSet(t *testing.T, group string) []string {
	t.Helper()
	set, ok := item(t, "EventCollection", "Rules")["Group#"+group].(*awstypes.AttributeValueMemberSS)
	if !ok {
		return nil
	}
	return set.Value
}

func groupRuleFlagged(t *testing.T, group, sk string) bool {
	t.Helper()
	b, _ := item(t, "GroupRules#"+group, sk)["CollectAllowedEvents"].(*awstypes.AttributeValueMemberBOOL)
	return b != nil && b.Value
}

func inSet(set []string, v string) bool {
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

func mustRule(t *testing.T, args ...string) string {
	t.Helper()
	out, err := ruleCmd(t, args...)
	if err != nil {
		t.Fatalf("rule %v: %v\n%s", args, err, out)
	}
	return out
}

func auditEntries(t *testing.T, action string) []modelaudit.Entry {
	t.Helper()
	all, err := modelaudit.List(ddblocal.Client(t), time.Now().Add(-time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	var out []modelaudit.Entry
	for _, e := range all {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

func TestGroupCollectCLI_FlagsAndClearsAGroupRule(t *testing.T) {
	ddblocal.Reset(t)
	mustGroup(t, "create", "eng")
	mustRule(t, "allow", "-t", "teamid", "-i", collectTeam, "--group", "eng")

	out := mustRule(t, "update", "-t", "teamid", "-i", collectTeam, "--group", "Eng", "--collect-allowed-events=true")
	if !groupRuleFlagged(t, "eng", collectTeamKey) {
		t.Error("the group rule row is not flagged CollectAllowedEvents")
	}
	if !inSet(groupIndexSet(t, "eng"), collectTeamKey) {
		t.Errorf("the index's Group#eng set %v does not hold %s", groupIndexSet(t, "eng"), collectTeamKey)
	}
	if !strings.Contains(out, "CollectAllowedEvents") {
		t.Errorf("the update does not show CollectAllowedEvents:\n%s", out)
	}
	if !strings.Contains(out, "EnableAllEventUpload") && !strings.Contains(strings.ToLower(out), "enable-all-event-upload") {
		t.Errorf("no warning that EnableAllEventUpload is off:\n%s", out)
	}
	updates := auditEntries(t, "rule.update")
	if len(updates) != 1 || updates[0].Target != "group:eng" || !strings.HasSuffix(updates[0].Summary, "collect allowed events true") {
		t.Errorf("audit entries for rule.update: %+v, want one for group:eng ending \"collect allowed events true\"", updates)
	}

	mustRule(t, "update", "-t", "teamid", "-i", collectTeam, "--group", "eng", "--collect-allowed-events=false")
	if groupRuleFlagged(t, "eng", collectTeamKey) {
		t.Error("the flag was not cleared")
	}
	if inSet(groupIndexSet(t, "eng"), collectTeamKey) {
		t.Error("the cleared rule is still in the index")
	}
	updates = auditEntries(t, "rule.update")
	if len(updates) != 2 || !strings.HasSuffix(updates[0].Summary, "collect allowed events false") {
		t.Errorf("audit entries for rule.update: %+v, want a second, newest first, ending \"collect allowed events false\"", updates)
	}

	// Already as given: nothing to do, and not an error (a command copied from rudolph-web may run twice).
	if out, err := ruleCmd(t, "update", "-t", "teamid", "-i", collectTeam, "--group", "eng", "--collect-allowed-events=false"); err != nil {
		t.Errorf("clearing a flag that is not set failed: %v\n%s", err, out)
	}
}

func TestGroupCollectCLI_Ungrouped(t *testing.T) {
	ddblocal.Reset(t)
	mustRule(t, "allow", "-t", "teamid", "-i", collectTeam, "--ungrouped")
	mustRule(t, "update", "-t", "teamid", "-i", collectTeam, "--ungrouped", "--collect-allowed-events=true")
	if !groupRuleFlagged(t, "rudolph:ungrouped", collectTeamKey) {
		t.Error("the ungrouped rule row is not flagged")
	}
	if !inSet(groupIndexSet(t, "rudolph:ungrouped"), collectTeamKey) {
		t.Errorf("Group#rudolph:ungrouped = %v", groupIndexSet(t, "rudolph:ungrouped"))
	}
	updates := auditEntries(t, "rule.update")
	if len(updates) != 1 || updates[0].Target != "ungrouped" {
		t.Errorf("audit entries for rule.update: %+v, want one for target ungrouped", updates)
	}
}

func TestGroupCollectCLI_AllowCompilerCanBeFlagged(t *testing.T) {
	ddblocal.Reset(t)
	mustGroup(t, "create", "eng")
	mustRule(t, "compiler", "-t", "signingid", "-i", collectTeam+":com.example.cc", "--group", "eng")
	mustRule(t, "update", "-t", "signingid", "-i", collectTeam+":com.example.cc", "--group", "eng", "--collect-allowed-events=true")
	if !groupRuleFlagged(t, "eng", "SigningID#"+collectTeam+":com.example.cc") {
		t.Error("an ALLOWLIST_COMPILER group rule could not be flagged")
	}
}

func TestGroupCollectCLI_Refusals(t *testing.T) {
	ddblocal.Reset(t)
	mustGroup(t, "create", "eng")
	mustRule(t, "deny", "-t", "teamid", "-i", collectTeam, "--group", "eng")
	mustRule(t, "allow", "-t", "teamid", "-i", "ALLOW00001", "--group", "eng")
	before := item(t, "GroupRules#eng", collectTeamKey)

	for name, args := range map[string][]string{
		"rule the group does not have": {"update", "-t", "teamid", "-i", "ZZZZZ99999", "--group", "eng", "--collect-allowed-events=true"},
		"group that does not exist":    {"update", "-t", "teamid", "-i", collectTeam, "--group", "nope", "--collect-allowed-events=true"},
		"a block cannot be flagged":    {"update", "-t", "teamid", "-i", collectTeam, "--group", "eng", "--collect-allowed-events=true"},
		"policy of a group rule":       {"update", "-t", "teamid", "-i", collectTeam, "--group", "eng", "-p", "allowlist"},
		"policy and collection":        {"update", "-t", "teamid", "-i", "ALLOW00001", "--group", "eng", "-p", "blocklist", "--collect-allowed-events=true"},
		"message and collection":       {"update", "-t", "teamid", "-i", "ALLOW00001", "--group", "eng", "--custom-msg", "hi", "--collect-allowed-events=true"},
		"expiry of a group rule":       {"update", "-t", "teamid", "-i", "ALLOW00001", "--group", "eng", "--expires", "24h", "--collect-allowed-events=true"},
	} {
		t.Run(name, func(t *testing.T) {
			if out, err := ruleCmd(t, args...); err == nil {
				t.Errorf("accepted:\n%s", out)
			}
		})
	}
	if groupRuleFlagged(t, "eng", collectTeamKey) || groupRuleFlagged(t, "eng", "TeamID#ALLOW00001") {
		t.Error("a refused update flagged a group rule")
	}
	if p, _ := item(t, "GroupRules#eng", collectTeamKey)["Policy"].(*awstypes.AttributeValueMemberN); p == nil || p.Value != before["Policy"].(*awstypes.AttributeValueMemberN).Value {
		t.Errorf("a refused update changed the group rule's policy to %v", p)
	}
	if got := item(t, "EventCollection", "Rules"); len(got) != 0 {
		t.Errorf("refused updates wrote the collection index: %v", got)
	}
	if got := item(t, "GroupRules#nope", collectTeamKey); len(got) != 0 {
		t.Errorf("a refused update created a rule row: %v", got)
	}
	if updates := auditEntries(t, "rule.update"); len(updates) != 0 {
		t.Errorf("refused updates were audited: %+v", updates)
	}
}

func TestGroupCollectCLI_DryRunWritesNothing(t *testing.T) {
	ddblocal.Reset(t)
	mustGroup(t, "create", "eng")
	mustRule(t, "allow", "-t", "teamid", "-i", collectTeam, "--group", "eng")
	out, err := ruleCmd(t, "update", "-t", "teamid", "-i", collectTeam, "--group", "eng", "--collect-allowed-events=true", "--dry-run")
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	if groupRuleFlagged(t, "eng", collectTeamKey) || len(item(t, "EventCollection", "Rules")) != 0 {
		t.Error("the dry run flagged the rule")
	}
}

func TestGroupCollectCLI_RemovingRecreatingOrDeletingEndsCollection(t *testing.T) {
	ddblocal.Reset(t)
	mustGroup(t, "create", "eng")
	mustGroup(t, "create", "lab")
	flag := func(group, team string) {
		t.Helper()
		mustRule(t, "allow", "-t", "teamid", "-i", team, "--group", group)
		mustRule(t, "update", "-t", "teamid", "-i", team, "--group", group, "--collect-allowed-events=true")
		if !inSet(groupIndexSet(t, group), "TeamID#"+team) {
			t.Fatalf("flagging %s of %s did not reach the index", team, group)
		}
	}

	flag("eng", collectTeam)
	mustRule(t, "remove", "-t", "teamid", "-i", collectTeam, "--group", "eng")
	if inSet(groupIndexSet(t, "eng"), collectTeamKey) {
		t.Error("the removed group rule is still in the index")
	}

	flag("eng", collectTeam)
	mustRule(t, "allow", "-t", "teamid", "-i", collectTeam, "--group", "eng", "--custom-msg", "again")
	if groupRuleFlagged(t, "eng", collectTeamKey) {
		t.Error("the re-created group rule kept the flag")
	}
	if inSet(groupIndexSet(t, "eng"), collectTeamKey) {
		t.Error("the re-created group rule is still in the index")
	}

	// Re-created as a block: no longer an allow, and out of the index.
	flag("eng", collectTeam)
	mustRule(t, "deny", "-t", "teamid", "-i", collectTeam, "--group", "eng")
	if inSet(groupIndexSet(t, "eng"), collectTeamKey) {
		t.Error("the group rule re-created as a block is still in the index")
	}

	flag("eng", "OTHER00001")
	flag("lab", collectTeam)
	mustGroup(t, "delete", "eng", "--force")
	if got := groupIndexSet(t, "eng"); len(got) != 0 {
		t.Errorf("Group#eng = %v after the group was deleted", got)
	}
	if !inSet(groupIndexSet(t, "lab"), collectTeamKey) {
		t.Error("deleting eng took lab's flagged rule out of the index")
	}
}
