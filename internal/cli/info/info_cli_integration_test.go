//go:build integration

package info

import (
	"os"
	"strings"
	"testing"

	"github.com/airbnb/rudolph/internal/cli/config"
	"github.com/airbnb/rudolph/internal/testutil/clitest"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
)

func TestMain(m *testing.M) { os.Exit(ddblocal.Main(m)) }

const self = "DDDDDDDD-4444-4444-8444-DDDDDDDDDDDD"

func useSelf(t *testing.T) {
	t.Helper()
	old := selfMachineID
	selfMachineID = func() (string, error) { return self, nil }
	t.Cleanup(func() { selfMachineID = old })
}

// `rudolph info` on a Mac prints its machine ID and, with a deployment, the configuration Rudolph sends it
// (plan C.13; the old FIXME in info.go).
func TestInfo_PrintsTheRemoteConfiguration(t *testing.T) {
	ddblocal.Reset(t)
	useSelf(t)
	env := clitest.Env{Table: ddblocal.TableName, Region: ddblocal.Region}
	if out, err := clitest.Run(t, env, config.ConfigCmd, "", "update", "--machine", self, "-c", "lockdown", "--for", "1h", "--yes"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	out, err := clitest.Run(t, env, InfoCmd, "")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{self, "lockdown"} {
		if !strings.Contains(strings.ToLower(out), strings.ToLower(want)) {
			t.Errorf("info output does not contain %q:\n%s", want, out)
		}
	}
}

// Without a deployment to read, info still prints the machine ID, as before.
func TestInfo_WithoutADeploymentPrintsTheMachineID(t *testing.T) {
	useSelf(t)
	t.Setenv("ENV", "")
	t.Setenv("REGION", "")
	t.Setenv("DYNAMODB_NAME", "")
	out, err := clitest.Run(t, clitest.Env{}, InfoCmd, "")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, self) {
		t.Errorf("info output does not contain the machine ID:\n%s", out)
	}
}
