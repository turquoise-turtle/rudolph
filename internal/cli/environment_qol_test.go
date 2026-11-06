package cli

// The CLI finds the environment file where the repo keeps it (plan review of 2026-11-06, B12):
// deployments/environments/<env>/config.auto.tfvars.json from the repo root, honours the store module's
// dynamodb_table_name override, and refuses an empty region instead of handing it to the SDK. Fails until fixed.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func qolEnvCommand(t *testing.T, env string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "x"}
	cmd.Flags().String("ENV", "", "environment")
	cmd.Flags().String("region", "", "region")
	cmd.Flags().String("prefix", "", "prefix")
	cmd.Flags().String("dynamodb_table", "", "table")
	if err := cmd.Flags().Set("ENV", env); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func qolRepoWithEnvironment(t *testing.T, env, tfvars string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "deployments", "environments", env)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.auto.tfvars.json"), []byte(tfvars), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	t.Setenv("HOME", root) // no ~/.rudolph-cli fallback can answer instead
}

func TestQOL_EnvironmentFileIsFoundUnderDeployments(t *testing.T) {
	qolRepoWithEnvironment(t, "qa", `{"prefix":"qa1","region":"ap-southeast-2","aws_account_id":"123456789012"}`)
	cmd := qolEnvCommand(t, "qa")
	cfg, err := retrieveConfig(cmd)
	if err != nil {
		t.Fatalf("retrieveConfig from the repo root: %v", err)
	}
	if cfg.Prefix != "qa1" || cfg.Region != "ap-southeast-2" {
		t.Errorf("read prefix %q and region %q, want qa1 and ap-southeast-2", cfg.Prefix, cfg.Region)
	}
	if got, _ := cmd.Flags().GetString("dynamodb_table"); got != "qa1_rudolph_store" {
		t.Errorf("dynamodb_table = %q, want qa1_rudolph_store", got)
	}
}

func TestQOL_EnvironmentFileTableNameOverrideIsHonoured(t *testing.T) {
	qolRepoWithEnvironment(t, "qa", `{"prefix":"qa1","region":"ap-southeast-2","dynamodb_table_name":"santa-rules"}`)
	cmd := qolEnvCommand(t, "qa")
	if _, err := retrieveConfig(cmd); err != nil {
		t.Fatalf("retrieveConfig: %v", err)
	}
	if got, _ := cmd.Flags().GetString("dynamodb_table"); got != "santa-rules" {
		t.Errorf("dynamodb_table = %q, want the dynamodb_table_name override santa-rules", got)
	}
}

func TestQOL_EnvironmentFileWithoutARegionIsAnError(t *testing.T) {
	qolRepoWithEnvironment(t, "qa", `{"prefix":"qa1"}`)
	_, err := retrieveConfig(qolEnvCommand(t, "qa"))
	// The file must be found first (the temp dir's name mentions this test, so match the message, not the path).
	if err == nil || strings.Contains(err.Error(), "Not Found") || !strings.Contains(strings.ToLower(err.Error()), "region is") {
		t.Errorf("an environment file without a region gave err %v, want an error saying the region is empty", err)
	}
}
