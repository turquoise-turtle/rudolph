//go:build integration

package config

// Regression tests for the independent review of batch 4's configuration fixes: requesting a clean sync for one
// Mac, and returning one machine setting to the global value, must not pin or clear the Mac's other settings.

import (
	"testing"

	"github.com/airbnb/rudolph/internal/testutil/clitest"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Review finding 3: `config update --machine <id> --clean-sync` requests one clean sync and stores nothing else.
func TestReview_ConfigUpdateCleanSyncTouchesNothingElse(t *testing.T) {
	ddblocal.Reset(t)
	if out, err := clitest.Run(t, env, ConfigCmd, "yes\n", "update", "--machine", machine, "--clean-sync"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	row := configRow(t, "Machine#"+machine)
	if v, ok := row["CleanSync"].(*awstypes.AttributeValueMemberBOOL); !ok || !v.Value {
		t.Fatalf("CleanSync not stored as true: %v", row)
	}
	for _, name := range []string{"ClientMode", "AllowedPathRegex", "BlockedPathRegex", "BatchSize", "FullSyncInterval", "EnableBundles", "EnableTransitiveRules"} {
		if _, ok := row[name]; ok {
			t.Errorf("requesting a clean sync also stored %s, pinning it for this machine", name)
		}
	}
	if _, err := clitest.Run(t, env, ConfigCmd, "yes\n", "update", "--global", "--clean-sync"); err == nil {
		t.Error("a global clean sync must be refused by update too")
	}
}

// Review finding 4: one machine override can be returned to the global value without dropping the others.
func TestReview_ConfigUpdateInheritRemovesOneOverride(t *testing.T) {
	ddblocal.Reset(t)
	if out, err := clitest.Run(t, env, ConfigCmd, "yes\n", "update", "--machine", machine, "--allowed-paths", "^/opt/a/", "--blocked-paths", "^/opt/b/"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err := clitest.Run(t, env, ConfigCmd, "yes\n", "update", "--machine", machine, "--inherit", "allowed-paths"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	row := configRow(t, "Machine#"+machine)
	if _, ok := row["AllowedPathRegex"]; ok {
		t.Errorf("--inherit allowed-paths left the override in place: %v", row["AllowedPathRegex"])
	}
	if v, ok := row["BlockedPathRegex"].(*awstypes.AttributeValueMemberS); !ok || v.Value != "^/opt/b/" {
		t.Errorf("--inherit allowed-paths also changed the blocked paths override: %v", row["BlockedPathRegex"])
	}
	if _, err := clitest.Run(t, env, ConfigCmd, "yes\n", "update", "--machine", machine, "--inherit", "no-such-setting"); err == nil {
		t.Error("an unknown setting name for --inherit must be refused")
	}
	if _, err := clitest.Run(t, env, ConfigCmd, "yes\n", "update", "--global", "--inherit", "allowed-paths"); err == nil {
		t.Error("--inherit only makes sense for a machine")
	}
}
