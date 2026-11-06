//go:build integration

package handlers

// R5 of rudolph-web's docs/data-layer-design.md: the web view filters global rules by policy through GSI
// GlobalRulePolicy_SK (hash GlobalRulePolicy, range SK). GlobalRulePolicy mirrors Policy on global rule rows only:
// machine rules and the rules feed carry Policy too, and must stay out of the index. Rows written before this change
// get the attribute from a backfill (pkg/model/globalrules/backfill_integration_test.go).

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const policyIndex = "GlobalRulePolicy_SK"

func rulesByPolicy(t *testing.T, policy types.Policy) []map[string]awstypes.AttributeValue {
	t.Helper()
	out, err := ddblocal.Raw(t).Query(context.Background(), &awsdynamodb.QueryInput{
		TableName:              aws.String(ddblocal.TableName),
		IndexName:              aws.String(policyIndex),
		KeyConditionExpression: aws.String("GlobalRulePolicy = :p"),
		ExpressionAttributeValues: map[string]awstypes.AttributeValue{
			":p": &awstypes.AttributeValueMemberN{Value: fmt.Sprint(int(policy))},
		},
	})
	if err != nil {
		t.Fatalf("query %s: %v", policyIndex, err)
	}
	return out.Items
}

func sortKeys(t *testing.T, items []map[string]awstypes.AttributeValue) string {
	t.Helper()
	var sks []string
	for _, it := range items {
		sk, _ := sensorString(t, it, "SK")
		sks = append(sks, sk)
	}
	return strings.Join(sks, ",")
}

func TestR5_GlobalRulesAreIndexedByPolicy(t *testing.T) {
	freshServer(t)
	addGlobalRule(t, sha2, types.RuleTypeBinary, types.RulePolicyBlocklist)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	addGlobalRule(t, chromeTeamID, types.RuleTypeTeamID, types.RulePolicyAllowlist)
	addGlobalRule(t, chromeSigningID, types.RuleTypeSigningID, types.RulePolicyBlocklist)

	items := rulesByPolicy(t, types.RulePolicyBlocklist)
	want := "Binary#" + sha1 + ",Binary#" + sha2 + ",SigningID#" + chromeSigningID
	if got := sortKeys(t, items); got != want {
		t.Errorf("blocklist rules %s, want %s (in SK order)", got, want)
	}
	// The rules page shows every rule attribute, so the index projects them all.
	full := getItem(t, "GlobalRules", "Binary#"+sha1)
	if !reflect.DeepEqual(items[0], full) {
		t.Errorf("index item %v differs from the table item %v", items[0], full)
	}
	if got := sortKeys(t, rulesByPolicy(t, types.RulePolicyAllowlist)); got != "TeamID#"+chromeTeamID {
		t.Errorf("allowlist rules %s", got)
	}
}

func TestR5_PolicyIndexFollowsUpdatesAndRemovals(t *testing.T) {
	freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	addGlobalRule(t, sha2, types.RuleTypeBinary, types.RulePolicyBlocklist)

	if err := globalrules.UpdateGlobalRule(clock.ConcreteTimeProvider{}, ddblocal.Client(t), sha1, types.RuleTypeBinary, types.RulePolicySilentBlocklist); err != nil {
		t.Fatal(err)
	}
	if got := sortKeys(t, rulesByPolicy(t, types.RulePolicyBlocklist)); got != "Binary#"+sha2 {
		t.Errorf("blocklist after the update: %s, want only %s", got, sha2)
	}
	if got := sortKeys(t, rulesByPolicy(t, types.RulePolicySilentBlocklist)); got != "Binary#"+sha1 {
		t.Errorf("silent blocklist after the update: %s, want %s", got, sha1)
	}

	if err := globalrules.RemoveGlobalRule(clock.ConcreteTimeProvider{}, ddblocal.Client(t), ddblocal.Client(t), "Binary#"+sha2, "r5-remove-test"); err != nil {
		t.Fatal(err)
	}
	if got := sortKeys(t, rulesByPolicy(t, types.RulePolicyBlocklist)); got != "" {
		t.Errorf("blocklist after the removal: %s, want none", got)
	}
	// The removal's feed row has Policy REMOVE, but is not a global rule.
	if got := rulesByPolicy(t, types.RulePolicyRemove); len(got) != 0 {
		t.Errorf("the index has %d REMOVE rows: %s", len(got), sortKeys(t, got))
	}
}

func TestR5_OnlyGlobalRuleRowsCarryGlobalRulePolicy(t *testing.T) {
	transport := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	addMachineRule(t, machineA, sha2, types.RuleTypeBinary, types.RulePolicyBlocklist, time.Now().Add(24*time.Hour))
	newHost(t, machineA, transport).MustSync(t)

	for _, it := range scanAll(t) {
		pk, _ := sensorString(t, it, "PK")
		sk, _ := sensorString(t, it, "SK")
		_, has := it["GlobalRulePolicy"]
		if pk == "GlobalRules" && !has {
			t.Errorf("global rule %s has no GlobalRulePolicy", sk)
		}
		if pk != "GlobalRules" && has {
			t.Errorf("%s / %s carries GlobalRulePolicy; only global rule rows may", pk, sk)
		}
	}
}
