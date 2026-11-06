//go:build integration

package handlers

// Device groups (docs/groups-design.md; owner decisions 2026-11-06). These tests set groups up by writing the rows
// the design specifies, the contract the CLI writes and rudolph-web reads (the CLI has its own tests in
// internal/cli/group):
//
//   - registry `GroupRegistry` / `Current`: `Groups`, a map from group name to {Description, AcceptReported (bool),
//     RulesVersion (number), CreatedAt}; `MembershipVersion` (number); DataType "GroupRegistry";
//   - group rule `GroupRules#<group>` / `<rule sort key>` (rules.RuleSortKeyFromTypeIdentifier): RuleType, Policy,
//     Identifier, CustomMessage, … as on global rule rows, plus Description, Group, DataType "GroupRule"; a change to a
//     group's rules bumps its RulesVersion in the registry;
//   - assigned membership `Machine#<id>` / `Groups`: `Assigned`, a string set.
//
// A Mac's groups are the registered groups among those Santa reports (primary_user_groups, normalised: trimmed, lower
// case; only groups with AcceptReported) together with its assigned groups; a Mac in none is in the reserved group
// "rudolph:ungrouped". For each rule type and identifier, a machine rule beats a group rule, which beats the global
// rule; between groups of one Mac, BLOCKLIST > SILENT_TTY_BLOCKLIST > SILENT_GUI_BLOCKLIST > SILENT_BLOCKLIST > CEL >
// ALLOWLIST > ALLOWLIST_COMPILER, then the group name that sorts first. When a group rule stops applying to a Mac, the
// Mac gets the global rule back, or a REMOVE. The server may cache the registry and group rules for up to 60 s; the
// tests start a fresh server instance (newHandlers) after changing rows.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/internal/testutil/fakesanta"
	"github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	geminiSigningID = "EQHXZ8M8AV:com.google.GeminiMacOS"
	ungrouped       = "rudolph:ungrouped"
)

// registerGroups writes the registry with these groups (name → AcceptReported), every RulesVersion 1.
func registerGroups(t *testing.T, groups map[string]bool) {
	t.Helper()
	m := map[string]any{}
	for name, accept := range groups {
		m[name] = map[string]any{"Description": "test group " + name, "AcceptReported": accept, "RulesVersion": 1, "CreatedAt": "2026-11-06T00:00:00Z"}
	}
	putItem(t, map[string]any{"PK": "GroupRegistry", "SK": "Current", "Groups": m, "MembershipVersion": 1, "DataType": "GroupRegistry"})
	handlers = newHandlers()
}

func bumpGroup(t *testing.T, group string) {
	t.Helper()
	_, err := ddblocal.Raw(t).UpdateItem(context.Background(), &awsdynamodb.UpdateItemInput{
		TableName:                 aws.String(ddblocal.TableName),
		Key:                       map[string]awstypes.AttributeValue{"PK": &awstypes.AttributeValueMemberS{Value: "GroupRegistry"}, "SK": &awstypes.AttributeValueMemberS{Value: "Current"}},
		UpdateExpression:          aws.String("SET #g.#n.RulesVersion = #g.#n.RulesVersion + :one ADD MembershipVersion :one"),
		ExpressionAttributeNames:  map[string]string{"#g": "Groups", "#n": group},
		ExpressionAttributeValues: map[string]awstypes.AttributeValue{":one": &awstypes.AttributeValueMemberN{Value: "1"}},
	})
	if err != nil {
		t.Fatalf("bump %s: %v", group, err)
	}
	handlers = newHandlers()
}

func putGroupRule(t *testing.T, group, identifier string, ruleType types.RuleType, policy types.Policy, customMsg string) {
	t.Helper()
	row := map[string]any{
		"PK": "GroupRules#" + group, "SK": rules.RuleSortKeyFromTypeIdentifier(identifier, ruleType),
		"RuleType": int(ruleType), "Policy": int(policy), "Identifier": identifier, "Group": group,
		"Description": "test", "DataType": "GroupRule",
	}
	if customMsg != "" {
		row["CustomMessage"] = customMsg
	}
	putItem(t, row)
	bumpGroup(t, group)
}

func deleteGroupRule(t *testing.T, group, identifier string, ruleType types.RuleType) {
	t.Helper()
	_, err := ddblocal.Raw(t).DeleteItem(context.Background(), &awsdynamodb.DeleteItemInput{
		TableName: aws.String(ddblocal.TableName),
		Key: map[string]awstypes.AttributeValue{
			"PK": &awstypes.AttributeValueMemberS{Value: "GroupRules#" + group},
			"SK": &awstypes.AttributeValueMemberS{Value: rules.RuleSortKeyFromTypeIdentifier(identifier, ruleType)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	bumpGroup(t, group)
}

// assignGroups sets the machine's assigned groups (none removes the attribute).
func assignGroups(t *testing.T, machineID string, groups ...string) {
	t.Helper()
	in := &awsdynamodb.UpdateItemInput{
		TableName: aws.String(ddblocal.TableName),
		Key:       map[string]awstypes.AttributeValue{"PK": &awstypes.AttributeValueMemberS{Value: "Machine#" + machineID}, "SK": &awstypes.AttributeValueMemberS{Value: "Groups"}},
	}
	if len(groups) == 0 {
		in.UpdateExpression = aws.String("REMOVE Assigned")
	} else {
		in.UpdateExpression = aws.String("SET Assigned = :g")
		in.ExpressionAttributeValues = map[string]awstypes.AttributeValue{":g": &awstypes.AttributeValueMemberSS{Value: groups}}
	}
	if _, err := ddblocal.Raw(t).UpdateItem(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	// Membership changes bump MembershipVersion in the registry.
	_, err := ddblocal.Raw(t).UpdateItem(context.Background(), &awsdynamodb.UpdateItemInput{
		TableName:                 aws.String(ddblocal.TableName),
		Key:                       map[string]awstypes.AttributeValue{"PK": &awstypes.AttributeValueMemberS{Value: "GroupRegistry"}, "SK": &awstypes.AttributeValueMemberS{Value: "Current"}},
		UpdateExpression:          aws.String("ADD MembershipVersion :one"),
		ExpressionAttributeValues: map[string]awstypes.AttributeValue{":one": &awstypes.AttributeValueMemberN{Value: "1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	handlers = newHandlers()
}

// groupsServer: a fleet with one global rule (so syncs become incremental) and Google's team ID allowed globally.
func groupsServer(t *testing.T) fakesanta.Transport {
	t.Helper()
	transport := freshServer(t)
	addGlobalRule(t, strings.Repeat("e5", 32), types.RuleTypeBinary, types.RulePolicyBlocklist)
	addGlobalRule(t, teamGoogle, types.RuleTypeTeamID, types.RulePolicyAllowlist)
	return transport
}

func syncNormal(t *testing.T, h *fakesanta.Host) *fakesanta.SyncResult {
	t.Helper()
	res := h.MustSync(t)
	if res.SyncType != "NORMAL" {
		// One more sync, e.g. after the first (clean) sync.
		res = h.MustSync(t)
	}
	return res
}

// The owner's use case: Google's team ID is allowed for everyone, but one group must not run Gemini.
func TestGroups_ReportedGroupBlocksASigningIDUnderAnAllowedTeamID(t *testing.T) {
	transport := groupsServer(t)
	registerGroups(t, map[string]bool{"eng": true})
	putGroupRule(t, "eng", geminiSigningID, types.RuleTypeSigningID, types.RulePolicyBlocklist, "Gemini is not allowed for Eng")

	eng := newHost(t, machineA, transport)
	eng.PrimaryUserGroups = []string{" Eng ", "staff"} // reported as Santa sends it; normalised by Rudolph
	other := newHost(t, machineB, transport)
	other.PrimaryUserGroups = []string{"staff"}
	syncNormal(t, eng)
	syncNormal(t, other)

	if eng.Policy("SIGNINGID", geminiSigningID) != "BLOCKLIST" || eng.Policy("TEAMID", teamGoogle) != "ALLOWLIST" {
		t.Errorf("Eng Mac: Gemini %q, Google team %q; want BLOCKLIST and ALLOWLIST", eng.Policy("SIGNINGID", geminiSigningID), eng.Policy("TEAMID", teamGoogle))
	}
	if eng.Rules[fakesanta.RuleKey{Type: "SIGNINGID", Identifier: geminiSigningID}].CustomMsg != "Gemini is not allowed for Eng" {
		t.Error("the group rule's custom message did not reach the Mac")
	}
	if got := other.Policy("SIGNINGID", geminiSigningID); got != "" {
		t.Errorf("a Mac outside the group got the group rule: %q", got)
	}
}

func TestGroups_AssignedMembershipAndTheUnionWithReported(t *testing.T) {
	transport := groupsServer(t)
	registerGroups(t, map[string]bool{"eng": true, "design": true})
	putGroupRule(t, "eng", sha1, types.RuleTypeBinary, types.RulePolicyBlocklist, "")
	putGroupRule(t, "design", sha2, types.RuleTypeBinary, types.RulePolicyBlocklist, "")
	h := newHost(t, machineA, transport)
	h.PrimaryUserGroups = []string{"eng"}
	assignGroups(t, machineA, "design")
	syncNormal(t, h)
	if h.Policy("BINARY", sha1) != "BLOCKLIST" || h.Policy("BINARY", sha2) != "BLOCKLIST" {
		t.Errorf("reported eng %q, assigned design %q; want both BLOCKLIST", h.Policy("BINARY", sha1), h.Policy("BINARY", sha2))
	}
}

// Only groups registered in Rudolph count, and reported membership only for groups that accept it.
func TestGroups_UnregisteredAndNonAcceptingGroupsDoNotCount(t *testing.T) {
	transport := groupsServer(t)
	registerGroups(t, map[string]bool{"admins": false})
	putGroupRule(t, "admins", sha1, types.RuleTypeBinary, types.RulePolicyAllowlist, "")
	h := newHost(t, machineA, transport)
	h.PrimaryUserGroups = []string{"admins", "unknown-directory-group", "rudolph:ungrouped", strings.Repeat("x", 200)}
	syncNormal(t, h)
	if got := h.Policy("BINARY", sha1); got != "" {
		t.Errorf("reporting a group that does not accept reported membership gained its rule: %q", got)
	}
	assignGroups(t, machineA, "admins")
	syncNormal(t, h)
	if got := h.Policy("BINARY", sha1); got != "ALLOWLIST" {
		t.Errorf("assigned to admins: %q, want ALLOWLIST", got)
	}
}

func TestGroups_UngroupedRulesAndJoiningAGroup(t *testing.T) {
	transport := groupsServer(t)
	registerGroups(t, map[string]bool{"eng": true, ungrouped: false})
	putGroupRule(t, ungrouped, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist, "")
	h := newHost(t, machineA, transport)
	h.PrimaryUserGroups = []string{"some-directory-group"} // not registered: still ungrouped
	syncNormal(t, h)
	if got := h.Policy("BINARY", sha1); got != "BLOCKLIST" {
		t.Fatalf("ungrouped Mac: %q, want the ungrouped BLOCKLIST rule", got)
	}
	h.PrimaryUserGroups = []string{"eng"}
	syncNormal(t, h)
	syncNormal(t, h)
	if got := h.Policy("BINARY", sha1); got != "" {
		t.Errorf("after joining eng the ungrouped rule is still %q; want it removed", got)
	}
}

// Leaving a group gives the Mac the global rule back (with its own fields), or removes the rule when there is none.
func TestGroups_LeavingAGroupRestoresTheGlobalRuleOrRemoves(t *testing.T) {
	transport := groupsServer(t)
	registerGroups(t, map[string]bool{"eng": true})
	putGroupRule(t, "eng", teamGoogle, types.RuleTypeTeamID, types.RulePolicyBlocklist, "no Google for eng")
	putGroupRule(t, "eng", sha1, types.RuleTypeBinary, types.RulePolicyBlocklist, "")
	h := newHost(t, machineA, transport)
	h.PrimaryUserGroups = []string{"eng"}
	syncNormal(t, h)
	if h.Policy("TEAMID", teamGoogle) != "BLOCKLIST" {
		t.Fatalf("in eng: Google team %q, want the group's BLOCKLIST over the global ALLOWLIST", h.Policy("TEAMID", teamGoogle))
	}
	h.PrimaryUserGroups = nil
	syncNormal(t, h)
	syncNormal(t, h)
	if got := h.Policy("TEAMID", teamGoogle); got != "ALLOWLIST" {
		t.Errorf("after leaving eng: Google team %q, want the global ALLOWLIST back", got)
	}
	if msg := h.Rules[fakesanta.RuleKey{Type: "TEAMID", Identifier: teamGoogle}].CustomMsg; msg != "" {
		t.Errorf("the restored global rule kept the group's custom message %q", msg)
	}
	if got := h.Policy("BINARY", sha1); got != "" {
		t.Errorf("after leaving eng: group-only rule still %q, want it removed", got)
	}
}

func TestGroups_ARuleStillProvidedElsewhereSurvivesLeavingAGroup(t *testing.T) {
	transport := groupsServer(t)
	registerGroups(t, map[string]bool{"eng": true, "design": true})
	putGroupRule(t, "eng", sha1, types.RuleTypeBinary, types.RulePolicyBlocklist, "")
	putGroupRule(t, "design", sha1, types.RuleTypeBinary, types.RulePolicyBlocklist, "")
	putGroupRule(t, "eng", sha2, types.RuleTypeBinary, types.RulePolicyBlocklist, "")
	addMachineRule(t, machineA, sha2, types.RuleTypeBinary, types.RulePolicyBlocklist, time.Now().Add(24*time.Hour))
	h := newHost(t, machineA, transport)
	h.PrimaryUserGroups = []string{"eng", "design"}
	syncNormal(t, h)
	h.PrimaryUserGroups = []string{"design"}
	syncNormal(t, h)
	syncNormal(t, h)
	if h.Policy("BINARY", sha1) != "BLOCKLIST" || h.Policy("BINARY", sha2) != "BLOCKLIST" {
		t.Errorf("after leaving eng: sha1 (also design) %q, sha2 (also a machine rule) %q; want both BLOCKLIST", h.Policy("BINARY", sha1), h.Policy("BINARY", sha2))
	}
}

func TestGroups_Precedence(t *testing.T) {
	for _, c := range []struct {
		name    string
		a, b    types.Policy // group "a-team" and group "b-team"
		machine types.Policy // 0: no machine rule
		global  types.Policy // 0: no global rule
		want    string
	}{
		{"block beats allow between groups", types.RulePolicyAllowlist, types.RulePolicyBlocklist, 0, 0, "BLOCKLIST"},
		{"blocklist beats silent blocklist", types.RulePolicySilentBlocklist, types.RulePolicyBlocklist, 0, 0, "BLOCKLIST"},
		{"CEL beats allowlist", types.RulePolicyAllowlist, types.RulePolicyCEL, 0, 0, "CEL"},
		{"allowlist beats compiler", types.RulePolicyAllowlistCompiler, types.RulePolicyAllowlist, 0, 0, "ALLOWLIST"},
		{"machine allow beats group block", types.RulePolicyBlocklist, types.RulePolicyBlocklist, types.RulePolicyAllowlist, 0, "ALLOWLIST"},
		{"group allow beats global block", types.RulePolicyAllowlist, types.RulePolicyAllowlist, 0, types.RulePolicyBlocklist, "ALLOWLIST"},
	} {
		t.Run(c.name, func(t *testing.T) {
			transport := groupsServer(t)
			registerGroups(t, map[string]bool{"a-team": true, "b-team": true})
			id := sha1
			for group, policy := range map[string]types.Policy{"a-team": c.a, "b-team": c.b} {
				row := map[string]any{
					"PK": "GroupRules#" + group, "SK": rules.RuleSortKeyFromTypeIdentifier(id, types.RuleTypeBinary),
					"RuleType": int(types.RuleTypeBinary), "Policy": int(policy), "Identifier": id, "Group": group, "DataType": "GroupRule",
				}
				if policy == types.RulePolicyCEL {
					row["CELExpr"] = "target.signing_time >= timestamp('2025-01-01T00:00:00Z')"
				}
				putItem(t, row)
				bumpGroup(t, group)
			}
			if c.global != 0 {
				addGlobalRule(t, id, types.RuleTypeBinary, c.global)
			}
			if c.machine != 0 {
				addMachineRule(t, machineA, id, types.RuleTypeBinary, c.machine, time.Now().Add(24*time.Hour))
			}
			h := newHost(t, machineA, transport)
			h.PrimaryUserGroups = []string{"a-team", "b-team"}
			syncNormal(t, h)
			syncNormal(t, h)
			if got := h.Policy("BINARY", id); got != c.want {
				t.Errorf("%s, want %s", got, c.want)
			}
		})
	}
}

// Group rule changes reach member Macs on incremental syncs, with no clean sync.
func TestGroups_RuleChangesReachMacsIncrementally(t *testing.T) {
	transport := groupsServer(t)
	registerGroups(t, map[string]bool{"eng": true})
	h := newHost(t, machineA, transport)
	h.PrimaryUserGroups = []string{"eng"}
	syncNormal(t, h)

	steps := []struct {
		name   string
		change func()
		policy string
		msg    string
	}{
		{"add", func() { putGroupRule(t, "eng", sha1, types.RuleTypeBinary, types.RulePolicyBlocklist, "") }, "BLOCKLIST", ""},
		{"edit the message only", func() { putGroupRule(t, "eng", sha1, types.RuleTypeBinary, types.RulePolicyBlocklist, "new message") }, "BLOCKLIST", "new message"},
		{"change the policy", func() { putGroupRule(t, "eng", sha1, types.RuleTypeBinary, types.RulePolicyAllowlist, "") }, "ALLOWLIST", ""},
		{"remove", func() { deleteGroupRule(t, "eng", sha1, types.RuleTypeBinary) }, "", ""},
	}
	for _, s := range steps {
		s.change()
		res := h.MustSync(t)
		if res.SyncType != "NORMAL" {
			t.Errorf("%s: %s sync, want NORMAL", s.name, res.SyncType)
		}
		if s.policy == "" {
			h.MustSync(t)
		}
		r := h.Rules[fakesanta.RuleKey{Type: "BINARY", Identifier: sha1}]
		if r.Policy != s.policy || r.CustomMsg != s.msg {
			t.Errorf("%s: policy %q message %q, want %q %q", s.name, r.Policy, r.CustomMsg, s.policy, s.msg)
		}
	}
}

// A change to the global rule a group overrides does not overwrite the group's rule on member Macs, nor does a clean
// sync.
func TestGroups_GlobalChangesAndCleanSyncsKeepTheGroupWinner(t *testing.T) {
	transport := groupsServer(t)
	registerGroups(t, map[string]bool{"eng": true})
	putGroupRule(t, "eng", teamGoogle, types.RuleTypeTeamID, types.RulePolicyBlocklist, "")
	h := newHost(t, machineA, transport)
	h.PrimaryUserGroups = []string{"eng"}
	syncNormal(t, h)

	addGlobalRule(t, teamGoogle, types.RuleTypeTeamID, types.RulePolicySilentBlocklist) // a global update of that key
	h.MustSync(t)
	if got := h.Policy("TEAMID", teamGoogle); got != "BLOCKLIST" {
		t.Errorf("after a global update: %q, want the group's BLOCKLIST", got)
	}
	h.RequestCleanSync = true
	if res := h.MustSync(t); res.SyncType == "NORMAL" {
		t.Fatalf("precondition: a clean sync, got %s", res.SyncType)
	}
	h.RequestCleanSync = false
	if got := h.Policy("TEAMID", teamGoogle); got != "BLOCKLIST" {
		t.Errorf("after a clean sync: %q, want the group's BLOCKLIST", got)
	}
}

// Group membership and many group rules must not make a steady-state sync dearer than the COST2 budget.
func TestGroups_SteadyStateSyncStaysWithinBudget(t *testing.T) {
	transport := groupsServer(t)
	registerGroups(t, map[string]bool{"eng": true, "design": true, "ops": true})
	for i := 0; i < 500; i++ {
		group := []string{"eng", "design", "ops"}[i%3]
		putItem(t, map[string]any{
			"PK": "GroupRules#" + group, "SK": rules.RuleSortKeyFromTypeIdentifier(fmt.Sprintf("%064x", i+1), types.RuleTypeBinary),
			"RuleType": int(types.RuleTypeBinary), "Policy": int(types.RulePolicyBlocklist), "Identifier": fmt.Sprintf("%064x", i+1),
			"Group": group, "DataType": "GroupRule",
		})
	}
	bumpGroup(t, "eng")
	bumpGroup(t, "design")
	bumpGroup(t, "ops")
	h := newHost(t, machineA, transport)
	h.PrimaryUserGroups = []string{"eng", "design", "ops"}
	syncNormal(t, h)
	h.MustSync(t)
	ddblocal.ResetCounts()
	h.MustSync(t)
	if c := ddblocal.ConsumedCapacity(); c.WriteUnits > 4 || c.ReadUnits > 8 {
		t.Errorf("steady-state sync of a Mac in 3 groups with 500 group rules: %s; budget 4 WCU and 8 RCU", c)
	}
	if got := len(h.Rules); got < 500 {
		t.Errorf("the Mac holds %d rules, want at least the 500 group rules", got)
	}
}
