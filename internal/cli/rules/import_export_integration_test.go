//go:build integration

package rules

// Regression tests for findings from the independent review of the P2/P3/P5 branch (import/export).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

func TestMain(m *testing.M) { os.Exit(ddblocal.Main(m)) }

var (
	shaA = strings.Repeat("0a", 32)
	shaB = strings.Repeat("0b", 32)
	shaC = strings.Repeat("0c", 32)
)

func globalPolicy(t *testing.T, identifier string, ruleType types.RuleType) (types.Policy, bool) {
	t.Helper()
	row, err := globalrules.GetGlobalRuleByIdentifier(ddblocal.Client(t), identifier, ruleType)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil {
		return 0, false
	}
	return row.Policy, true
}

// Review finding 2: an old backup containing a legacy ALLOWLIST_TRANSITIVE rule must import the other rules
// instead of aborting partway through.
func TestReview_ImportSkipsLegacyTransitiveRules(t *testing.T) {
	ddblocal.Reset(t)
	file := filepath.Join(t.TempDir(), "backup.json")
	contents := `[
		{"type":"BINARY","policy":"BLOCKLIST","identifier":"` + shaA + `","description":"a"},
		{"type":"BINARY","policy":"ALLOWLIST_TRANSITIVE","identifier":"` + shaB + `","description":"legacy"},
		{"type":"BINARY","policy":"ALLOWLIST","identifier":"` + shaC + `","description":"c"}
	]`
	if err := os.WriteFile(file, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("import panicked: %v", r)
			}
		}()
		_ = runImport(ddblocal.Client(t), clock.ConcreteTimeProvider{}, file, 1)
	}()

	if p, ok := globalPolicy(t, shaA, types.RuleTypeBinary); !ok || p != types.RulePolicyBlocklist {
		t.Errorf("rule before the legacy row not imported: %v %v", p, ok)
	}
	if p, ok := globalPolicy(t, shaC, types.RuleTypeBinary); !ok || p != types.RulePolicyAllowlist {
		t.Errorf("rule after the legacy row not imported: %v %v", p, ok)
	}
	if _, ok := globalPolicy(t, shaB, types.RuleTypeBinary); ok {
		t.Error("legacy transitive rule must not be imported")
	}
}

// Review finding 3: legacy rows that only have SHA256 must export with their identifier and round-trip.
func TestReview_ExportIncludesLegacySHA256OnlyRules(t *testing.T) {
	for _, ext := range []string{"json", "csv"} {
		t.Run(ext, func(t *testing.T) {
			ddblocal.Reset(t)
			item, err := attributevalue.MarshalMap(map[string]any{
				"PK": "GlobalRules", "SK": "Binary#" + shaA, "RuleType": 1, "Policy": 2, "SHA256": shaA, "Description": "legacy",
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ddblocal.Raw(t).PutItem(context.Background(), &awsdynamodb.PutItemInput{TableName: aws.String(ddblocal.TableName), Item: item}); err != nil {
				t.Fatal(err)
			}

			file := filepath.Join(t.TempDir(), "rules."+ext)
			if err := runExport(ddblocal.Client(t), file, ext); err != nil {
				t.Fatalf("export failed: %v", err)
			}
			exported, _ := os.ReadFile(file)
			if !strings.Contains(string(exported), shaA) {
				t.Fatalf("export lost the identifier of a legacy SHA256-only rule:\n%s", exported)
			}

			ddblocal.Reset(t)
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("re-import panicked: %v", r)
					}
				}()
				if err := runImport(ddblocal.Client(t), clock.ConcreteTimeProvider{}, file, 1); err != nil {
					t.Fatalf("re-import failed: %v", err)
				}
			}()
			if p, ok := globalPolicy(t, shaA, types.RuleTypeBinary); !ok || p != types.RulePolicyBlocklist {
				t.Fatalf("round-tripped rule = %v %v, want BLOCKLIST", p, ok)
			}
		})
	}
}
