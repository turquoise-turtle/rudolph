//go:build integration

package handlers

// Regression tests for the independent review of batch 3 (L6, L7, L9, C3).

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/internal/testutil/fakesanta"
	"github.com/airbnb/rudolph/pkg/model/machinerules"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// addManyMachineRules writes n machine rules for a machine directly, for speed.
func addManyMachineRules(t *testing.T, machineID string, n int, expires time.Time) []string {
	t.Helper()
	shas := make([]string, n)
	for i := range shas {
		shas[i] = fmt.Sprintf("%064x", i+1)
	}
	for start := 0; start < n; start += 25 {
		var requests []awstypes.WriteRequest
		for _, sha := range shas[start:min(start+25, n)] {
			item, err := attributevalue.MarshalMap(map[string]any{
				"PK": "MachineRules#" + machineID, "SK": "Binary#" + sha,
				"RuleType": 1, "Policy": 1, "Identifier": sha, "ExpiresAfter": expires.Unix(),
			})
			if err != nil {
				t.Fatal(err)
			}
			requests = append(requests, awstypes.WriteRequest{PutRequest: &awstypes.PutRequest{Item: item}})
		}
		if _, err := ddblocal.Raw(t).BatchWriteItem(context.Background(), &awsdynamodb.BatchWriteItemInput{
			RequestItems: map[string][]awstypes.WriteRequest{ddblocal.TableName: requests},
		}); err != nil {
			t.Fatal(err)
		}
	}
	return shas
}

// itemSize approximates DynamoDB's item size: attribute names plus value sizes.
func itemSize(item map[string]awstypes.AttributeValue) int {
	size := 0
	for name, value := range item {
		size += len(name) + attributeSize(value)
	}
	return size
}

func attributeSize(v awstypes.AttributeValue) int {
	switch v := v.(type) {
	case *awstypes.AttributeValueMemberS:
		return len(v.Value)
	case *awstypes.AttributeValueMemberN:
		return len(v.Value)
	case *awstypes.AttributeValueMemberB:
		return len(v.Value)
	case *awstypes.AttributeValueMemberBOOL, *awstypes.AttributeValueMemberNULL:
		return 1
	case *awstypes.AttributeValueMemberSS:
		n := 0
		for _, s := range v.Value {
			n += len(s)
		}
		return n
	case *awstypes.AttributeValueMemberBS:
		n := 0
		for _, b := range v.Value {
			n += len(b)
		}
		return n
	case *awstypes.AttributeValueMemberNS:
		n := 0
		for _, s := range v.Value {
			n += len(s)
		}
		return n
	case *awstypes.AttributeValueMemberL:
		n := 3
		for _, e := range v.Value {
			n += 1 + attributeSize(e)
		}
		return n
	case *awstypes.AttributeValueMemberM:
		n := 3
		for k, e := range v.Value {
			n += len(k) + 1 + attributeSize(e)
		}
		return n
	}
	return 0
}

// Review finding F1: a Mac with thousands of machine rules must keep syncing. The delivery tracking must not grow a
// single item past DynamoDB's 400 KB limit (which made every later preflight fail permanently).
func TestReview_MachineWithThousandsOfMachineRulesKeepsSyncing_F1(t *testing.T) {
	transport := freshServer(t)
	shas := addManyMachineRules(t, machineA, 3000, time.Now().Add(24*time.Hour))
	host := newHost(t, machineA, transport)
	for i := 0; i < 3; i++ {
		if _, err := host.Sync(t); err != nil {
			t.Fatalf("sync %d for a Mac with 3000 machine rules failed: %v", i+1, err)
		}
	}
	if len(host.Rules) != 3000 {
		t.Fatalf("host has %d rules, want 3000", len(host.Rules))
	}

	// Removing one of them must still reach the Mac.
	if err := machinerules.GetMachineRulesService(ddblocal.Client(t)).Remove(machineA, shas[1234], types.RuleTypeBinary); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := host.Sync(t); err != nil {
			t.Fatalf("sync after removal failed: %v", err)
		}
	}
	if got := host.Policy("BINARY", shas[1234]); got != "" {
		t.Fatalf("removed rule still on the host: %q", got)
	}
}

// Review finding F2: DynamoDB bills a write by the size of the whole item, so the items written on every sync must
// stay small however many machine rules a Mac has. Data that only changes when rules change may live elsewhere and
// be written only then.
func TestReview_ItemsWrittenEverySyncStaySmall_F2(t *testing.T) {
	transport := freshServer(t)
	addManyMachineRules(t, machineA, 1000, time.Now().Add(24*time.Hour))
	host := newHost(t, machineA, transport)
	host.MustSync(t)
	host.MustSync(t)
	host.MustSync(t)

	for _, sk := range []string{"SyncState", "Current"} {
		size := itemSize(getItem(t, "Machine#"+machineA, sk))
		if size > 4096 {
			t.Errorf("the %s item written on every sync is %d bytes (%d write units per write) for a Mac with 1000 machine rules; it must not grow with the number of rules", sk, size, (size+1023)/1024)
		}
	}
}

// Review finding F4: rules that stop applying are replaced with the global rule or REMOVE; looking those up must not
// cost one read per rule.
func TestReview_ExpiringManyMachineRulesDoesNotCostAReadEach_F4(t *testing.T) {
	transport := freshServer(t)
	addManyMachineRules(t, machineA, 50, time.Now().Add(2*time.Second))
	host := newHost(t, machineA, transport)
	host.MustSync(t)
	if len(host.Rules) != 50 {
		t.Fatalf("precondition: host should have 50 machine rules, has %d", len(host.Rules))
	}
	time.Sleep(3 * time.Second)

	ddblocal.ResetCounts()
	host.MustSync(t)
	counts := ddblocal.Counts()
	if len(host.Rules) != 0 {
		t.Fatalf("expired rules still on the host: %d", len(host.Rules))
	}
	reads := counts["GetItem"] + counts["Query"] + counts["BatchGetItem"]
	if reads > 8 {
		t.Errorf("a sync where 50 machine rules expired made %d read operations: %v", reads, counts)
	}
}

// Review finding F3: a postflight that does not belong to the sync that served a removal (a late duplicate from an
// earlier sync, or a retry) must not make the server believe the removal was delivered.
func TestReview_StrayPostflightDoesNotLoseARemoval_F3(t *testing.T) {
	base := freshServer(t)
	addMachineRule(t, machineA, sha1, types.RuleTypeBinary, types.RulePolicyAllowlist, time.Now().Add(24*time.Hour))

	dropRuledownload := false
	transport := func(req events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, error) {
		resp, err := base(req)
		if dropRuledownload && req.Resource == "/ruledownload/{machine_id}" {
			// The server did the work, but the response never reached the Mac.
			return &events.APIGatewayProxyResponse{StatusCode: 504, Body: `{}`}, nil
		}
		return resp, err
	}
	host := newHost(t, machineA, fakesanta.Transport(transport))
	host.MustSync(t)
	if host.Policy("BINARY", sha1) != "ALLOWLIST" {
		t.Fatal("precondition: host should have the machine rule")
	}

	if err := machinerules.GetMachineRulesService(ddblocal.Client(t)).Remove(machineA, sha1, types.RuleTypeBinary); err != nil {
		t.Fatal(err)
	}

	dropRuledownload = true
	if _, err := host.Sync(t); err == nil {
		t.Fatal("precondition: the sync should fail at rule download")
	}
	dropRuledownload = false

	// A stray postflight for this machine arrives (e.g. a delayed duplicate of an earlier request).
	resp, err := base(events.APIGatewayProxyRequest{
		HTTPMethod: "POST", Resource: "/postflight/{machine_id}", Path: "/postflight/" + machineA,
		PathParameters: map[string]string{"machine_id": machineA},
		Headers:        map[string]string{"Content-Type": "application/json"},
		Body:           `{"machine_id":"` + machineA + `","rules_received":1,"rules_processed":1,"sync_type":"NORMAL"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = resp

	for i := 0; i < 3; i++ {
		host.MustSync(t)
	}
	if got := host.Policy("BINARY", sha1); got != "" {
		t.Fatalf("a stray postflight made the server forget to send the removal; the host still has %q", got)
	}
}

// Review finding F5: updating a machine rule after removing it must bring it back, not be silently dropped.
func TestReview_UpdatingARemovedMachineRuleRestoresIt_F5(t *testing.T) {
	transport := freshServer(t)
	addMachineRule(t, machineA, sha1, types.RuleTypeBinary, types.RulePolicyAllowlist, time.Now().Add(24*time.Hour))
	host := newHost(t, machineA, transport)
	host.MustSync(t)

	svc := machinerules.GetMachineRulesService(ddblocal.Client(t))
	if err := svc.Remove(machineA, sha1, types.RuleTypeBinary); err != nil {
		t.Fatal(err)
	}
	err := svc.Update(machineA, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist, time.Now().Add(24*time.Hour))
	host.MustSync(t)
	host.MustSync(t)

	got := host.Policy("BINARY", sha1)
	if err == nil && got != "BLOCKLIST" {
		t.Fatalf("update after removal reported success but the host has %q, want BLOCKLIST", got)
	}
	if err != nil && got != "" {
		t.Fatalf("update after removal was refused (%v) but the host still has %q", err, got)
	}
}
