//go:build integration

package config

// Quality-of-life gaps of `rudolph config` from the plan review of 2026-11-06 (plan B11, C.10). They fail until the
// CLI batch lands: `config update` can set every setting that `--inherit` can unset again (batch size, bundles,
// transitive rules, full sync interval, upload logs), and --global with --machine is refused instead of silently
// taking --global.

import (
	"testing"

	"github.com/airbnb/rudolph/internal/testutil/clitest"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestQOL_ConfigGetRefusesBothTargets(t *testing.T) {
	ddblocal.Reset(t)
	if out, err := clitest.Run(t, env, ConfigCmd, "yes\n", "set", "--global", "-c", "monitor"); err != nil {
		t.Fatalf("set --global: %v\n%s", err, out)
	}
	out, err := clitest.Run(t, env, ConfigCmd, "", "get", "--global", "-m", machine)
	if err == nil {
		t.Errorf("get with --global and --machine was accepted:\n%s", out)
	}
}

func TestQOL_ConfigUpdateCanSetWhatInheritCanUnset(t *testing.T) {
	ddblocal.Reset(t)
	if out, err := clitest.Run(t, env, ConfigCmd, "yes\n", "set", "--global", "-c", "monitor"); err != nil {
		t.Fatalf("set --global: %v\n%s", err, out)
	}
	out, err := clitest.Run(t, env, ConfigCmd, "yes\n", "update", "-m", machine, "--batch-size", "7")
	if err != nil {
		t.Fatalf("config update -m --batch-size: %v\n%s", err, out)
	}
	got, _ := configRow(t, "Machine#"+machine)["BatchSize"].(*awstypes.AttributeValueMemberN)
	if got == nil || got.Value != "7" {
		t.Errorf("BatchSize on the machine row = %v, want 7", got)
	}

	out, err = clitest.Run(t, env, ConfigCmd, "yes\n", "update", "-m", machine, "--inherit", "batch-size")
	if err != nil {
		t.Fatalf("config update --inherit batch-size: %v\n%s", err, out)
	}
	if _, present := configRow(t, "Machine#"+machine)["BatchSize"]; present {
		t.Error("BatchSize is still on the machine row after --inherit batch-size")
	}
}
