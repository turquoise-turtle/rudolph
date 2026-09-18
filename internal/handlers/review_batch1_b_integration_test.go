//go:build integration

package handlers

// Regression tests for findings from the independent review of the L1-L4/L11/L12/P4 branch.

import (
	"strings"
	"testing"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/internal/testutil/santaproto"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
)

// Review finding M1: a config row whose RemountUsbMode is a string (hand-edited or older tooling) must not break
// every host's preflight.
func TestReview_LegacyStringRemountUsbModeStillLoads(t *testing.T) {
	transport := freshServer(t)
	svc := machineconfiguration.GetMachineConfigurationService(ddblocal.Client(t), clock.ConcreteTimeProvider{})
	if err := svc.SetGlobalConfig(machineconfiguration.GetUniversalDefaultConfig()); err != nil {
		t.Fatal(err)
	}
	updateItem(t, "GlobalConfig", "Config", map[string]any{"BlockUsbMount": true, "RemountUsbMode": "rdonly,nosuid"})

	res := newHost(t, machineA, transport).MustSync(t)
	if got := santaproto.Canonical(t, res.Preflight); !strings.Contains(got, `"remount_usb_mode":["rdonly","nosuid"]`) {
		t.Fatalf("legacy string RemountUsbMode not sent as a list: %s", got)
	}
}

// Review finding L2: optional Santa settings not configured on a machine-specific config fall back to the global
// config, so machines with an override still get fleet-wide settings added later.
func TestReview_MachineConfigInheritsUnsetOptionalSettingsFromGlobal(t *testing.T) {
	transport := freshServer(t)
	svc := machineconfiguration.GetMachineConfigurationService(ddblocal.Client(t), clock.ConcreteTimeProvider{})
	base := machineconfiguration.GetUniversalDefaultConfig()
	if err := svc.SetGlobalConfig(base); err != nil {
		t.Fatal(err)
	}
	if err := svc.SetMachineConfig(machineA, base); err != nil {
		t.Fatal(err)
	}
	updateItem(t, "GlobalConfig", "Config", map[string]any{
		"EnableAllEventUpload": true, "EventDetailText": "Request access", "OverrideFileAccessAction": "AUDIT_ONLY",
	})
	updateItem(t, "Machine#"+machineA, "Config", map[string]any{"OverrideFileAccessAction": "DISABLE"})
	handlers = newHandlers()

	got := santaproto.Canonical(t, newHost(t, machineA, transport).MustSync(t).Preflight)
	for _, want := range []string{`"enable_all_event_upload":true`, `"event_detail_text":"Request access"`, `"override_file_access_action":"DISABLE"`} {
		if !strings.Contains(got, want) {
			t.Errorf("machine preflight missing %s: %s", want, got)
		}
	}
}

// Review finding L4: a legal legacy spelling stored in DynamoDB must not block unrelated config updates.
func TestReview_LegacyFileAccessActionSpellingDoesNotBlockUpdates(t *testing.T) {
	transport := freshServer(t)
	svc := machineconfiguration.GetMachineConfigurationService(ddblocal.Client(t), clock.ConcreteTimeProvider{})
	if err := svc.SetGlobalConfig(machineconfiguration.GetUniversalDefaultConfig()); err != nil {
		t.Fatal(err)
	}
	updateItem(t, "GlobalConfig", "Config", map[string]any{"OverrideFileAccessAction": "AuditOnly"})

	batch := 40
	if _, err := svc.UpdateGlobalConfig(machineconfiguration.MachineConfigurationUpdateRequest{BatchSize: &batch}); err != nil {
		t.Fatalf("unrelated update blocked by a stored legacy value: %v", err)
	}
	handlers = newHandlers()
	if got := santaproto.Canonical(t, newHost(t, machineA, transport).MustSync(t).Preflight); !strings.Contains(got, `"override_file_access_action":"AUDIT_ONLY"`) {
		t.Fatalf("legacy spelling not normalised in preflight: %s", got)
	}
}
