//go:build integration

package groups_test

// Collection of allowed executions for group rules (owner approved 2026-11-23), the model API the CLI and rudolph-web
// share:
//
//	groups.SetCollectAllowedEvents(client RegistryWriter, group, ruleSortKey string, collect bool) error
//
// sets (or clears) CollectAllowedEvents on the group rule row `GroupRules#<group>` / `<rule sort key>` and adds it to
// (or takes it out of) the string set `Group#<group>` of the collection index (PK "EventCollection", SK "Rules", see
// pkg/model/eventcollection) in one transaction, as eventcollection.SetCollectAllowedEvents does for global and
// machine rules. groups.RuleRow carries the flag as CollectAllowedEvents.
//
//   - A missing rule (or group) is refused with ErrRuleNotFound (ErrGroupNotFound is accepted for a group that is not
//     registered); nothing is written.
//   - Only an allow can be flagged: ALLOWLIST or ALLOWLIST_COMPILER. Flagging any other policy is refused with
//     ErrNotAnAllowRule and writes nothing. Clearing the flag is accepted whatever the policy.
//   - The flag is not part of what Santa downloads, so it does not change the rule's Santa fields.
//
// The group rule writers take a rule out of the index, as rule removal and re-creation do for global and machine
// rules: PutRule (the replaced row has no flag), RemoveRule, and DeleteGroup with force (the group's whole set).

import (
	"context"
	"errors"
	"os"
	"sort"
	"testing"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/model/groups"
	"github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestMain(m *testing.M) { os.Exit(ddblocal.Main(m)) }

const (
	otherTeam = "ABCDE12345"
	teamKey   = "TeamID#" + otherTeam
)

func writer(t *testing.T) groups.RegistryWriter {
	t.Helper()
	w, ok := ddblocal.Client(t).(groups.RegistryWriter)
	if !ok {
		t.Fatal("the DynamoDB client cannot write groups")
	}
	return w
}

func rawItem(t *testing.T, pk, sk string) map[string]awstypes.AttributeValue {
	t.Helper()
	out, err := ddblocal.Raw(t).GetItem(context.Background(), &awsdynamodb.GetItemInput{
		TableName:      aws.String(ddblocal.TableName),
		Key:            map[string]awstypes.AttributeValue{"PK": &awstypes.AttributeValueMemberS{Value: pk}, "SK": &awstypes.AttributeValueMemberS{Value: sk}},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.Item
}

// indexSet returns the collection index's Group#<group> set, sorted (nil when absent).
func indexSet(t *testing.T, group string) []string {
	t.Helper()
	item := rawItem(t, "EventCollection", "Rules")
	set, ok := item["Group#"+group].(*awstypes.AttributeValueMemberSS)
	if !ok {
		return nil
	}
	out := append([]string(nil), set.Value...)
	sort.Strings(out)
	return out
}

func flagOf(t *testing.T, group, sk string) bool {
	t.Helper()
	b, _ := rawItem(t, "GroupRules#"+group, sk)["CollectAllowedEvents"].(*awstypes.AttributeValueMemberBOOL)
	return b != nil && b.Value
}

func setupGroupRule(t *testing.T, group string, policy types.Policy) groups.RegistryWriter {
	t.Helper()
	w := writer(t)
	if err := groups.CreateGroup(clock.ConcreteTimeProvider{}, w, group, "test", true); err != nil && !errors.Is(err, groups.ErrGroupExists) {
		t.Fatal(err)
	}
	if err := groups.PutRule(w, group, rules.SantaRule{RuleType: types.RuleTypeTeamID, Policy: policy, Identifier: otherTeam}, "test"); err != nil {
		t.Fatal(err)
	}
	return w
}

func TestSetCollectAllowedEvents_SetsTheRowAndTheIndexTogether(t *testing.T) {
	ddblocal.Reset(t)
	w := setupGroupRule(t, "eng", types.RulePolicyAllowlist)
	before := rawItem(t, "GroupRules#eng", teamKey)
	registryBefore, err := groups.GetRegistry(w, true)
	if err != nil {
		t.Fatal(err)
	}

	if err := groups.SetCollectAllowedEvents(w, "eng", teamKey, true); err != nil {
		t.Fatalf("flagging the group rule: %v", err)
	}
	// Santa never sees the flag, so the group's rules version (what servers cache group rules by) does not move.
	registryAfter, err := groups.GetRegistry(w, true)
	if err != nil {
		t.Fatal(err)
	}
	if registryAfter.Groups["eng"].RulesVersion != registryBefore.Groups["eng"].RulesVersion {
		t.Errorf("flagging moved eng's RulesVersion from %d to %d", registryBefore.Groups["eng"].RulesVersion, registryAfter.Groups["eng"].RulesVersion)
	}
	if !flagOf(t, "eng", teamKey) {
		t.Error("the group rule row is not flagged CollectAllowedEvents")
	}
	if got := indexSet(t, "eng"); len(got) != 1 || got[0] != teamKey {
		t.Errorf("index Group#eng = %v, want [%s]", got, teamKey)
	}
	// Santa's fields of the row are left as they were.
	after := rawItem(t, "GroupRules#eng", teamKey)
	for name, v := range before {
		got, _ := after[name].(*awstypes.AttributeValueMemberN)
		want, _ := v.(*awstypes.AttributeValueMemberN)
		if want != nil && (got == nil || got.Value != want.Value) {
			t.Errorf("flagging changed %s from %v to %v", name, v, after[name])
		}
	}
	rows, err := groups.GetRules(w, "eng")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || !rows[0].CollectAllowedEvents {
		t.Errorf("GetRules does not return the flag as RuleRow.CollectAllowedEvents: %+v", rows)
	}

	if err := groups.SetCollectAllowedEvents(w, "eng", teamKey, false); err != nil {
		t.Fatalf("clearing the flag: %v", err)
	}
	if flagOf(t, "eng", teamKey) {
		t.Error("the flag was not cleared on the row")
	}
	if got := indexSet(t, "eng"); len(got) != 0 {
		t.Errorf("index Group#eng = %v after clearing, want no entry", got)
	}
}

func TestSetCollectAllowedEvents_GroupsKeepTheirOwnSets(t *testing.T) {
	ddblocal.Reset(t)
	w := setupGroupRule(t, "eng", types.RulePolicyAllowlist)
	setupGroupRule(t, "lab", types.RulePolicyAllowlistCompiler)
	setupGroupRule(t, groups.Ungrouped, types.RulePolicyAllowlist)
	for _, g := range []string{"eng", "lab", groups.Ungrouped} {
		if err := groups.SetCollectAllowedEvents(w, g, teamKey, true); err != nil {
			t.Fatalf("flagging %s: %v", g, err)
		}
	}
	for _, g := range []string{"eng", "lab", groups.Ungrouped} {
		if got := indexSet(t, g); len(got) != 1 || got[0] != teamKey {
			t.Errorf("index Group#%s = %v, want [%s]", g, got, teamKey)
		}
	}
	if err := groups.SetCollectAllowedEvents(w, "lab", teamKey, false); err != nil {
		t.Fatal(err)
	}
	if got := indexSet(t, "eng"); len(got) != 1 {
		t.Errorf("clearing lab's rule changed eng's set: %v", got)
	}
	// A global rule of the same key is another set.
	item := rawItem(t, "EventCollection", "Rules")
	if _, ok := item["Global"]; ok {
		t.Errorf("flagging group rules wrote the Global set: %v", item["Global"])
	}
}

func TestSetCollectAllowedEvents_Refusals(t *testing.T) {
	ddblocal.Reset(t)
	w := setupGroupRule(t, "eng", types.RulePolicyBlocklist)

	err := groups.SetCollectAllowedEvents(w, "eng", "TeamID#ZZZZZ99999", true)
	if !errors.Is(err, groups.ErrRuleNotFound) {
		t.Errorf("flagging a rule the group does not have: %v, want ErrRuleNotFound", err)
	}
	err = groups.SetCollectAllowedEvents(w, "nope", teamKey, true)
	if !errors.Is(err, groups.ErrRuleNotFound) && !errors.Is(err, groups.ErrGroupNotFound) {
		t.Errorf("flagging a rule of a group that does not exist: %v, want ErrRuleNotFound or ErrGroupNotFound", err)
	}
	if got := rawItem(t, "GroupRules#eng", "TeamID#ZZZZZ99999"); len(got) != 0 {
		t.Errorf("a refused flag created a rule row: %v", got)
	}
	if got := rawItem(t, "GroupRules#nope", teamKey); len(got) != 0 {
		t.Errorf("a refused flag created a rule row: %v", got)
	}

	for _, policy := range []types.Policy{types.RulePolicyBlocklist, types.RulePolicySilentBlocklist, types.RulePolicySilentGUIBlocklist, types.RulePolicySilentTTYBlocklist} {
		setupGroupRule(t, "eng", policy)
		err := groups.SetCollectAllowedEvents(w, "eng", teamKey, true)
		if !errors.Is(err, groups.ErrNotAnAllowRule) {
			t.Errorf("flagging a group rule with policy %v: %v, want ErrNotAnAllowRule", policy, err)
		}
		if flagOf(t, "eng", teamKey) {
			t.Errorf("a refused flag was written on the %v rule", policy)
		}
	}
	if got := indexSet(t, "eng"); len(got) != 0 {
		t.Errorf("refused flags reached the index: %v", got)
	}
	// Clearing is always accepted (e.g. a row flagged by hand, or a command copied twice).
	if err := groups.SetCollectAllowedEvents(w, "eng", teamKey, false); err != nil {
		t.Errorf("clearing the flag of a block rule: %v", err)
	}
}

func TestGroupRuleWritersTakeTheRuleOutOfTheIndex(t *testing.T) {
	ddblocal.Reset(t)
	w := setupGroupRule(t, "eng", types.RulePolicyAllowlist)
	setupGroupRule(t, "lab", types.RulePolicyAllowlist)
	other := "SigningID#" + otherTeam + ":com.example.tool"
	if err := groups.PutRule(w, "eng", rules.SantaRule{RuleType: types.RuleTypeSigningID, Policy: types.RulePolicyAllowlist, Identifier: otherTeam + ":com.example.tool"}, "test"); err != nil {
		t.Fatal(err)
	}
	flag := func(g, sk string) {
		t.Helper()
		if err := groups.SetCollectAllowedEvents(w, g, sk, true); err != nil {
			t.Fatalf("flagging %s %s: %v", g, sk, err)
		}
	}

	// Writing the rule again replaces the row without the flag: collection ends.
	flag("eng", teamKey)
	if err := groups.PutRule(w, "eng", rules.SantaRule{RuleType: types.RuleTypeTeamID, Policy: types.RulePolicyAllowlist, Identifier: otherTeam, CustomMessage: "again"}, "test"); err != nil {
		t.Fatal(err)
	}
	if flagOf(t, "eng", teamKey) {
		t.Error("the rewritten group rule kept the flag")
	}
	for _, sk := range indexSet(t, "eng") {
		if sk == teamKey {
			t.Error("the rewritten group rule is still in the index")
		}
	}

	// Removing the rule takes it out.
	flag("eng", teamKey)
	if err := groups.RemoveRule(w, "eng", teamKey); err != nil {
		t.Fatal(err)
	}
	for _, sk := range indexSet(t, "eng") {
		if sk == teamKey {
			t.Error("the removed group rule is still in the index")
		}
	}

	// Deleting the group with its rules takes its whole set out, and leaves other groups' sets.
	flag("eng", other)
	flag("lab", teamKey)
	if err := groups.DeleteGroup(w, "eng", true); err != nil {
		t.Fatal(err)
	}
	if got := indexSet(t, "eng"); len(got) != 0 {
		t.Errorf("index Group#eng = %v after the group was deleted, want no entry", got)
	}
	if got := indexSet(t, "lab"); len(got) != 1 {
		t.Errorf("deleting eng changed lab's set: %v", got)
	}
}
