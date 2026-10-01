//go:build integration

package handlers

// Regression test for the re-review of the batch 3 redesign (finding H1a).

import (
	"context"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/internal/testutil/fakesanta"
	"github.com/airbnb/rudolph/pkg/model/machinerules"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func syncStateAttribute(t *testing.T, machineID, name string) awstypes.AttributeValue {
	t.Helper()
	return getItem(t, "Machine#"+machineID, "SyncState")[name]
}

func restoreSyncStateAttribute(t *testing.T, machineID, name string, value awstypes.AttributeValue) {
	t.Helper()
	key := map[string]awstypes.AttributeValue{
		"PK": &awstypes.AttributeValueMemberS{Value: "Machine#" + machineID},
		"SK": &awstypes.AttributeValueMemberS{Value: "SyncState"},
	}
	input := &awsdynamodb.UpdateItemInput{TableName: aws.String(ddblocal.TableName), Key: key}
	if value == nil {
		input.UpdateExpression = aws.String("REMOVE #a")
		input.ExpressionAttributeNames = map[string]string{"#a": name}
	} else {
		input.UpdateExpression = aws.String("SET #a = :v")
		input.ExpressionAttributeNames = map[string]string{"#a": name}
		input.ExpressionAttributeValues = map[string]awstypes.AttributeValue{":v": value}
	}
	if _, err := ddblocal.Raw(t).UpdateItem(context.Background(), input); err != nil {
		t.Fatal(err)
	}
}

// Review finding H1(a): rule download writes per-rule delivery tracking and then a summary of it on the sync state
// row. If the Lambda dies between the two (timeout, crash, a failing later batch), the summary is stale. A stale
// summary must never let the server believe a Mac already got a removal it never received.
//
// Simulated failure: the rule download that serves a removal runs server-side, but the Mac never gets the response
// and the summary update is lost (restored to its previous value). The admin then re-adds the rule, and later removes
// it again; the second removal must reach the Mac.
func TestReview_LostTrackingSummaryUpdateNeverHidesARemoval_H1(t *testing.T) {
	base := freshServer(t)
	addMachineRule(t, machineA, sha1, types.RuleTypeBinary, types.RulePolicyAllowlist, time.Now().Add(24*time.Hour))

	failRuledownload := false
	transport := func(req events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, error) {
		resp, err := base(req)
		if failRuledownload && req.Resource == "/ruledownload/{machine_id}" {
			return &events.APIGatewayProxyResponse{StatusCode: 504, Body: `{}`}, nil
		}
		return resp, err
	}
	host := newHost(t, machineA, fakesanta.Transport(transport))
	host.MustSync(t)
	host.MustSync(t)
	if host.Policy("BINARY", sha1) != "ALLOWLIST" {
		t.Fatal("precondition: the host should have the machine rule")
	}
	summaryBefore := syncStateAttribute(t, machineA, "MachineRulesTracked")
	if summaryBefore == nil {
		t.Fatal("precondition: expected a MachineRulesTracked summary on the sync state row; update this test if it was renamed")
	}

	svc := machinerules.GetMachineRulesService(ddblocal.Client(t))
	if err := svc.Remove(machineA, sha1, types.RuleTypeBinary); err != nil {
		t.Fatal(err)
	}
	failRuledownload = true
	if _, err := host.Sync(t); err == nil {
		t.Fatal("precondition: the sync should fail at rule download")
	}
	failRuledownload = false
	// The summary update after the tracking writes was lost.
	restoreSyncStateAttribute(t, machineA, "MachineRulesTracked", summaryBefore)

	// The admin changes their mind, then removes the rule again later.
	addMachineRule(t, machineA, sha1, types.RuleTypeBinary, types.RulePolicyAllowlist, time.Now().Add(24*time.Hour))
	for i := 0; i < 3; i++ {
		host.MustSync(t)
	}
	if host.Policy("BINARY", sha1) != "ALLOWLIST" {
		t.Fatal("the re-added rule should be on the host")
	}
	if err := svc.Remove(machineA, sha1, types.RuleTypeBinary); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		host.MustSync(t)
	}
	if got := host.Policy("BINARY", sha1); got != "" {
		t.Fatalf("after a lost tracking summary update, a later removal never reached the host: %q", got)
	}
}
