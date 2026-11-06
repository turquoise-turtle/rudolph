//go:build integration

package handlers

// Client versions (plan section E, owner decisions 2026-11-06). Santa 2026.9 is current; 2026.8 is what the fleet runs
// now; 2025.12 is the last release for macOS 13 and the only older client supported, for basic sync. The preflight
// request carries santa_version (e.g. "2025.12.0"); an empty or unparseable version counts as current.
//
//   - Path regexes: Santa below 2026.8 compiles the regex as received, so an empty string matches every path. Such a
//     client gets the never-matching `(?!x)x` wherever the configured regex is empty; a configured regex is sent
//     unchanged. 2026.8 and newer get "" as before (they clear the regex).
//   - SILENT_GUI_BLOCKLIST and SILENT_TTY_BLOCKLIST exist from Santa 2026.6; an older client skips them as unknown
//     (the binary would not be blocked at all), so it gets SILENT_BLOCKLIST instead, on clean and incremental syncs.
//   - A Mac upgraded across that boundary holds the downgraded rules: its next sync is a clean sync, so it gets the
//     real policies.
//   - Event field identity_unverified (2026.9) reaches the event sinks and the events table (IdentityUnverified).

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/fakeaws"
	"github.com/airbnb/rudolph/internal/testutil/fakesanta"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
	"github.com/airbnb/rudolph/pkg/types"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const neverMatches = "(?!x)x"

func setGlobalRegexes(t *testing.T, allowed, blocked string) {
	t.Helper()
	cfg := machineconfiguration.GetUniversalDefaultConfig()
	cfg.AllowedPathRegex = allowed
	cfg.BlockedPathRegex = blocked
	if err := configService(t).SetGlobalConfig(cfg); err != nil {
		t.Fatal(err)
	}
	handlers = newHandlers() // the global config is cached
}

func hostOn(t *testing.T, version, id string, transport fakesanta.Transport) *fakesanta.Host {
	t.Helper()
	h := newHost(t, id, transport)
	h.Version = version
	return h
}

func TestClientVersions_EmptyPathRegexesForOldClients(t *testing.T) {
	transport := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	setGlobalRegexes(t, "", "")
	for version, want := range map[string]string{"2025.12": neverMatches, "2026.8": "", "2026.9": ""} {
		h := hostOn(t, version, map[string]string{"2025.12": machineA, "2026.8": machineB, "2026.9": "CCCCCCCC-3333-4333-8333-CCCCCCCCCCCC"}[version], transport)
		h.MustSync(t)
		if h.AllowedPathRegex != want || h.BlockedPathRegex != want {
			t.Errorf("Santa %s: allowed %q, blocked %q; want %q for both", version, h.AllowedPathRegex, h.BlockedPathRegex, want)
		}
	}
}

func TestClientVersions_ConfiguredAndClearedRegexReachAnOldClient(t *testing.T) {
	transport := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	setGlobalRegexes(t, "^/opt/tools/", "^/tmp/")
	h := hostOn(t, "2025.12", machineA, transport)
	h.MustSync(t)
	if h.AllowedPathRegex != "^/opt/tools/" || h.BlockedPathRegex != "^/tmp/" {
		t.Fatalf("configured regexes: allowed %q, blocked %q", h.AllowedPathRegex, h.BlockedPathRegex)
	}
	setGlobalRegexes(t, "", "^/tmp/")
	h.MustSync(t)
	if h.AllowedPathRegex != neverMatches || h.BlockedPathRegex != "^/tmp/" {
		t.Errorf("after clearing the allowed regex: allowed %q, blocked %q; want %q and ^/tmp/", h.AllowedPathRegex, h.BlockedPathRegex, neverMatches)
	}
}

func TestClientVersions_SilentGUIAndTTYRulesForOldClients(t *testing.T) {
	transport := freshServer(t)
	addGlobalRule(t, strings.Repeat("e5", 32), types.RuleTypeBinary, types.RulePolicyBlocklist) // a rule every client knows
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicySilentGUIBlocklist)
	old := hostOn(t, "2025.12", machineA, transport)
	current := hostOn(t, "2026.8", machineB, transport)
	old.MustSync(t) // clean
	current.MustSync(t)
	addGlobalRule(t, sha2, types.RuleTypeBinary, types.RulePolicySilentTTYBlocklist)
	addMachineRule(t, machineA, sha3, types.RuleTypeBinary, types.RulePolicySilentGUIBlocklist, time.Now().Add(24*time.Hour))
	if res := old.MustSync(t); res.SyncType != "NORMAL" {
		t.Fatalf("precondition: an incremental sync, got %s", res.SyncType)
	}
	current.MustSync(t)

	for _, sha := range []string{sha1, sha2, sha3} {
		if got := old.Policy("BINARY", sha); got != "SILENT_BLOCKLIST" {
			t.Errorf("Santa 2025.12, %s…: %q, want SILENT_BLOCKLIST", sha[:4], got)
		}
	}
	if current.Policy("BINARY", sha1) != "SILENT_GUI_BLOCKLIST" || current.Policy("BINARY", sha2) != "SILENT_TTY_BLOCKLIST" {
		t.Errorf("Santa 2026.8: %q and %q, want the policies unchanged", current.Policy("BINARY", sha1), current.Policy("BINARY", sha2))
	}
}

func TestClientVersions_UpgradeFromAnOldClientGetsTheRealPolicies(t *testing.T) {
	transport := freshServer(t)
	addGlobalRule(t, strings.Repeat("e5", 32), types.RuleTypeBinary, types.RulePolicyBlocklist) // a rule every client knows
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicySilentGUIBlocklist)
	setGlobalRegexes(t, "", "")
	h := hostOn(t, "2025.12", machineA, transport)
	h.MustSync(t)
	h.MustSync(t)
	if h.Policy("BINARY", sha1) != "SILENT_BLOCKLIST" {
		t.Fatalf("precondition: %q", h.Policy("BINARY", sha1))
	}
	h.Version = "2026.9"
	res := h.MustSync(t)
	if res.SyncType == "NORMAL" {
		t.Errorf("the first sync after upgrading from 2025.12 was NORMAL, want a clean sync")
	}
	if got := h.Policy("BINARY", sha1); got != "SILENT_GUI_BLOCKLIST" {
		t.Errorf("after the upgrade: %q, want SILENT_GUI_BLOCKLIST", got)
	}
	if h.AllowedPathRegex != "" || h.BlockedPathRegex != "" {
		t.Errorf("after the upgrade the regexes are %q and %q, want cleared", h.AllowedPathRegex, h.BlockedPathRegex)
	}
}

func TestClientVersions_OldClientSyncsEndToEnd(t *testing.T) {
	transport := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	h := hostOn(t, "2025.12", machineA, transport)
	h.SantaExactEventUpload = true
	res := h.MustSync(t)
	if res.SyncType == "NORMAL" || h.Policy("BINARY", sha1) != "BLOCKLIST" {
		t.Fatalf("first sync: %s, rule %q", res.SyncType, h.Policy("BINARY", sha1))
	}
	addGlobalRule(t, sha2, types.RuleTypeBinary, types.RulePolicyBlocklist)
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	h.PendingEvents = []string{execEvent(sha3, "BLOCK_UNKNOWN", 7, at, "")}
	res = h.MustSync(t)
	if res.SyncType != "NORMAL" || h.Policy("BINARY", sha2) != "BLOCKLIST" {
		t.Errorf("incremental sync: %s, new rule %q", res.SyncType, h.Policy("BINARY", sha2))
	}
	if res.PostflightError != nil {
		t.Errorf("postflight: %v", res.PostflightError)
	}
	if len(h.PendingEvents) != 0 || len(eventsOf(t, machineA)) != 1 {
		t.Errorf("event upload: %d pending, %d stored; want 0 and 1", len(h.PendingEvents), len(eventsOf(t, machineA)))
	}
	sensor := getItem(t, "Machine#"+machineA, "Current")
	if v, _ := sensorString(t, sensor, "SantaVersion"); v != "2025.12.0" {
		t.Errorf("stored SantaVersion %q", v)
	}
}

func TestClientVersions_IdentityUnverifiedReachesSinksAndTheEventsTable(t *testing.T) {
	aws := fakeaws.Start(t)
	aws.Use(t)
	t.Setenv("HANDLER", "FIREHOSE")
	t.Setenv("FIREHOSE_NAME", "events")
	t.Setenv("LAMBDA_NAME", "")
	transport := freshServer(t)
	addGlobalRule(t, strings.Repeat("e5", 32), types.RuleTypeBinary, types.RulePolicyBlocklist)
	h := hostOn(t, "2026.9", machineA, transport)
	syncNormal(t, h)
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	h.PendingEvents = []string{
		execEvent(sha1, "BLOCK_BINARY_MISMATCH", 1, at, `"identity_unverified":true`),
		execEvent(sha2, "BLOCK_UNKNOWN", 2, at, ""),
	}
	h.MustSync(t)

	found := false
	for _, r := range aws.FirehoseRecords() {
		var e struct {
			FileSHA256         string `json:"file_sha256"`
			IdentityUnverified *bool  `json:"identity_unverified"`
		}
		if err := json.Unmarshal(r.Data, &e); err != nil {
			t.Fatal(err)
		}
		if e.FileSHA256 == sha1 {
			found = e.IdentityUnverified != nil && *e.IdentityUnverified
		}
	}
	if !found {
		t.Error("the Firehose record does not carry identity_unverified: true")
	}

	unverified := eventsItem(t, "Exec#"+machineA, tsOf(at)+"#"+sha1+"#1")
	if b, ok := unverified["IdentityUnverified"].(*awstypes.AttributeValueMemberBOOL); !ok || !b.Value {
		t.Errorf("events table IdentityUnverified %v, want true", unverified["IdentityUnverified"])
	}
	verified := eventsItem(t, "Exec#"+machineA, tsOf(at)+"#"+sha2+"#2")
	if _, ok := verified["IdentityUnverified"]; ok {
		t.Errorf("an ordinary event stores IdentityUnverified %v; leave it out", verified["IdentityUnverified"])
	}
}

// From the review: a Mac going back to an older Santa (or an old client's first sync after Rudolph starts adapting
// to it) holds rules that release does not apply, so its next sync is a clean sync too, with the adapted policies.
func TestClientVersions_DowngradeToAnOldClientCleanSyncs(t *testing.T) {
	transport := freshServer(t)
	addGlobalRule(t, strings.Repeat("e5", 32), types.RuleTypeBinary, types.RulePolicyBlocklist)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicySilentGUIBlocklist)
	h := hostOn(t, "2026.9", machineA, transport)
	h.MustSync(t)
	h.MustSync(t)
	h.Version = "2025.12"
	res := h.MustSync(t)
	if res.SyncType == "NORMAL" {
		t.Errorf("the first sync after going back to 2025.12 was NORMAL, want a clean sync")
	}
	if got := h.Policy("BINARY", sha1); got != "SILENT_BLOCKLIST" {
		t.Errorf("after the downgrade: %q, want SILENT_BLOCKLIST", got)
	}
}
