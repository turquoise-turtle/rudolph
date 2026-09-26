//go:build integration

package handlers

// Replaces the postflight unit tests that pinned how rows marked for deletion used to be cleaned up (a query and
// delete in every postflight). What matters is the behaviour: a removed machine rule reaches the Mac, and its row
// does not linger in DynamoDB afterwards, even when a sync is interrupted.

import (
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/fakesanta"
	"github.com/airbnb/rudolph/pkg/model/machinerules"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-lambda-go/events"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
)

func TestSync_RemovedMachineRuleRowIsCleanedUpAfterDelivery(t *testing.T) {
	transport := freshServer(t)
	addMachineRule(t, machineA, sha1, types.RuleTypeBinary, types.RulePolicyAllowlist, time.Now().Add(24*time.Hour))
	host := newHost(t, machineA, transport)
	host.MustSync(t)

	if err := machinerules.GetMachineRulesService(ddblocal.Client(t)).Remove(machineA, sha1, types.RuleTypeBinary); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		host.MustSync(t)
	}

	if got := host.Policy("BINARY", sha1); got != "" {
		t.Fatalf("removed machine rule still on the host: %q", got)
	}
	if item := getItem(t, "MachineRules#"+machineA, "Binary#"+sha1); len(item) != 0 {
		t.Fatalf("the removed machine rule's row is still in DynamoDB after it was delivered: %v", item)
	}
}

// A removal served in a sync that never reaches postflight must still reach the Mac on a later sync, and the row
// must still be cleaned up eventually.
func TestSync_RemovalServedInAnAbortedSyncStillReachesTheHost(t *testing.T) {
	base := freshServer(t)
	addMachineRule(t, machineA, sha1, types.RuleTypeBinary, types.RulePolicyAllowlist, time.Now().Add(24*time.Hour))

	failPostflight := false
	transport := func(req events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, error) {
		if failPostflight && req.Resource == "/postflight/{machine_id}" {
			return &events.APIGatewayProxyResponse{StatusCode: 503, Body: `{}`}, nil
		}
		return base(req)
	}

	host := newHost(t, machineA, fakesanta.Transport(transport))
	host.MustSync(t)
	if host.Policy("BINARY", sha1) != "ALLOWLIST" {
		t.Fatal("precondition: host should have the machine rule")
	}

	if err := machinerules.GetMachineRulesService(ddblocal.Client(t)).Remove(machineA, sha1, types.RuleTypeBinary); err != nil {
		t.Fatal(err)
	}

	// The Mac downloads rules but its postflight fails. Santa applies rules before postflight, but a real client
	// may equally have crashed before applying them, so model the worst case: the Mac still holds the rule.
	failPostflight = true
	if _, err := host.Sync(t); err == nil {
		t.Fatal("precondition: the sync should fail at postflight")
	}
	host.Rules[fakesanta.RuleKey{Type: "BINARY", Identifier: sha1}] = fakesanta.Rule{Policy: "ALLOWLIST"}
	failPostflight = false

	for i := 0; i < 3; i++ {
		host.MustSync(t)
	}
	if got := host.Policy("BINARY", sha1); got != "" {
		t.Fatalf("a removal first served in an aborted sync never reached the host: %q", got)
	}
	if item := getItem(t, "MachineRules#"+machineA, "Binary#"+sha1); len(item) != 0 {
		t.Fatalf("the removed machine rule's row was never cleaned up: %v", item)
	}
}
