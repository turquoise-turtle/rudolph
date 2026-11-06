//go:build integration

package handlers

// Zero-rule clean syncs (owner request, 2026-11-06). Preflight forces a clean sync when a Mac reports holding no rules,
// so that a Mac whose rule database was wiped gets its rules back. But Santa uploads no events during a clean sync, so
// with no rules for the Mac at all (a fresh monitor-mode rollout, collecting unknown executions before writing rules)
// every sync was clean and no event ever reached Rudolph.
//
// Config setting MonitorEventsWithoutRules (bool; global or per machine, inherited like the others; unset means on):
// when on, a MONITOR-mode Mac reporting zero rules is clean synced only if Rudolph holds rules for it (any global
// rule, or a machine rule of that Mac), so it syncs normally and uploads its events otherwise. When off, the old
// behaviour: every zero-rule preflight is a clean sync. LOCKDOWN Macs keep the old behaviour either way. CLI:
// `config set|update ... --monitor-events-without-rules=true|false`, `config update --machine <id> --inherit
// monitor-events-without-rules`.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/fakesanta"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
	"github.com/airbnb/rudolph/pkg/types"
)

// zeroRuleHost is a monitor-mode Mac that uploads events like Santa (not during clean syncs) and has one pending.
func zeroRuleHost(t *testing.T, id string, transport fakesanta.Transport) *fakesanta.Host {
	t.Helper()
	h := newHost(t, id, transport)
	h.SantaExactEventUpload = true
	h.PendingEvents = []string{fmt.Sprintf(`{"file_sha256":%q,"file_name":"tool","decision":"ALLOW_UNKNOWN","pid":1,"execution_time":%d}`,
		strings.Repeat("f6", 32), time.Now().Add(-time.Minute).Unix())}
	return h
}

// syncTypes runs n syncs and returns their sync types.
func syncTypes(t *testing.T, h *fakesanta.Host, n int) []string {
	t.Helper()
	var got []string
	for i := 0; i < n; i++ {
		got = append(got, h.MustSync(t).SyncType)
	}
	return got
}

func setGlobalMode(t *testing.T, mode types.ClientMode) {
	t.Helper()
	cfg := machineconfiguration.GetUniversalDefaultConfig()
	cfg.ClientMode = mode
	if err := configService(t).SetGlobalConfig(cfg, machineconfiguration.ConfirmGlobalLockdown()); err != nil {
		t.Fatal(err)
	}
}

func TestZeroRules_MonitorMacWithoutRulesSyncsNormallyAndUploadsEvents(t *testing.T) {
	transport := freshServer(t)
	h := zeroRuleHost(t, machineA, transport)
	var got []string
	uploaded := 0
	for i := 0; i < 3; i++ {
		res := h.MustSync(t)
		got = append(got, res.SyncType)
		uploaded += res.EventsUploaded
		if i == 0 && len(h.PendingEvents) == 0 {
			// Santa deletes pending events without uploading them during a clean first sync; queue one again.
			h.PendingEvents = zeroRuleHost(t, machineA, transport).PendingEvents
		}
	}
	if got[1] != "NORMAL" || got[2] != "NORMAL" {
		t.Errorf("sync types %v: with no rules for the Mac, later syncs should be NORMAL", got)
	}
	if uploaded == 0 {
		t.Errorf("no event was uploaded in 3 syncs (sync types %v)", got)
	}
}

func TestZeroRules_LockdownMacWithoutRulesStillCleanSyncs(t *testing.T) {
	transport := freshServer(t)
	setGlobalMode(t, types.Lockdown)
	h := zeroRuleHost(t, machineA, transport)
	h.ClientMode = "LOCKDOWN"
	if got := syncTypes(t, h, 3); got[1] != "CLEAN" || got[2] != "CLEAN" {
		t.Errorf("lockdown sync types %v, want every zero-rule preflight CLEAN as before", got)
	}
}

func TestZeroRules_SettingOffRestoresCleanSyncs(t *testing.T) {
	transport := freshServer(t)
	setGlobalMode(t, types.Monitor)
	updateItem(t, "GlobalConfig", "Config", map[string]any{"MonitorEventsWithoutRules": false})
	h := zeroRuleHost(t, machineA, transport)
	if got := syncTypes(t, h, 3); got[1] != "CLEAN" || got[2] != "CLEAN" {
		t.Errorf("setting off globally: sync types %v, want CLEAN as before", got)
	}
}

func TestZeroRules_SettingIsPerMachine(t *testing.T) {
	transport := freshServer(t)
	setGlobalMode(t, types.Monitor)
	cfg := machineconfiguration.GetUniversalDefaultConfig()
	cfg.ClientMode = types.Monitor
	if err := configService(t).SetMachineConfig(machineA, cfg); err != nil {
		t.Fatal(err)
	}
	updateItem(t, "Machine#"+machineA, "Config", map[string]any{"MonitorEventsWithoutRules": false})

	a := zeroRuleHost(t, machineA, transport)
	b := zeroRuleHost(t, machineB, transport)
	if got := syncTypes(t, a, 3); got[2] != "CLEAN" {
		t.Errorf("machine A (setting off on its own config) sync types %v, want CLEAN", got)
	}
	if got := syncTypes(t, b, 3); got[2] != "NORMAL" {
		t.Errorf("machine B (inherits the default) sync types %v, want NORMAL", got)
	}
}

// A Mac reporting zero rules while Rudolph has rules for it lost its rule database: it gets a clean sync at once.
func TestZeroRules_WipedMonitorMacStillCleanSyncs(t *testing.T) {
	t.Run("global rule", func(t *testing.T) {
		transport := freshServer(t)
		addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
		h := newHost(t, machineA, transport)
		syncTypes(t, h, 2)
		h.Rules = map[fakesanta.RuleKey]fakesanta.Rule{}
		res := h.MustSync(t)
		if res.SyncType != "CLEAN" || h.Policy("BINARY", sha1) != "BLOCKLIST" {
			t.Errorf("wiped Mac: sync %s, sha1 policy %q; want CLEAN and the block rule back", res.SyncType, h.Policy("BINARY", sha1))
		}
	})
	t.Run("machine rule of this Mac", func(t *testing.T) {
		transport := freshServer(t)
		addMachineRule(t, machineA, sha2, types.RuleTypeBinary, types.RulePolicyBlocklist, time.Now().Add(24*time.Hour))
		h := newHost(t, machineA, transport)
		syncTypes(t, h, 2)
		h.Rules = map[fakesanta.RuleKey]fakesanta.Rule{}
		if res := h.MustSync(t); res.SyncType != "CLEAN" {
			t.Errorf("wiped Mac with a machine rule: sync %s, want CLEAN", res.SyncType)
		}
	})
	t.Run("machine rule of another Mac only", func(t *testing.T) {
		transport := freshServer(t)
		addMachineRule(t, machineB, sha2, types.RuleTypeBinary, types.RulePolicyBlocklist, time.Now().Add(24*time.Hour))
		h := zeroRuleHost(t, machineA, transport)
		if got := syncTypes(t, h, 3); got[2] != "NORMAL" {
			t.Errorf("no rules for this Mac (only another Mac's): sync types %v, want NORMAL", got)
		}
	})
}
