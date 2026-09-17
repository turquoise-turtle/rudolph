//go:build integration

package handlers

// Regression test for the final review of the L1-L4/L11/L12/P4 branch.

import (
	"strings"
	"testing"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/internal/testutil/santaproto"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
	"github.com/airbnb/rudolph/pkg/types"
)

// Review finding M1: pinning a machine to the current global value must create its machine-specific config, so a
// later fleet-wide change (e.g. confirmed global lockdown) does not reach it.
func TestReview_PinningMachineToCurrentGlobalValueSurvivesGlobalChange(t *testing.T) {
	transport := freshServer(t)
	svc := machineconfiguration.GetMachineConfigurationService(ddblocal.Client(t), clock.ConcreteTimeProvider{})
	if err := svc.SetGlobalConfig(machineconfiguration.GetUniversalDefaultConfig()); err != nil {
		t.Fatal(err)
	}

	monitor := types.Monitor
	if _, err := svc.UpdateMachineConfig(machineA, machineconfiguration.MachineConfigurationUpdateRequest{ClientMode: &monitor}); err != nil {
		t.Fatal(err)
	}
	if item := getItem(t, "Machine#"+machineA, "Config"); len(item) == 0 {
		t.Fatal("pinning a machine to MONITOR while global is MONITOR did not create a machine-specific config")
	}

	lockdown := types.Lockdown
	if _, err := svc.UpdateGlobalConfig(machineconfiguration.MachineConfigurationUpdateRequest{ClientMode: &lockdown}, machineconfiguration.ConfirmGlobalLockdown()); err != nil {
		t.Fatal(err)
	}
	handlers = newHandlers()

	if got := santaproto.Canonical(t, newHost(t, machineA, transport).MustSync(t).Preflight); !strings.Contains(got, `"client_mode":"MONITOR"`) {
		t.Errorf("pinned machine followed the global change: %s", got)
	}
	if got := santaproto.Canonical(t, newHost(t, machineB, transport).MustSync(t).Preflight); !strings.Contains(got, `"client_mode":"LOCKDOWN"`) {
		t.Errorf("unpinned machine should follow global LOCKDOWN: %s", got)
	}
}
