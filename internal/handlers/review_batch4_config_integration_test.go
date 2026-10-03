//go:build integration

package handlers

// Regression tests for the independent review of batch 4's configuration fixes (B5, B6, INH).

import (
	"strings"
	"testing"

	"github.com/airbnb/rudolph/internal/testutil/fakesanta"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
	"github.com/airbnb/rudolph/pkg/types"
)

// Review finding 1: earlier versions accepted `config set --global --clean-sync` and stored the flag. A global
// clean-sync flag must never reach Macs, whether or not they have a machine config of their own.
func TestReview_LegacyGlobalCleanSyncFlagNeverReachesMacs(t *testing.T) {
	transport := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	if err := configService(t).SetGlobalConfig(machineconfiguration.GetUniversalDefaultConfig()); err != nil {
		t.Fatal(err)
	}
	updateItem(t, "GlobalConfig", "Config", map[string]any{"CleanSync": true}) // as written by an earlier version
	handlers = newHandlers()

	withoutRow := newHost(t, machineA, transport)
	withRow := newHost(t, machineB, transport)
	if _, err := configService(t).UpdateMachineConfig(machineB, machineconfiguration.MachineConfigurationUpdateRequest{BatchSize: intPtr(20)}); err != nil {
		t.Fatal(err)
	}
	for _, host := range []*fakesanta.Host{withoutRow, withRow} {
		host.MustSync(t) // a new Mac's first sync is clean
		for i := 0; i < 3; i++ {
			if res := host.MustSync(t); res.SyncType != "NORMAL" {
				t.Fatalf("%s: sync %d after the first was %s: a stored global clean-sync flag reached the Mac", host.MachineID, i+2, res.SyncType)
			}
		}
	}
}

// Review finding 2: Santa 2026.8 does not remember that the server asked for a clean sync. If the clean sync an admin
// requested for a Mac does not complete, the next sync must be clean again.
func TestReview_MachineCleanSyncSurvivesAnIncompleteSync(t *testing.T) {
	base := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	fail := false
	host := newHost(t, machineA, failingStage(base, "ruledownload", 503, &fail))
	host.MustSync(t)
	host.MustSync(t)
	host.Rules[fakesanta.RuleKey{Type: "BINARY", Identifier: sha2}] = fakesanta.Rule{Policy: "ALLOWLIST"} // drifted

	if _, err := configService(t).UpdateMachineConfig(machineA, machineconfiguration.MachineConfigurationUpdateRequest{CleanSync: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}
	fail = true
	if _, err := host.Sync(t); err == nil {
		t.Fatal("precondition: the sync should fail at rule download")
	}
	fail = false

	if res := host.MustSync(t); res.SyncType != "CLEAN" {
		t.Fatalf("the requested clean sync never completed, but the next sync was %s", res.SyncType)
	}
	if host.Policy("BINARY", sha2) != "" {
		t.Fatal("the drifted rule survived: the requested clean sync was lost")
	}
	for i := 0; i < 2; i++ {
		if res := host.MustSync(t); res.SyncType != "NORMAL" {
			t.Fatalf("after the clean sync completed, sync %d was %s", i+1, res.SyncType)
		}
	}
}

// Review finding 6: when a stored remount flag list is dropped as invalid, block_usb_mount must not be sent on its
// own either: Santa would then block removable media instead of remounting it, which is stricter than configured.
func TestReview_DroppedRemountFlagsDoNotTurnRemountIntoBlock(t *testing.T) {
	transport := freshServer(t)
	if err := configService(t).SetGlobalConfig(machineconfiguration.GetUniversalDefaultConfig()); err != nil {
		t.Fatal(err)
	}
	updateItem(t, "GlobalConfig", "Config", map[string]any{"BlockUsbMount": true, "RemountUsbMode": manyFlags(40)})
	handlers = newHandlers()
	p := preflightOf(t, newHost(t, machineA, transport).MustSync(t))
	if strings.Contains(p, `"block_usb_mount":true`) && !strings.Contains(p, `"remount_usb_mode"`) {
		t.Fatalf("block_usb_mount sent without its remount flags: %s", p)
	}
}
