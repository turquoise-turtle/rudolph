//go:build integration

package handlers

// Regression tests for findings from the independent review of the P2/P3/P5 branch.

import (
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/internal/testutil/fakesanta"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	"github.com/airbnb/rudolph/pkg/model/machinerules"
	"github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/types"
)

// Review finding 1: a CEL rule whose policy is later changed keeps its expression and can no longer be removed.
func TestReview_CELRuleUpdatedToAnotherPolicyCanBeRemoved(t *testing.T) {
	transport := freshServer(t)
	tp := clock.ConcreteTimeProvider{}
	if err := globalrules.AddNewGlobalRule(tp, ddblocal.Client(t), chromeSigningID, types.RuleTypeSigningID, types.RulePolicyCEL, "test", rules.WithCELExpr("true")); err != nil {
		t.Fatal(err)
	}
	host := newHost(t, machineA, transport)
	host.MustSync(t)

	if err := globalrules.UpdateGlobalRule(tp, ddblocal.Client(t), chromeSigningID, types.RuleTypeSigningID, types.RulePolicyBlocklist); err != nil {
		t.Fatalf("updating a CEL rule to BLOCKLIST failed: %v", err)
	}
	for name, h := range map[string]func() (policy, celExpr string){
		"incremental": func() (string, string) {
			host.MustSync(t)
			r := host.Rules[ruleKey("SIGNINGID", chromeSigningID)]
			return r.Policy, r.CELExpr
		},
		"clean": func() (string, string) {
			fresh := newHost(t, machineB, transport)
			fresh.MustSync(t)
			r := fresh.Rules[ruleKey("SIGNINGID", chromeSigningID)]
			return r.Policy, r.CELExpr
		},
	} {
		if policy, expr := h(); policy != "BLOCKLIST" || expr != "" {
			t.Errorf("%s sync: policy %q with cel_expr %q, want BLOCKLIST without an expression", name, policy, expr)
		}
	}

	removal := globalrules.ConcreteRuleRemovalService{TimeProvider: tp, Getter: ddblocal.Client(t), Transacter: ddblocal.Client(t)}
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("removing the rule panicked: %v", r)
			}
		}()
		if err := removal.RemoveGlobalRule(rules.RuleSortKeyFromTypeIdentifier(chromeSigningID, types.RuleTypeSigningID), ""); err != nil {
			t.Fatalf("removing the rule failed: %v", err)
		}
	}()
	host.MustSync(t)
	if got := host.Policy("SIGNINGID", chromeSigningID); got != "" {
		t.Fatalf("removed rule still on host: %q", got)
	}
}

// Review finding 7: changing a rule's policy must not drop its other fields for incrementally syncing hosts.
func TestReview_PolicyUpdateKeepsRuleFieldsOnIncrementalSync(t *testing.T) {
	transport := freshServer(t)
	tp := clock.ConcreteTimeProvider{}
	if err := globalrules.AddNewGlobalRule(tp, ddblocal.Client(t), sha1, types.RuleTypeBinary, types.RulePolicyBlocklist, "test",
		rules.WithCustomMessage("Ask IT"), rules.WithCustomURL("https://it.example.com/%file_sha%")); err != nil {
		t.Fatal(err)
	}
	host := newHost(t, machineA, transport)
	host.MustSync(t)

	if err := globalrules.UpdateGlobalRule(tp, ddblocal.Client(t), sha1, types.RuleTypeBinary, types.RulePolicySilentGUIBlocklist); err != nil {
		t.Fatal(err)
	}
	host.MustSync(t)
	got := host.Rules[ruleKey("BINARY", sha1)]
	if got.Policy != "SILENT_GUI_BLOCKLIST" || got.CustomMsg != "Ask IT" || got.CustomURL != "https://it.example.com/%file_sha%" {
		t.Fatalf("after a policy update the host has %+v, want the new policy with the original message and URL", got)
	}
}

// Review finding 4: removing a machine override restores the global rule, including its fields.
func TestReview_RemovingMachineOverrideRestoresGlobalCELRule(t *testing.T) {
	transport := freshServer(t)
	tp := clock.ConcreteTimeProvider{}
	const expr = "target.signing_time >= timestamp('2025-06-01T00:00:00Z')"
	if err := globalrules.AddNewGlobalRule(tp, ddblocal.Client(t), chromeSigningID, types.RuleTypeSigningID, types.RulePolicyCEL, "test", rules.WithCELExpr(expr)); err != nil {
		t.Fatal(err)
	}
	addMachineRule(t, machineA, chromeSigningID, types.RuleTypeSigningID, types.RulePolicyBlocklist, time.Now().Add(time.Hour))
	host := newHost(t, machineA, transport)
	host.MustSync(t)
	if host.Policy("SIGNINGID", chromeSigningID) != "BLOCKLIST" {
		t.Fatal("precondition: machine override should apply")
	}

	if err := machinerules.GetMachineRulesService(ddblocal.Client(t)).Remove(machineA, chromeSigningID, types.RuleTypeSigningID); err != nil {
		t.Fatal(err)
	}
	host.MustSync(t)
	if got := host.Rules[ruleKey("SIGNINGID", chromeSigningID)]; got.Policy != "CEL" || got.CELExpr != expr {
		t.Fatalf("after removing the override the host has %+v, want the global CEL rule with its expression", got)
	}
}

func TestReview_RemovingMachineCELOverrideDropsItsExpression(t *testing.T) {
	transport := freshServer(t)
	tp := clock.ConcreteTimeProvider{}
	addGlobalRule(t, chromeSigningID, types.RuleTypeSigningID, types.RulePolicyAllowlist)
	if err := machinerules.AddNewMachineRule(ddblocal.Client(t), machineA, chromeSigningID, types.RuleTypeSigningID, types.RulePolicyCEL, "test", time.Now().Add(time.Hour), rules.WithCELExpr("false")); err != nil {
		t.Fatal(err)
	}
	_ = tp
	host := newHost(t, machineA, transport)
	host.MustSync(t)
	if host.Policy("SIGNINGID", chromeSigningID) != "CEL" {
		t.Fatal("precondition: machine CEL override should apply")
	}

	if err := machinerules.GetMachineRulesService(ddblocal.Client(t)).Remove(machineA, chromeSigningID, types.RuleTypeSigningID); err != nil {
		t.Fatal(err)
	}
	host.MustSync(t)
	if got := host.Rules[ruleKey("SIGNINGID", chromeSigningID)]; got.Policy != "ALLOWLIST" || got.CELExpr != "" {
		t.Fatalf("after removing the CEL override the host has %+v, want ALLOWLIST without an expression", got)
	}
}

func ruleKey(ruleType, identifier string) fakesanta.RuleKey {
	return fakesanta.RuleKey{Type: ruleType, Identifier: identifier}
}
