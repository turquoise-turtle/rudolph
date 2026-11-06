//go:build integration

package globalrules_test

// R5 of rudolph-web's docs/data-layer-design.md: GlobalRulePolicy mirrors Policy on global rule rows, for the web
// view's GSI GlobalRulePolicy_SK. Rows written before that change get it from BackfillPolicyIndex, which must not
// touch the rules feed or anything else Santa sees. In its own package so that, until BackfillPolicyIndex exists, only
// this test fails to build.

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestMain(m *testing.M) { os.Exit(ddblocal.Main(m)) }

func str(it map[string]awstypes.AttributeValue, attr string) (string, bool) {
	s, ok := it[attr].(*awstypes.AttributeValueMemberS)
	if !ok {
		return "", false
	}
	return s.Value, true
}

func putItem(t *testing.T, item map[string]any) {
	t.Helper()
	av, err := attributevalue.MarshalMap(item)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ddblocal.Raw(t).PutItem(context.Background(), &awsdynamodb.PutItemInput{TableName: aws.String(ddblocal.TableName), Item: av}); err != nil {
		t.Fatal(err)
	}
}

func scanAll(t *testing.T) []map[string]awstypes.AttributeValue {
	t.Helper()
	var items []map[string]awstypes.AttributeValue
	var start map[string]awstypes.AttributeValue
	for {
		out, err := ddblocal.Raw(t).Scan(context.Background(), &awsdynamodb.ScanInput{TableName: aws.String(ddblocal.TableName), ExclusiveStartKey: start, ConsistentRead: aws.Bool(true)})
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, out.Items...)
		if len(out.LastEvaluatedKey) == 0 {
			return items
		}
		start = out.LastEvaluatedKey
	}
}

func rulesByPolicy(t *testing.T, policy types.Policy) []map[string]awstypes.AttributeValue {
	t.Helper()
	out, err := ddblocal.Raw(t).Query(context.Background(), &awsdynamodb.QueryInput{
		TableName:              aws.String(ddblocal.TableName),
		IndexName:              aws.String("GlobalRulePolicy_SK"),
		KeyConditionExpression: aws.String("GlobalRulePolicy = :p"),
		ExpressionAttributeValues: map[string]awstypes.AttributeValue{
			":p": &awstypes.AttributeValueMemberN{Value: fmt.Sprint(int(policy))},
		},
	})
	if err != nil {
		t.Fatalf("query GlobalRulePolicy_SK: %v", err)
	}
	return out.Items
}

// Global rule rows written by an older Rudolph have no GlobalRulePolicy. The backfill adds it to those rows only,
// changes nothing else, and does nothing when run again.
func TestR5_BackfillAddsThePolicyToOlderRows(t *testing.T) {
	ddblocal.Reset(t)
	if err := globalrules.AddNewGlobalRule(clock.ConcreteTimeProvider{}, ddblocal.Client(t), "EQHXZ8M8AV", types.RuleTypeTeamID, types.RulePolicyAllowlist, "already indexed"); err != nil {
		t.Fatal(err)
	}
	for i, policy := range []types.Policy{types.RulePolicyBlocklist, types.RulePolicySilentBlocklist, types.RulePolicyAllowlist} {
		sha := strings.Repeat(fmt.Sprintf("%02d", i+10), 32)
		putItem(t, map[string]any{
			"PK": "GlobalRules", "SK": "Binary#" + sha,
			"RuleType": int(types.RuleTypeBinary), "Policy": int(policy), "Identifier": sha,
			"Description": "written by an older Rudolph",
		})
	}
	before := scanAll(t)

	updated, err := globalrules.BackfillPolicyIndex(ddblocal.Client(t))
	if err != nil {
		t.Fatal(err)
	}
	if updated != 3 {
		t.Errorf("backfill updated %d rows, want the 3 older ones", updated)
	}
	after := scanAll(t)
	if len(after) != len(before) {
		t.Fatalf("the backfill changed the number of items from %d to %d", len(before), len(after))
	}
	byKey := map[string]map[string]awstypes.AttributeValue{}
	for _, it := range after {
		pk, _ := str(it, "PK")
		sk, _ := str(it, "SK")
		byKey[pk+"/"+sk] = it
	}
	for _, old := range before {
		pk, _ := str(old, "PK")
		sk, _ := str(old, "SK")
		now := byKey[pk+"/"+sk]
		got := map[string]awstypes.AttributeValue{}
		for k, v := range now {
			if k != "GlobalRulePolicy" {
				got[k] = v
			}
		}
		oldWithout := map[string]awstypes.AttributeValue{}
		for k, v := range old {
			if k != "GlobalRulePolicy" {
				oldWithout[k] = v
			}
		}
		if !reflect.DeepEqual(got, oldWithout) {
			t.Errorf("the backfill changed %s / %s beyond GlobalRulePolicy", pk, sk)
		}
		if pk == "GlobalRules" {
			p, ok := now["GlobalRulePolicy"].(*awstypes.AttributeValueMemberN)
			want := old["Policy"].(*awstypes.AttributeValueMemberN)
			if !ok || p.Value != want.Value {
				t.Errorf("%s: GlobalRulePolicy %v, want %s", sk, now["GlobalRulePolicy"], want.Value)
			}
		}
	}
	if got := len(rulesByPolicy(t, types.RulePolicyAllowlist)); got != 2 {
		t.Errorf("%d allowlist rules in the index after the backfill, want 2", got)
	}

	again, err := globalrules.BackfillPolicyIndex(ddblocal.Client(t))
	if err != nil || again != 0 {
		t.Errorf("second backfill: updated %d, err %v; want 0 and nil", again, err)
	}
}
