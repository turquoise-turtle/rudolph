//go:build integration

package rules

// CLI2 (batch 4): every rule field survives `rules export` followed by `rules import`, in both file formats, including
// values with commas, quotes and newlines.

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	rudolphrules "github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/types"
)

type roundTripRule struct {
	identifier  string
	ruleType    types.RuleType
	policy      types.Policy
	description string
	options     []rudolphrules.RuleOption
}

func roundTripRules() []roundTripRule {
	return []roundTripRule{
		{shaA, types.RuleTypeBinary, types.RulePolicyBlocklist, `Blocked, "for now"`, []rudolphrules.RuleOption{
			rudolphrules.WithCustomMessage("Line one,\nline \"two\""),
			rudolphrules.WithCustomURL("https://it.example.com/block?sha=%file_sha%&a=1,2"),
			rudolphrules.WithEventDetailButtonLabel(`Ask "IT", now`),
		}},
		{"EQHXZ8M8AV:com.google.Chrome", types.RuleTypeSigningID, types.RulePolicyAllowlist, "chrome", []rudolphrules.RuleOption{
			rudolphrules.WithNotificationAppName("Google Chrome, \"stable\""),
		}},
		{"EQHXZ8M8AV", types.RuleTypeTeamID, types.RulePolicyCEL, "cel", []rudolphrules.RuleOption{
			rudolphrules.WithCELExpr("target.signing_time >= timestamp('2025-01-01T00:00:00Z') &&\n  args.exists(a, a == \"--safe,mode\")"),
		}},
		{shaC, types.RuleTypeCertificate, types.RulePolicySilentGUIBlocklist, "", nil},
	}
}

func TestCLI2_ExportImportRoundTripsEveryRuleField(t *testing.T) {
	for _, format := range []string{"csv", "json"} {
		t.Run(format, func(t *testing.T) {
			ddblocal.Reset(t)
			client := ddblocal.Client(t)
			for _, r := range roundTripRules() {
				if err := globalrules.AddNewGlobalRule(clock.ConcreteTimeProvider{}, client, r.identifier, r.ruleType, r.policy, r.description, r.options...); err != nil {
					t.Fatalf("add %s: %v", r.identifier, err)
				}
			}
			want := map[string]globalrules.GlobalRuleRow{}
			for _, r := range roundTripRules() {
				row, err := globalrules.GetGlobalRuleByIdentifier(client, r.identifier, r.ruleType)
				if err != nil || row == nil {
					t.Fatalf("read %s: %v", r.identifier, err)
				}
				want[r.identifier] = *row
			}

			file := filepath.Join(t.TempDir(), "rules."+format)
			if err := runExport(client, file, format); err != nil {
				t.Fatalf("export: %v", err)
			}
			ddblocal.Reset(t)
			if err := runImport(client, clock.ConcreteTimeProvider{}, file, 1); err != nil {
				t.Fatalf("import: %v", err)
			}
			for _, r := range roundTripRules() {
				row, err := globalrules.GetGlobalRuleByIdentifier(client, r.identifier, r.ruleType)
				if err != nil || row == nil {
					t.Fatalf("%s missing after the round trip: %v", r.identifier, err)
				}
				if !reflect.DeepEqual(row.SantaRule, want[r.identifier].SantaRule) || row.Description != want[r.identifier].Description {
					t.Errorf("%s changed in the round trip:\n got  %+v %q\n want %+v %q", r.identifier, row.SantaRule, row.Description, want[r.identifier].SantaRule, want[r.identifier].Description)
				}
			}
		})
	}
}
