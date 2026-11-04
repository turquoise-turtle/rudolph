//go:build integration

package rule

// Found by the review of batch 4's CLI follow-ups: the server keeps a Mac's machine ID exactly as the Mac sends it,
// and Santa's MachineID setting can be a lower-case UUID. Upper-casing every --machine value made such a Mac's rules
// unreachable from the CLI and put new rules under an ID the Mac never reads.

import (
	"context"
	"strings"
	"testing"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/pkg/model/sensordata"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

var lowerMachine = strings.ToLower("CCCCCCCC-3333-4333-8333-CCCCCCCCCCCC")

func seedSyncingMac(t *testing.T, machineID string) {
	t.Helper()
	pk, sk := sensordata.MachineIDSensorDataPKSK(machineID)
	if _, err := ddblocal.Raw(t).PutItem(context.Background(), &awsdynamodb.PutItemInput{
		TableName: aws.String(ddblocal.TableName),
		Item: map[string]awstypes.AttributeValue{
			"PK":        &awstypes.AttributeValueMemberS{Value: pk},
			"SK":        &awstypes.AttributeValueMemberS{Value: sk},
			"MachineID": &awstypes.AttributeValueMemberS{Value: machineID},
			"DataType":  &awstypes.AttributeValueMemberS{Value: string(sensordata.GetDataType())},
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func machineRuleOf(t *testing.T, machineID string) map[string]awstypes.AttributeValue {
	t.Helper()
	out, err := ddblocal.Raw(t).GetItem(context.Background(), &awsdynamodb.GetItemInput{
		TableName: aws.String(ddblocal.TableName),
		Key: map[string]awstypes.AttributeValue{
			"PK": &awstypes.AttributeValueMemberS{Value: "MachineRules#" + machineID},
			"SK": &awstypes.AttributeValueMemberS{Value: "Binary#" + sha},
		},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.Item
}

func TestReview_RuleCommandsReachAMacThatSyncsUnderALowerCaseID(t *testing.T) {
	ddblocal.Reset(t)
	seedSyncingMac(t, lowerMachine)
	upper := strings.ToUpper(lowerMachine)

	if out, err := run(t, "allow", "-t", "binary", "-i", sha, "--machine", lowerMachine, "--permanent", "--force"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if machineRuleOf(t, lowerMachine) == nil {
		t.Fatalf("the rule is not stored under the ID the Mac syncs with (%s); stored under %s: %v", lowerMachine, upper, machineRuleOf(t, upper) != nil)
	}
	before := policyOf(machineRuleOf(t, lowerMachine))

	if out, err := run(t, "update", "-t", "binary", "-i", sha, "-p", "blocklist", "--machine", upper, "--force"); err != nil {
		t.Fatalf("rule update with the ID typed in upper case: %v\n%s", err, out)
	}
	if policyOf(machineRuleOf(t, lowerMachine)) == before {
		t.Errorf("rule update did not reach the Mac's rule")
	}

	if out, err := run(t, "remove", "Binary#"+sha, "--machine", lowerMachine, "--force"); err != nil {
		t.Fatalf("rule remove: %v\n%s", err, out)
	}
	// A removed machine rule stays, marked, until the Mac has been told to remove it.
	if item := machineRuleOf(t, lowerMachine); item != nil {
		if marked, _ := item["DeleteOnNextSync"].(*awstypes.AttributeValueMemberBOOL); marked == nil || !marked.Value {
			t.Errorf("rule remove did not reach the Mac's rule: %v", item)
		}
	}
}
