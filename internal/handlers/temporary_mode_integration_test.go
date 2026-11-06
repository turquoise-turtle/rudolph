//go:build integration

package handlers

// Temporary client modes per Mac (owner request, 2026-11-06). A machine's configuration row may hold
// TemporaryClientMode (number, like ClientMode) and TemporaryClientModeExpiresAt (Unix seconds). Until that time,
// preflight sends the temporary mode instead of the mode the Mac would otherwise get (its own or the inherited global
// one); after it, the temporary mode is ignored and the Mac returns to that mode at its next sync. Monitor, lockdown
// and standalone can all be temporary. A temporary mode that cannot be decoded is ignored, like any mistyped setting.
// The CLI side is internal/cli/config/temporary_mode_cli_integration_test.go.

import (
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/santaproto"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
	"github.com/airbnb/rudolph/pkg/types"
	"google.golang.org/protobuf/types/dynamicpb"
)

func setGlobalClientMode(t *testing.T, mode types.ClientMode) {
	t.Helper()
	cfg := machineconfiguration.GetUniversalDefaultConfig()
	cfg.ClientMode = mode
	if err := configService(t).SetGlobalConfig(cfg, machineconfiguration.ConfirmGlobalLockdown()); err != nil {
		t.Fatal(err)
	}
}

// setTemporaryMode writes the temporary mode attributes on the machine's configuration row, creating a row that holds
// only them (a partial row: every other setting is inherited) when the machine has none.
func setTemporaryMode(t *testing.T, machineID string, mode types.ClientMode, expires time.Time) {
	t.Helper()
	attrs := map[string]any{"TemporaryClientMode": int(mode), "TemporaryClientModeExpiresAt": expires.Unix()}
	if len(getItem(t, "Machine#"+machineID, "Config")) == 0 {
		attrs["PK"] = "Machine#" + machineID
		attrs["SK"] = "Config"
		attrs["DataType"] = "MachineConfiguration"
		putItem(t, attrs)
		return
	}
	updateItem(t, "Machine#"+machineID, "Config", attrs)
}

func TestTemporaryMode_AppliesUntilItExpires(t *testing.T) {
	for _, c := range []struct {
		name            string
		global, temp    types.ClientMode
		wantDuring      string
		wantAfterExpiry string
	}{
		{"monitor on a lockdown fleet", types.Lockdown, types.Monitor, "MONITOR", "LOCKDOWN"},
		{"lockdown on a monitor fleet", types.Monitor, types.Lockdown, "LOCKDOWN", "MONITOR"},
		{"standalone on a monitor fleet", types.Monitor, types.Standalone, "STANDALONE", "MONITOR"},
	} {
		t.Run(c.name, func(t *testing.T) {
			transport := freshServer(t)
			setGlobalClientMode(t, c.global)
			setTemporaryMode(t, machineA, c.temp, time.Now().Add(2*time.Hour))
			a := newHost(t, machineA, transport)
			b := newHost(t, machineB, transport)

			if got := sentMode(t, a.MustSync(t).Preflight); got != c.wantDuring {
				t.Errorf("during the temporary mode: %s, want %s", got, c.wantDuring)
			}
			if got := sentMode(t, b.MustSync(t).Preflight); got != c.wantAfterExpiry {
				t.Errorf("another Mac: %s, want the global %s", got, c.wantAfterExpiry)
			}

			setTemporaryMode(t, machineA, c.temp, time.Now().Add(-time.Minute))
			handlers = newHandlers() // a config cache must not keep an expired temporary mode either
			if got := sentMode(t, a.MustSync(t).Preflight); got != c.wantAfterExpiry {
				t.Errorf("after expiry: %s, want %s", got, c.wantAfterExpiry)
			}
		})
	}
}

// The temporary mode overrides a mode set on the machine's own configuration too, and the machine's own mode comes
// back afterwards.
func TestTemporaryMode_OverridesTheMachinesOwnMode(t *testing.T) {
	transport := freshServer(t)
	setGlobalClientMode(t, types.Monitor)
	cfg := machineconfiguration.GetUniversalDefaultConfig()
	cfg.ClientMode = types.Lockdown
	if err := configService(t).SetMachineConfig(machineA, cfg); err != nil {
		t.Fatal(err)
	}
	setTemporaryMode(t, machineA, types.Monitor, time.Now().Add(time.Hour))
	a := newHost(t, machineA, transport)
	if got := sentMode(t, a.MustSync(t).Preflight); got != "MONITOR" {
		t.Errorf("during: %s, want MONITOR", got)
	}
	setTemporaryMode(t, machineA, types.Monitor, time.Now().Add(-time.Second))
	handlers = newHandlers()
	if got := sentMode(t, a.MustSync(t).Preflight); got != "LOCKDOWN" {
		t.Errorf("after: %s, want the machine's own LOCKDOWN", got)
	}
}

func TestTemporaryMode_UndecodableValuesAreIgnored(t *testing.T) {
	for name, attrs := range map[string]map[string]any{
		"mode is a string":   {"TemporaryClientMode": "MONITOR", "TemporaryClientModeExpiresAt": time.Now().Add(time.Hour).Unix()},
		"unknown mode":       {"TemporaryClientMode": 99, "TemporaryClientModeExpiresAt": time.Now().Add(time.Hour).Unix()},
		"no expiry":          {"TemporaryClientMode": int(types.Monitor)},
		"expiry is a string": {"TemporaryClientMode": int(types.Monitor), "TemporaryClientModeExpiresAt": "tomorrow"},
	} {
		t.Run(name, func(t *testing.T) {
			transport := freshServer(t)
			setGlobalClientMode(t, types.Lockdown)
			attrs["PK"] = "Machine#" + machineA
			attrs["SK"] = "Config"
			attrs["DataType"] = "MachineConfiguration"
			putItem(t, attrs)
			res, err := newHost(t, machineA, transport).Sync(t)
			if err != nil {
				t.Fatalf("sync failed: %v", err)
			}
			if got := sentMode(t, res.Preflight); got != "LOCKDOWN" {
				t.Errorf("%s, want the global LOCKDOWN", got)
			}
		})
	}
}

// sentMode is the client mode a preflight response told Santa to use.
func sentMode(t *testing.T, preflight *dynamicpb.Message) string {
	t.Helper()
	canonical := santaproto.Canonical(t, preflight)
	for _, mode := range []string{"MONITOR", "LOCKDOWN", "STANDALONE"} {
		if strings.Contains(canonical, `"client_mode":"`+mode+`"`) {
			return mode
		}
	}
	return "none in " + canonical
}
