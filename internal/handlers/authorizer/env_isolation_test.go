package authorizer

import (
	"os"
	"testing"
)

// The authoriser reads REGION, GATEWAY_ID and ACCOUNT_ID from its Lambda environment and prefers them over the method
// ARN. The tests use method ARNs of their own, so they must not inherit an operator's shell (the production runbook
// exports REGION before `make test`).
func TestMain(m *testing.M) {
	for _, k := range []string{"REGION", "GATEWAY_ID", "ACCOUNT_ID"} {
		os.Unsetenv(k)
	}
	os.Exit(m.Run())
}
