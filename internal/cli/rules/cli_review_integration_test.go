//go:build integration

package rules

// From the review of the CLI batch: export picks the format from the file's extension when --fileformat is not given,
// so an export round-trips through import; and import, which can add and remove global rules, asks before writing.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/airbnb/rudolph/internal/testutil/clitest"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/pkg/types"
)

func TestReviewQOL_ExportFormatFollowsTheExtensionAndRoundTrips(t *testing.T) {
	ddblocal.Reset(t)
	qolAdd(t, 1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	file := filepath.Join(t.TempDir(), "rules.json")
	if out, err := clitest.Run(t, qolEnv, RulesCmd, "yes\n", "export", "-f", file); err != nil {
		t.Fatalf("export: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if s := strings.TrimSpace(string(raw)); !strings.HasPrefix(s, "[") && !strings.HasPrefix(s, "{") {
		t.Fatalf("rules.json is not JSON:\n%.200s", s)
	}
	ddblocal.Reset(t)
	if out, err := clitest.Run(t, qolEnv, RulesCmd, "yes\n", "import", "-f", file); err != nil {
		t.Fatalf("import of the export: %v\n%s", err, out)
	}
	out, err := clitest.Run(t, qolEnv, RulesCmd, "", "--global")
	if err != nil || !strings.Contains(out, qolSHA(1)) {
		t.Errorf("after the round trip the rule is missing: %v\n%s", err, out)
	}
}

func TestReviewQOL_ImportAsksBeforeWriting(t *testing.T) {
	ddblocal.Reset(t)
	file := filepath.Join(t.TempDir(), "rules.json")
	if err := os.WriteFile(file, []byte(`[{"type":"BINARY","policy":"BLOCKLIST","identifier":"`+qolSHA(2)+`","description":"x"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := clitest.Run(t, qolEnv, RulesCmd, "no\n", "import", "-f", file); err == nil {
		t.Errorf("a declined import succeeded:\n%s", out)
	}
	out, _ := clitest.Run(t, qolEnv, RulesCmd, "", "--global")
	if strings.Contains(out, qolSHA(2)) {
		t.Error("a declined import wrote the rule")
	}
	if out, err := clitest.Run(t, qolEnv, RulesCmd, "", "import", "-f", file, "--yes"); err != nil {
		t.Errorf("import --yes: %v\n%s", err, out)
	}
}
