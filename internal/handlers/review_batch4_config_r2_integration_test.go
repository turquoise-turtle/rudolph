//go:build integration

package handlers

// Found by the second review of batch 4's machine config fixes: requested clean syncs that are forgotten, and remount
// flags of the wrong type that turn into an outright USB block.

import (
	"strings"
	"testing"

	"github.com/airbnb/rudolph/internal/testutil/fakesanta"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
	"github.com/airbnb/rudolph/pkg/types"
)

// A clean sync flag set again by hand (no new request ID) after an earlier requested clean sync completed must still
// be performed: the earlier request's ID must not confirm it.
func TestReview_CleanSyncFlagSetAgainAfterACompletedRequestIsPerformed(t *testing.T) {
	transport := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	host := newHost(t, machineA, transport)
	host.MustSync(t)
	host.MustSync(t)
	if _, err := configService(t).UpdateMachineConfig(machineA, machineconfiguration.MachineConfigurationUpdateRequest{CleanSync: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}
	if res := host.MustSync(t); res.SyncType != "CLEAN" {
		t.Fatalf("precondition: the requested clean sync happens, got %s", res.SyncType)
	}
	updateItem(t, "Machine#"+machineA, "Config", map[string]any{"CleanSync": true})
	host.Rules[fakesanta.RuleKey{Type: "BINARY", Identifier: sha2}] = fakesanta.Rule{Policy: "ALLOWLIST"}
	if res := host.MustSync(t); res.SyncType != "CLEAN" {
		t.Errorf("a clean sync flag set again was not performed: %s", res.SyncType)
	}
}

// A Mac whose postflights arrive does not have its requested clean sync forgotten because three of them failed after
// the last page was served: only a postflight or a changed rules hash shows it happened.
func TestReview_HealthyMacKeepsARequestedCleanSyncThatKeepsFailing(t *testing.T) {
	base := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	lose := map[string]int{}
	host := newHost(t, machineA, losingResponses(base, lose))
	host.Retries = true
	host.HashRules = true
	host.MustSync(t)
	host.MustSync(t)
	host.Rules[fakesanta.RuleKey{Type: "BINARY", Identifier: sha2}] = fakesanta.Rule{Policy: "ALLOWLIST"}
	if _, err := configService(t).UpdateMachineConfig(machineA, machineconfiguration.MachineConfigurationUpdateRequest{CleanSync: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		lose["ruledownload"] = fakesanta.RetryAttempts
		if _, err := host.Sync(t); err == nil {
			t.Fatalf("precondition: sync %d fails", i+1)
		}
	}
	lose["ruledownload"] = 0
	res := host.MustSync(t)
	if res.SyncType != "CLEAN" || host.Policy("BINARY", sha2) != "" {
		t.Errorf("the requested clean sync was forgotten: next sync %s, drifted rule %q", res.SyncType, host.Policy("BINARY", sha2))
	}
}

// Remount flags stored as a list of non-strings cannot be sent, so block_usb_mount must not go out alone either.
func TestReview_RemountFlagsOfTheWrongTypeDoNotTurnRemountIntoBlock(t *testing.T) {
	transport := freshServer(t)
	if err := configService(t).SetGlobalConfig(machineconfiguration.GetUniversalDefaultConfig()); err != nil {
		t.Fatal(err)
	}
	updateItem(t, "GlobalConfig", "Config", map[string]any{"BlockUsbMount": true, "RemountUsbMode": []any{1, 2}})
	handlers = newHandlers()
	p := preflightOf(t, newHost(t, machineA, transport).MustSync(t))
	if strings.Contains(p, `"block_usb_mount":true`) && !strings.Contains(p, `"remount_usb_mode"`) {
		t.Errorf("block_usb_mount sent without its remount flags: %s", p)
	}
}
