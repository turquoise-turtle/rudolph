//go:build integration

package machine

// `rudolph machine show` says who requested a pending clean sync. The fleet job's request (for a rules hash outlier,
// machineconfiguration.RequestFleetCleanSync) shows as
//
//	  Clean sync:      requested by the fleet job (rules hash differed)
//
// A manual request afterwards (`rudolph config update --clean-sync`) replaces CleanSyncRequest, and then shows plain
// "requested" again. Once the clean sync is performed, no clean sync line is shown, whoever requested it.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/cli/config"
	"github.com/airbnb/rudolph/internal/testutil/clitest"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
)

const (
	fleetRequestedLine = "  Clean sync:      requested by the fleet job (rules hash differed)"
	plainRequestedLine = "  Clean sync:      requested"
)

func showLines(t *testing.T, id string) map[string]bool {
	t.Helper()
	out, err := clitest.Run(t, env(), MachineCmd, "", "show", "-m", id)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	lines := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		lines[strings.TrimRight(line, " ")] = true
	}
	return lines
}

func TestMachineShow_CleanSyncRequestedByTheFleetJob(t *testing.T) {
	ddblocal.Reset(t)
	seedMac(t, macA, "lab-mac-01", "C02ABC123", time.Now())
	request, err := machineconfiguration.RequestFleetCleanSync(context.Background(), ddblocal.Raw(t), ddblocal.TableName, macA, "none", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	lines := showLines(t, macA)
	if !lines[fleetRequestedLine] {
		t.Errorf("no line %q", fleetRequestedLine)
	}
	if lines[plainRequestedLine] {
		t.Errorf("the fleet job's request shows as %q", plainRequestedLine)
	}

	// A manual request after it is a manual request.
	if out, err := clitest.Run(t, env(), config.ConfigCmd, "", "update", "--machine", macA, "--clean-sync", "--yes"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	lines = showLines(t, macA)
	if !lines[plainRequestedLine] || lines[fleetRequestedLine] {
		t.Errorf("after a manual request: plain line %v, fleet job line %v; want the plain one only", lines[plainRequestedLine], lines[fleetRequestedLine])
	}

	// Performed: no clean sync line.
	current, err := machineconfiguration.GetMachineSpecificConfig(ddblocal.Client(t), macA)
	if err != nil {
		t.Fatal(err)
	}
	if current.CleanSyncRequest == request {
		t.Fatal("the manual request kept the fleet job's CleanSyncRequest")
	}
	if err := machineconfiguration.ClearMachineCleanSync(ddblocal.Client(t), macA, current.CleanSyncRequest); err != nil {
		t.Fatal(err)
	}
	for line := range showLines(t, macA) {
		if strings.Contains(line, "Clean sync:") {
			t.Errorf("a performed clean sync still shows %q", line)
		}
	}
}

// Performed fleet job request: the record of it stays on the config, but nothing is requested.
func TestMachineShow_PerformedFleetCleanSyncShowsNoRequest(t *testing.T) {
	ddblocal.Reset(t)
	seedMac(t, macA, "lab-mac-01", "C02ABC123", time.Now())
	request, err := machineconfiguration.RequestFleetCleanSync(context.Background(), ddblocal.Raw(t), ddblocal.TableName, macA, "none", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err := machineconfiguration.ClearMachineCleanSync(ddblocal.Client(t), macA, request); err != nil {
		t.Fatal(err)
	}
	for line := range showLines(t, macA) {
		if strings.Contains(line, "Clean sync:") {
			t.Errorf("a performed clean sync still shows %q", line)
		}
	}
}
