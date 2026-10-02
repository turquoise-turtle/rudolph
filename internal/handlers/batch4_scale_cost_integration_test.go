//go:build integration

package handlers

// SCALE: large rule sets must sync within Lambda's 6 MB response limit and in a reasonable number of requests.
// COST2: per-sync DynamoDB capacity budgets (read and write units, including index writes), measured by the
// ddblocal proxy from the capacity DynamoDB Local reports.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/internal/testutil/fakesanta"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
	"github.com/airbnb/rudolph/pkg/model/machinerules"
	"github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-lambda-go/events"
)

func addGlobalRulesFast(t *testing.T, n int, customMsg string) {
	t.Helper()
	items := make([]map[string]any, n)
	for i := range items {
		item := map[string]any{"PK": "GlobalRules", "SK": "Binary#" + shaN(i+1), "RuleType": 1, "Policy": 2, "Identifier": shaN(i + 1)}
		if customMsg != "" {
			item["CustomMessage"] = customMsg
		}
		items[i] = item
	}
	batchPut(t, items)
}

// countingRuledownloads counts rule download requests and fails every one above limit, so a test that is over budget
// ends quickly instead of paging through tens of thousands of rules.
func countingRuledownloads(base fakesanta.Transport, count *int, limit int) fakesanta.Transport {
	return func(req events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, error) {
		if req.Resource == "/ruledownload/{machine_id}" {
			*count++
			if *count > limit {
				return &events.APIGatewayProxyResponse{StatusCode: 429, Body: `{"error":"test budget of rule download requests exceeded"}`}, nil
			}
		}
		return base(req)
	}
}

// Figma reported multi-minute first syncs and dropped connections at 80,000 rules. A clean sync of 80,000 global rules
// must complete in at most 40 rule download requests (pages sized by response size, not by the event batch size),
// with every response under 6 MB (checked by fakesanta).
func TestSCALE_80000RuleCleanSync(t *testing.T) {
	base := freshServer(t)
	const total = 80000
	addGlobalRulesFast(t, total, "")
	requests := 0
	host := newHost(t, machineA, countingRuledownloads(base, &requests, 40))
	res, err := host.Sync(t)
	if err != nil {
		t.Fatalf("clean sync of %d rules failed after %d rule download requests (budget 40): %v", total, requests, err)
	}
	if len(host.Rules) != total {
		t.Fatalf("the Mac has %d rules, want %d", len(host.Rules), total)
	}
	t.Logf("%d rules in %d pages, largest response %d bytes", total, res.RulePages, res.MaxResponseBytes)
}

// Global rule pages must not be sized by the configured batch_size, which Santa uses for event uploads.
func TestSCALE_GlobalRulePagesDoNotUseTheEventBatchSize(t *testing.T) {
	transport := freshServer(t)
	cfg := machineconfiguration.GetUniversalDefaultConfig()
	cfg.BatchSize = 5
	if err := configService(t).SetGlobalConfig(cfg); err != nil {
		t.Fatal(err)
	}
	addGlobalRulesFast(t, 60, "")
	res := newHost(t, machineA, transport).MustSync(t)
	if res.RulesProcessed != 60 {
		t.Fatalf("precondition: 60 rules, got %d", res.RulesProcessed)
	}
	if res.RulePages > 2 {
		t.Fatalf("60 small rules took %d rule download pages: pages follow the event batch size (5)", res.RulePages)
	}
}

// A Mac with many machine rules: they are all sent at the end of the sync, and that last response must stay under
// 6 MB. 25,000 rules with a 250 byte custom message are about 10 MB of rules.
func TestSCALE_ManyMachineRulesStayUnderTheResponseLimit(t *testing.T) {
	transport := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	const n = 25000
	msg := strings.Repeat("m", 250)
	items := make([]map[string]any, n)
	for i := range items {
		items[i] = map[string]any{
			"PK": "MachineRules#" + machineA, "SK": "Binary#" + shaN(i+1), "RuleType": 1, "Policy": 1,
			"Identifier": shaN(i + 1), "CustomMessage": msg, "ExpiresAfter": time.Now().Add(48 * time.Hour).Unix(),
		}
	}
	batchPut(t, items)
	host := newHost(t, machineA, transport)
	if _, err := host.Sync(t); err != nil {
		t.Fatalf("sync of a Mac with %d machine rules failed: %v", n, err)
	}
	if len(host.Rules) != n+1 {
		t.Fatalf("the Mac has %d rules, want %d", len(host.Rules), n+1)
	}
}

// Rule fields without a natural length bound must be bounded when a rule is created, so a single rule cannot make
// every page that carries it (and every Mac's rule download) fail.
func TestSCALE_UnboundedRuleFieldsAreBoundedAtCreation(t *testing.T) {
	freshServer(t)
	huge := strings.Repeat("x", 64*1024)
	for name, tc := range map[string]struct {
		policy types.Policy
		option rules.RuleOption
	}{
		"custom_msg":                {types.RulePolicyBlocklist, rules.WithCustomMessage(huge)},
		"custom_url":                {types.RulePolicyBlocklist, rules.WithCustomURL("https://x.example.com/" + huge)},
		"cel_expr":                  {types.RulePolicyCEL, rules.WithCELExpr("true || '" + huge + "' == ''")},
		"event_detail_button_label": {types.RulePolicyBlocklist, rules.WithEventDetailButtonLabel(huge)},
		"notification_app_name":     {types.RulePolicyAllowlist, rules.WithNotificationAppName(huge)},
	} {
		t.Run(name, func(t *testing.T) {
			if err := globalrules.AddNewGlobalRule(clock.ConcreteTimeProvider{}, ddblocal.Client(t), sha1, types.RuleTypeBinary, tc.policy, "test", tc.option); err == nil {
				t.Errorf("a global rule with a 64 KiB %s was accepted", name)
			}
			if err := machinerules.AddNewMachineRule(ddblocal.Client(t), machineA, sha2, types.RuleTypeBinary, tc.policy, "test", time.Now().Add(time.Hour), tc.option); err == nil {
				t.Errorf("a machine rule with a 64 KiB %s was accepted", name)
			}
		})
	}
	// Ordinary values are accepted.
	normal := strings.Repeat("y", 200)
	if err := globalrules.AddNewGlobalRule(clock.ConcreteTimeProvider{}, ddblocal.Client(t), sha3, types.RuleTypeBinary, types.RulePolicyBlocklist, "test",
		rules.WithCustomMessage(normal), rules.WithCustomURL("https://it.example.com/"+normal), rules.WithEventDetailButtonLabel("Get help")); err != nil {
		t.Fatalf("ordinary rule fields must be accepted: %v", err)
	}
}

// --------------------------------------------------------------------------------------------------------------
// COST2
// --------------------------------------------------------------------------------------------------------------

func costFixture(t *testing.T) *fakesanta.Host {
	t.Helper()
	transport := freshServer(t)
	addGlobalRulesFast(t, 1000, "")
	addManyMachineRules(t, machineA, 100, time.Now().Add(24*time.Hour))
	return newHost(t, machineA, transport)
}

// A sync with no changes, for a Mac with 1,000 global and 100 machine rules: at most 4 write units and 8 read units,
// index writes included (today: 3 WCU, 5.5 RCU).
func TestCOST2_SteadyStateSyncCapacityBudget(t *testing.T) {
	host := costFixture(t)
	host.MustSync(t)
	host.MustSync(t)
	ddblocal.ResetCounts()
	host.MustSync(t)
	c := ddblocal.ConsumedCapacity()
	if c.WriteUnits > 4 || c.ReadUnits > 8 {
		t.Errorf("steady-state sync consumed %s; budget 4 WCU and 8 RCU", c)
	}
}

// A Mac's first (clean) sync with 1,000 global and 100 machine rules: reading 1,000 small rules is about 150 KB, so at
// most 30 read units; writes at most one unit per machine rule tracked plus 10 (today: 37 RCU, because pages of 50
// rules each round up, and 108 WCU).
func TestCOST2_FirstSyncCapacityBudget(t *testing.T) {
	host := costFixture(t)
	ddblocal.ResetCounts()
	host.MustSync(t)
	c := ddblocal.ConsumedCapacity()
	if c.ReadUnits > 30 {
		t.Errorf("first sync consumed %.1f RCU; budget 30: %s", c.ReadUnits, c)
	}
	if c.WriteUnits > 110 {
		t.Errorf("first sync consumed %.1f WCU; budget 110: %s", c.WriteUnits, c)
	}
}

// A Mac reporting many primary user groups (directory-bound Macs can have hundreds) must not make every preflight
// write a large item: a steady-state sync stays within the 4 write unit budget (today: 18 WCU with 256 groups).
func TestCOST2_ManyPrimaryUserGroupsDoNotInflateEverySync(t *testing.T) {
	host := costFixture(t)
	for i := 0; i < 256; i++ {
		host.PrimaryUserGroups = append(host.PrimaryUserGroups, fmt.Sprintf("group-%03d-%s", i, strings.Repeat("g", 50)))
	}
	host.MustSync(t)
	host.MustSync(t)
	ddblocal.ResetCounts()
	host.MustSync(t)
	if c := ddblocal.ConsumedCapacity(); c.WriteUnits > 4 {
		t.Errorf("with 256 primary user groups a steady-state sync consumed %.1f WCU; budget 4: %s", c.WriteUnits, c)
	}
}
