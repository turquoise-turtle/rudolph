package machineconfiguration

import (
	"testing"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/types"
)

// Review finding M3: the global confirmation must fail closed for every mode other than MONITOR, including
// modes added later (STANDALONE is the next ClientMode value).
func TestReview_GlobalConfirmationFailsClosed(t *testing.T) {
	if RequiresGlobalLockdownConfirmation(types.Monitor) {
		t.Error("MONITOR must not require confirmation")
	}
	for _, mode := range []types.ClientMode{types.Lockdown, types.ClientMode(3), types.ClientMode(42)} {
		if !RequiresGlobalLockdownConfirmation(mode) {
			t.Errorf("client mode %d must require confirmation", mode)
		}
	}
}

// Review finding L1 (P4): Santa only reads remount flags together with block_usb_mount, so storing remount flags
// without blocking USB mounts must be rejected instead of silently doing nothing.
func TestReview_RemountFlagsRequireBlockUsbMount(t *testing.T) {
	store := newFakeStore()
	svc := GetMachineConfigurationService(fakeClient{s: store}, clock.FrozenTimeProvider{Current: clock.Y2KTime()})
	cfg := GetUniversalDefaultConfig()
	cfg.RemountUsbMode = []string{"rdonly"}
	if err := svc.SetGlobalConfig(cfg); err == nil {
		t.Error("remount flags without block_usb_mount were accepted")
	}
	blocked := true
	cfg.BlockUsbMount = &blocked
	if err := svc.SetGlobalConfig(cfg); err != nil {
		t.Errorf("remount flags with block_usb_mount rejected: %v", err)
	}

	id := "0F0E0D0C-0B0A-4908-8706-050403020100"
	flags := []string{"noexec"}
	if _, err := svc.UpdateMachineConfig(id, MachineConfigurationUpdateRequest{RemountUsbMode: &flags}); err != nil {
		t.Errorf("machine inherits block_usb_mount from global, remount update should be accepted: %v", err)
	}
}
