//go:build integration

package rules

// Quality-of-life gaps of `rudolph rules` from the plan review of 2026-11-06 (plan C.4, C.10, C.12). They fail until
// the CLI batch lands: the list shows every global rule (it stopped at 40 with no way to see the rest) and can be
// filtered with --policy and --type; --global and --machine together are refused; an export with an unknown
// --fileformat is an error instead of a silent run that writes nothing.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/airbnb/rudolph/internal/testutil/clitest"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	"github.com/airbnb/rudolph/pkg/types"
)

var qolEnv = clitest.Env{Table: ddblocal.TableName, Region: ddblocal.Region}

func qolSHA(i int) string { return fmt.Sprintf("%064x", 0xc0de0000+i) }

func qolTeam(i int) string { return fmt.Sprintf("QOL%07d", i) }

func qolAdd(t *testing.T, i int, ruleType types.RuleType, policy types.Policy) {
	t.Helper()
	id := qolSHA(i)
	if ruleType == types.RuleTypeTeamID {
		id = qolTeam(i)
	}
	if err := globalrules.AddNewGlobalRule(clock.ConcreteTimeProvider{}, ddblocal.Client(t), id, ruleType, policy, "qol"); err != nil {
		t.Fatal(err)
	}
}

func TestQOL_ListShowsEveryGlobalRule(t *testing.T) {
	ddblocal.Reset(t)
	const n = 45 // more than the 40 the list used to stop at
	for i := 0; i < n; i++ {
		qolAdd(t, i, types.RuleTypeBinary, types.RulePolicyBlocklist)
	}
	out, err := clitest.Run(t, qolEnv, RulesCmd, "", "--global")
	if err != nil {
		t.Fatalf("rules --global: %v\n%s", err, out)
	}
	missing := 0
	for i := 0; i < n; i++ {
		if !strings.Contains(out, qolSHA(i)) {
			missing++
		}
	}
	if missing != 0 {
		t.Errorf("%d of %d global rules are not in the list", missing, n)
	}
	if strings.Contains(out, "more than") {
		t.Errorf("the list stopped early:\n%s", out)
	}
}

func TestQOL_ListFiltersByPolicyAndType(t *testing.T) {
	ddblocal.Reset(t)
	qolAdd(t, 1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	qolAdd(t, 2, types.RuleTypeBinary, types.RulePolicyAllowlist)
	qolAdd(t, 3, types.RuleTypeTeamID, types.RulePolicyAllowlist)

	out, err := clitest.Run(t, qolEnv, RulesCmd, "", "--global", "--policy", "blocklist")
	if err != nil {
		t.Fatalf("rules --global --policy blocklist: %v\n%s", err, out)
	}
	if !strings.Contains(out, qolSHA(1)) || strings.Contains(out, qolSHA(2)) || strings.Contains(out, qolTeam(3)) {
		t.Errorf("--policy blocklist listed the wrong rules:\n%s", out)
	}

	out, err = clitest.Run(t, qolEnv, RulesCmd, "", "--global", "--type", "teamid")
	if err != nil {
		t.Fatalf("rules --global --type teamid: %v\n%s", err, out)
	}
	if !strings.Contains(out, qolTeam(3)) || strings.Contains(out, qolSHA(1)) || strings.Contains(out, qolSHA(2)) {
		t.Errorf("--type teamid listed the wrong rules:\n%s", out)
	}
}

func TestQOL_ListRefusesBothTargets(t *testing.T) {
	ddblocal.Reset(t)
	out, err := clitest.Run(t, qolEnv, RulesCmd, "", "--global", "-m", "AAAAAAAA-1111-4111-8111-AAAAAAAAAAAA")
	if err == nil {
		t.Errorf("--global and --machine together were accepted:\n%s", out)
	}
}

func TestQOL_ExportRefusesAnUnknownFormat(t *testing.T) {
	ddblocal.Reset(t)
	qolAdd(t, 1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	file := filepath.Join(t.TempDir(), "rules.out")
	out, err := clitest.Run(t, qolEnv, RulesCmd, "", "export", "-f", file, "--fileformat", "yaml")
	if err == nil {
		t.Errorf("export with --fileformat yaml succeeded:\n%s", out)
	}
	if _, statErr := os.Stat(file); statErr == nil {
		t.Error("a file was written for an unknown format")
	}
}
