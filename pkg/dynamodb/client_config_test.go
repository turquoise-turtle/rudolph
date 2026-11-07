package dynamodb

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// GetClient used to log.Fatalf when the AWS configuration could not be loaded, which bypassed cobra's error
// handling (plan C.12); and GetClientWithTimeout ignored its timeout.
func TestGetClient_ReturnsConfigurationErrors(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(cfg, []byte("[default]\nregion = us-east-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", cfg)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "credentials"))
	t.Setenv("AWS_PROFILE", "rudolph-test-no-such-profile")

	if _, err := GetClient("table", "us-east-1"); err == nil {
		t.Error("GetClient with a missing profile returned no error")
	}
	if _, err := GetClientWithTimeout("table", "us-east-1", time.Second); err == nil {
		t.Error("GetClientWithTimeout with a missing profile returned no error")
	}
}

func TestGetClientWithTimeout_UsesTheTimeout(t *testing.T) {
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	c, err := GetClientWithTimeout("table", "us-east-1", 1234*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	cc, ok := c.(concreteDynamoDBClient)
	if !ok {
		t.Fatalf("GetClientWithTimeout returned %T", c)
	}
	if cc.timeout != 1234*time.Millisecond {
		t.Errorf("timeout %v, want 1.234s", cc.timeout)
	}
}
