package lambda

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestGetClient(t *testing.T) {
	// Hermetic regardless of the operator's shell (the runbook exports AWS_PROFILE).
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_DEFAULT_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "credentials"))
	client := GetClient(
		"rudolph-test",
		"dev",
		"us-east-1",
	)

	assert.NotNil(t, client)
}
