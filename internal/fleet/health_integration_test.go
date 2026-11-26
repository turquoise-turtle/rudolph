//go:build integration

package fleet_test

// Fleet health (Fix 2 of the fleet health design, owner approved 2026-11-22). Each run walks every Mac once and writes
// an immutable snapshot to the events table: list rows per health category, then the pointer `FleetPointer` /
// `Current` with every count. A Mac is in a category when:
//
//   - Stale: its last completed sync (PostflightAt if set, else the newest CompletedSyncs[].PreflightAt) is more than
//     the stale threshold old, or it has none (Never). The threshold is StaleDays of `FleetHealth` / `Config` (10 days
//     when absent), or HolidayStaleDays while holiday mode lasts (holiday_integration_test.go). Exactly the threshold
//     is not stale; a second more is.
//   - Incomplete: PostflightAt empty, PreflightAt at least 30 minutes old and newer than the stale threshold.
//   - Mode: the reported ClientMode differs from the one Rudolph intends now (temporary while it lasts, own, strictest
//     of the Mac's groups, global, default MONITOR, as preflight resolves it), and the Mac is not Stale.
//   - Rules: RulesProcessed < RulesReceived on a sync with a postflight (Reason NotApplied), or its RulesHashAfterSync
//     differs from the majority of Macs with the same MachineRulesTracked digest (Reason Outlier).
//   - SIP: SIPStatus present and not 0; Flags is the value in hex ("0x7f").
//   - Version: the Santa release is below the minimum Santa version (FleetHealth / Config's MinSantaVersion, else
//     fleethealth.DefaultMinSantaVersion "2026.9"; compared numerically; none when it is "off", see
//     min_santa_version_integration_test.go); an empty or unparseable version counts in VersionUnknown and is not
//     listed.
//
// Health row: PK `FleetSnapshot#<snapshot>#Health#<category>`, SK `<order>#<machine ID>` where the order is the last
// completed sync as 20060102T150405Z (zeros when none) for Stale, the PreflightAt for Incomplete, the release as
// "2026.08" for Version (oldest first), and the SK is the machine ID alone for Mode, Rules and SIP. Every row has
// MachineID, Hostname and SerialNum (left out when the Mac reports none), LastSeen (sensor data Time), LastCompleted
// (left out when none), ExpiresAfter (now + 3 days) and the category's detail: Stale Never; Incomplete
// SyncsWithoutPostflight and Never; Mode Reported, Intended (MONITOR, LOCKDOWN, STANDALONE) and Source (temporary,
// own, group:<name>, global, default); Rules Reason, RulesReceived, RulesProcessed, Hash, ExpectedHash; SIP Flags;
// Version SantaVersion.
//
// Pointer: Snapshot, GeneratedAt (RFC 3339), Macs, Attention (Macs in at least one category), Health (map of the six
// categories to counts), VersionUnknown, MinSantaVersion (the minimum the run used, or "off"), StaleDays, Holiday,
// Readiness, Capped. It never expires.

import (
	"strings"
	"testing"
	"time"
)

func TestFleet_CountsAndLists(t *testing.T) {
	reset(t)
	healthy := mac{id: machineID(1), hostname: "healthy-mac", serial: "C02HEALTHY"}
	stale := mac{id: machineID(2), hostname: "stale-mac", serial: "C02STALE", preflightAt: now.AddDate(0, 0, -11), postflightAt: now.AddDate(0, 0, -11).Add(time.Minute)}
	never := mac{id: machineID(3), hostname: "never-mac", preflightAt: now.AddDate(0, 0, -12), noPostflight: true}
	incomplete := mac{id: machineID(4), hostname: "incomplete-mac", preflightAt: now.Add(-2 * time.Hour), noPostflight: true,
		completed: []time.Time{now.AddDate(0, 0, -2), now.AddDate(0, 0, -1)}, syncsWithoutPostflight: 3}
	mode := mac{id: machineID(5), hostname: "mode-mac", reportedMode: monitor, ownMode: lockdown}
	rules := mac{id: machineID(6), hostname: "rules-mac", rulesReceived: 10, rulesProcessed: 7, rulesHash: "abc123"}
	sip := mac{id: machineID(7), hostname: "sip-mac", sip: sipFlags(127)}
	old := mac{id: machineID(8), hostname: "old-mac", santaVersion: "2026.8.1"}
	unknown := mac{id: machineID(9), hostname: "unknown-version-mac", santaVersion: "garbage"}
	seed(t, healthy, stale, never, incomplete, mode, rules, sip, old, unknown)

	run(t)
	p := mustPointer(t)

	if p.Macs != 9 {
		t.Errorf("Macs %d, want 9", p.Macs)
	}
	for category, want := range map[string]int{"Stale": 2, "Incomplete": 1, "Mode": 1, "Rules": 1, "SIP": 1, "Version": 1} {
		if got, ok := p.Health[category]; !ok || got != want {
			t.Errorf("Health.%s %d (present %v), want %d", category, got, ok, want)
		}
	}
	if p.Attention != 7 {
		t.Errorf("Attention %d, want 7 (every Mac but the healthy one and the one with an unknown version)", p.Attention)
	}
	if p.VersionUnknown != 1 {
		t.Errorf("VersionUnknown %d, want 1", p.VersionUnknown)
	}
	if p.MinSantaVersion != "2026.9" {
		t.Errorf("MinSantaVersion %q, want the default 2026.9", p.MinSantaVersion)
	}
	if gen, err := time.Parse(time.RFC3339, p.GeneratedAt); err != nil || !gen.Equal(now) {
		t.Errorf("GeneratedAt %q, want %s", p.GeneratedAt, rfc(now))
	}
	if _, ok := p.raw["ExpiresAfter"]; ok {
		t.Error("the pointer expires; it is replaced by each run and must never expire")
	}

	// Stale: oldest first, a Mac with no completed sync before any other.
	staleRows := rows(t, p, "Health#Stale")
	if got, want := strings.Join(sks(staleRows), " "), "00000000T000000Z#"+never.id+" "+now.AddDate(0, 0, -11).Add(time.Minute).Format("20060102T150405Z")+"#"+stale.id; got != want {
		t.Errorf("Stale sort keys %q, want %q", got, want)
	}
	byID := byMachine(staleRows)
	if v, ok := b(byID[never.id], "Never"); !ok || !v {
		t.Errorf("the never-completed Mac's Never = %v (present %v), want true", v, ok)
	}
	if v, _ := b(byID[stale.id], "Never"); v {
		t.Error("the stale Mac with a completed sync has Never true")
	}
	if _, ok := byID[never.id]["LastCompleted"]; ok {
		t.Error("LastCompleted is set for a Mac that never completed a sync")
	}
	st := byID[stale.id]
	for attr, want := range map[string]string{"MachineID": stale.id, "Hostname": "stale-mac", "SerialNum": "C02STALE",
		"LastSeen": rfc(stale.preflightAt), "LastCompleted": rfc(stale.postflightAt)} {
		if got := s(st, attr); got != want {
			t.Errorf("Stale row %s %q, want %q", attr, got, want)
		}
	}
	if _, ok := byID[never.id]["SerialNum"]; ok {
		t.Error("SerialNum stored for a Mac that reports none")
	}

	inc := rows(t, p, "Health#Incomplete")
	if got, want := strings.Join(sks(inc), " "), incomplete.preflightAt.Format("20060102T150405Z")+"#"+incomplete.id; got != want {
		t.Errorf("Incomplete sort keys %q, want %q (the never-completed Mac's preflight is older than the threshold, so it is Stale only)", got, want)
	}
	if len(inc) == 1 {
		if v, _ := n(inc[0], "SyncsWithoutPostflight"); v != 3 {
			t.Errorf("Incomplete SyncsWithoutPostflight %d, want 3", v)
		}
		if v, ok := b(inc[0], "Never"); !ok || v {
			t.Errorf("Incomplete Never = %v (present %v), want false: the Mac completed a sync yesterday", v, ok)
		}
		if got := s(inc[0], "LastCompleted"); got != rfc(now.AddDate(0, 0, -1)) {
			t.Errorf("Incomplete LastCompleted %q, want the newest CompletedSyncs PreflightAt %s", got, rfc(now.AddDate(0, 0, -1)))
		}
	}

	modeRows := rows(t, p, "Health#Mode")
	if got := strings.Join(sks(modeRows), " "); got != mode.id {
		t.Errorf("Mode sort keys %q, want %q", got, mode.id)
	}
	if len(modeRows) == 1 {
		for attr, want := range map[string]string{"Reported": "MONITOR", "Intended": "LOCKDOWN", "Source": "own"} {
			if got := s(modeRows[0], attr); got != want {
				t.Errorf("Mode %s %q, want %q", attr, got, want)
			}
		}
	}

	ruleRows := rows(t, p, "Health#Rules")
	if got := strings.Join(sks(ruleRows), " "); got != rules.id {
		t.Errorf("Rules sort keys %q, want %q", got, rules.id)
	}
	if len(ruleRows) == 1 {
		if got := s(ruleRows[0], "Reason"); got != "NotApplied" {
			t.Errorf("Rules Reason %q, want NotApplied", got)
		}
		if got := s(ruleRows[0], "Hash"); got != "abc123" {
			t.Errorf("Rules Hash %q, want abc123", got)
		}
		received, _ := n(ruleRows[0], "RulesReceived")
		processed, _ := n(ruleRows[0], "RulesProcessed")
		if received != 10 || processed != 7 {
			t.Errorf("Rules received %d, processed %d; want 10 and 7", received, processed)
		}
	}

	sipRows := rows(t, p, "Health#SIP")
	if got := strings.Join(sks(sipRows), " "); got != sip.id {
		t.Errorf("SIP sort keys %q, want %q", got, sip.id)
	}
	if len(sipRows) == 1 && s(sipRows[0], "Flags") != "0x7f" {
		t.Errorf("SIP Flags %q, want 0x7f", s(sipRows[0], "Flags"))
	}

	versionRows := rows(t, p, "Health#Version")
	if got := strings.Join(sks(versionRows), " "); got != "2026.08#"+old.id {
		t.Errorf("Version sort keys %q, want %q", got, "2026.08#"+old.id)
	}
	if len(versionRows) == 1 && s(versionRows[0], "SantaVersion") != "2026.8.1" {
		t.Errorf("Version SantaVersion %q, want 2026.8.1", s(versionRows[0], "SantaVersion"))
	}

	// Every health row: base attributes and a 3-day expiry.
	for _, category := range []string{"Stale", "Incomplete", "Mode", "Rules", "SIP", "Version"} {
		for _, row := range rows(t, p, "Health#"+category) {
			if s(row, "MachineID") == "" || s(row, "Hostname") == "" || s(row, "LastSeen") == "" {
				t.Errorf("%s row %s lacks MachineID, Hostname or LastSeen", category, s(row, "SK"))
			}
			if exp, _ := n(row, "ExpiresAfter"); int64(exp) != now.Add(72*time.Hour).Unix() {
				t.Errorf("%s row ExpiresAfter %d, want now + 3 days (%d)", category, exp, now.Add(72*time.Hour).Unix())
			}
		}
	}
}

func TestFleet_HealthyMacIsInNoCategory(t *testing.T) {
	reset(t)
	seed(t, mac{id: machineID(1), hostname: "fine", sip: sipFlags(0)})
	run(t)
	p := mustPointer(t)
	if p.Attention != 0 {
		t.Errorf("Attention %d, want 0", p.Attention)
	}
	for _, category := range []string{"Stale", "Incomplete", "Mode", "Rules", "SIP", "Version"} {
		if got := p.Health[category]; got != 0 {
			t.Errorf("Health.%s %d, want 0", category, got)
		}
		if r := rows(t, p, "Health#"+category); len(r) != 0 {
			t.Errorf("%s lists %d rows for a healthy Mac (SIPStatus 0 is SIP fully on)", category, len(r))
		}
	}
}

func TestFleet_StaleAndIncompleteBoundaries(t *testing.T) {
	reset(t)
	threshold := 10 * 24 * time.Hour
	atThreshold := mac{id: machineID(1), hostname: "at", preflightAt: now.Add(-threshold).Add(-time.Minute), postflightAt: now.Add(-threshold)}
	past := mac{id: machineID(2), hostname: "past", preflightAt: now.Add(-threshold).Add(-time.Minute), postflightAt: now.Add(-threshold).Add(-time.Second)}
	// Completed earlier syncs only: the newest completed sync's preflight is the last completed sync.
	fromCompleted := mac{id: machineID(3), hostname: "from-completed", preflightAt: now.Add(-time.Hour), noPostflight: true,
		completed: []time.Time{now.AddDate(0, 0, -20), now.AddDate(0, 0, -3)}}
	grace29 := mac{id: machineID(4), hostname: "grace-29", preflightAt: now.Add(-29 * time.Minute), noPostflight: true, completed: []time.Time{now.AddDate(0, 0, -1)}}
	grace30 := mac{id: machineID(5), hostname: "grace-30", preflightAt: now.Add(-30 * time.Minute), noPostflight: true, completed: []time.Time{now.AddDate(0, 0, -1)}}
	// Talking to Rudolph 11 days ago without finishing, nothing since: Stale, not Incomplete.
	oldPreflight := mac{id: machineID(6), hostname: "old-preflight", preflightAt: now.AddDate(0, 0, -11), noPostflight: true, completed: []time.Time{now.AddDate(0, 0, -12)}}
	seed(t, atThreshold, past, fromCompleted, grace29, grace30, oldPreflight)
	run(t)
	p := mustPointer(t)

	stale := byMachine(rows(t, p, "Health#Stale"))
	incomplete := byMachine(rows(t, p, "Health#Incomplete"))
	for _, c := range []struct {
		m                 mac
		stale, incomplete bool
	}{
		{atThreshold, false, false},
		{past, true, false},
		{fromCompleted, false, true},
		{grace29, false, false},
		{grace30, false, true},
		{oldPreflight, true, false},
	} {
		if _, got := stale[c.m.id]; got != c.stale {
			t.Errorf("%s: Stale %v, want %v", c.m.hostname, got, c.stale)
		}
		if _, got := incomplete[c.m.id]; got != c.incomplete {
			t.Errorf("%s: Incomplete %v, want %v", c.m.hostname, got, c.incomplete)
		}
	}
	if row, ok := incomplete[fromCompleted.id]; ok && s(row, "LastCompleted") != rfc(now.AddDate(0, 0, -3)) {
		t.Errorf("LastCompleted %q, want the newest completed sync %s", s(row, "LastCompleted"), rfc(now.AddDate(0, 0, -3)))
	}
}

// The intended mode is resolved as preflight resolves it, and Source says where it came from.
func TestFleet_IntendedModeAndSource(t *testing.T) {
	reset(t)
	globalConfig(t, monitor, nil)
	registry(t,
		group{name: "plain"},
		group{name: "mon", mode: monitor},
		group{name: "lock", mode: lockdown},
		group{name: "rudolph:ungrouped", mode: standalone},
	)
	cases := []struct {
		m        mac
		intended string
		source   string // "" when the Mac is not listed
	}{
		{mac{id: machineID(1), hostname: "temporary-running", assigned: []string{"plain"}, ownMode: monitor, temporaryMode: lockdown, temporaryExpires: now.Add(time.Hour)}, "LOCKDOWN", "temporary"},
		{mac{id: machineID(2), hostname: "temporary-ended", assigned: []string{"plain"}, reportedMode: lockdown, temporaryMode: lockdown, temporaryExpires: now.Add(-time.Hour)}, "MONITOR", "global"},
		{mac{id: machineID(3), hostname: "temporary-too-far", assigned: []string{"plain"}, reportedMode: lockdown, temporaryMode: lockdown, temporaryExpires: now.AddDate(0, 0, 40)}, "MONITOR", "global"},
		{mac{id: machineID(4), hostname: "own", assigned: []string{"plain"}, ownMode: standalone}, "STANDALONE", "own"},
		{mac{id: machineID(5), hostname: "own-mistyped", assigned: []string{"plain"}, reportedMode: lockdown, ownMode: 9}, "MONITOR", "global"},
		{mac{id: machineID(6), hostname: "two-groups", assigned: []string{"lock", "mon"}}, "LOCKDOWN", "group:lock"},
		{mac{id: machineID(7), hostname: "ungrouped"}, "STANDALONE", "group:rudolph:ungrouped"},
		{mac{id: machineID(8), hostname: "agrees", assigned: []string{"mon"}}, "MONITOR", ""},
	}
	for _, c := range cases {
		c.m.seed(t)
	}
	run(t)
	listed := byMachine(rows(t, mustPointer(t), "Health#Mode"))
	for _, c := range cases {
		row, ok := listed[c.m.id]
		if c.source == "" {
			if ok {
				t.Errorf("%s: listed under Mode although it reports the intended mode", c.m.hostname)
			}
			continue
		}
		if !ok {
			t.Errorf("%s: not listed under Mode (intended %s from %s)", c.m.hostname, c.intended, c.source)
			continue
		}
		if s(row, "Intended") != c.intended || s(row, "Source") != c.source {
			t.Errorf("%s: Intended %q from %q, want %q from %q", c.m.hostname, s(row, "Intended"), s(row, "Source"), c.intended, c.source)
		}
	}

	// No global config row at all: MONITOR by default.
	reset(t)
	seed(t, mac{id: machineID(9), hostname: "default", reportedMode: lockdown})
	run(t)
	r := rows(t, mustPointer(t), "Health#Mode")
	if len(r) != 1 || s(r[0], "Intended") != "MONITOR" || s(r[0], "Source") != "default" {
		t.Errorf("without a global config: Mode rows %v, want one with Intended MONITOR and Source default", r)
	}
}

func TestFleet_ModeMismatchSkipsStaleMacs(t *testing.T) {
	reset(t)
	seed(t, mac{id: machineID(1), hostname: "stale-and-wrong", ownMode: lockdown, preflightAt: now.AddDate(0, 0, -20)})
	run(t)
	p := mustPointer(t)
	if len(rows(t, p, "Health#Mode")) != 0 || p.Health["Mode"] != 0 {
		t.Error("a Stale Mac is listed under Mode; its reported mode is not current")
	}
	if p.Health["Stale"] != 1 {
		t.Errorf("Health.Stale %d, want 1", p.Health["Stale"])
	}
}

func TestFleet_RulesNotAppliedNeedsAPostflight(t *testing.T) {
	reset(t)
	withPostflight := mac{id: machineID(1), hostname: "with", rulesReceived: 4, rulesProcessed: 3}
	without := mac{id: machineID(2), hostname: "without", rulesReceived: 4, rulesProcessed: 3, noPostflight: true, preflightAt: now.Add(-5 * time.Minute), completed: []time.Time{now.Add(-24 * time.Hour)}}
	all := mac{id: machineID(3), hostname: "all", rulesReceived: 4, rulesProcessed: 4}
	seed(t, withPostflight, without, all)
	run(t)
	got := byMachine(rows(t, mustPointer(t), "Health#Rules"))
	if _, ok := got[withPostflight.id]; !ok {
		t.Error("a Mac that applied 3 of 4 rules on a completed sync is not listed")
	}
	for _, m := range []mac{without, all} {
		if _, ok := got[m.id]; ok {
			t.Errorf("%s is listed under Rules", m.hostname)
		}
	}
}

// Macs with the same MachineRulesTracked digest are sent the same rules, so they should report the same hash after
// a completed sync. The majority is the hash more than half of at least three such Macs report (postflight in the
// last 24 hours); a Mac is an outlier when its hash differs. With a rules feed row, only Macs that completed a sync
// more than 10 minutes after it take part (outlier_after_rule_change_integration_test.go); this fleet has none.
func TestFleet_RulesHashOutliers(t *testing.T) {
	reset(t)
	base := now.Add(-2 * time.Hour)
	var macs []mac
	add := func(id int, digest, hash string, postflight time.Time) mac {
		m := mac{id: machineID(id), hostname: "m" + machineID(id)[:8], digest: digest, rulesHash: hash, rulesReceived: 3, rulesProcessed: 3,
			preflightAt: postflight.Add(-time.Minute), postflightAt: postflight}
		macs = append(macs, m)
		return m
	}
	// Digest d1: three agree on h1; the fourth reports h2: outlier.
	add(1, "d1", "h1", base)
	add(2, "d1", "h1", base.Add(time.Minute))
	add(3, "d1", "h1", base.Add(2*time.Minute))
	outlier := add(4, "d1", "h2", base.Add(11*time.Minute))
	// Digest d3: a 2-2 split has no majority.
	add(9, "d3", "k1", base)
	add(10, "d3", "k1", base)
	add(11, "d3", "k2", base.Add(time.Hour))
	add(12, "d3", "k2", base.Add(time.Hour))
	// Digest d4: two Macs only.
	add(13, "d4", "j1", base)
	add(14, "d4", "j2", base.Add(time.Hour))
	// dirty and empty digests are never judged.
	for i, digest := range []string{"dirty", "dirty", "dirty", "dirty", "", "", "", ""} {
		hash := "x1"
		if i%4 == 3 {
			hash = "x2"
		}
		add(20+i, digest, hash, base.Add(time.Duration(i)*time.Hour/4))
	}
	seed(t, macs...)
	run(t)
	listed := rows(t, mustPointer(t), "Health#Rules")
	if len(listed) != 1 || s(listed[0], "MachineID") != outlier.id {
		t.Fatalf("Rules lists %v, want only %s", sks(listed), outlier.id)
	}
	row := listed[0]
	if s(row, "Reason") != "Outlier" || s(row, "Hash") != "h2" || s(row, "ExpectedHash") != "h1" {
		t.Errorf("outlier row Reason %q Hash %q ExpectedHash %q, want Outlier, h2, h1", s(row, "Reason"), s(row, "Hash"), s(row, "ExpectedHash"))
	}
}

func TestFleet_SIP(t *testing.T) {
	reset(t)
	seed(t,
		mac{id: machineID(1), hostname: "absent"},
		mac{id: machineID(2), hostname: "zero", sip: sipFlags(0)},
		mac{id: machineID(3), hostname: "all-off", sip: sipFlags(127)},
		mac{id: machineID(4), hostname: "one-flag", sip: sipFlags(2)},
	)
	run(t)
	got := map[string]string{}
	for _, r := range rows(t, mustPointer(t), "Health#SIP") {
		got[s(r, "Hostname")] = s(r, "Flags")
	}
	if len(got) != 2 || got["all-off"] != "0x7f" || got["one-flag"] != "0x2" {
		t.Errorf("SIP rows %v, want all-off 0x7f and one-flag 0x2", got)
	}
}

// Versions compare numerically (2026.10 is newer than 2026.9), and the list is oldest first.
func TestFleet_Version(t *testing.T) {
	reset(t)
	seed(t,
		mac{id: machineID(1), hostname: "v2026-8-1", santaVersion: "2026.8.1"},
		mac{id: machineID(2), hostname: "v2026-9", santaVersion: "2026.9"},
		mac{id: machineID(3), hostname: "v2026-10-0", santaVersion: "2026.10.0"},
		mac{id: machineID(4), hostname: "v2025-12", santaVersion: "2025.12"},
		mac{id: machineID(5), hostname: "empty", santaVersion: "-"},
		mac{id: machineID(6), hostname: "garbage", santaVersion: "garbage"},
	)
	run(t)
	p := mustPointer(t)
	want := "2025.12#" + machineID(4) + " 2026.08#" + machineID(1)
	if got := strings.Join(sks(rows(t, p, "Health#Version")), " "); got != want {
		t.Errorf("Version sort keys %q, want %q", got, want)
	}
	if p.VersionUnknown != 2 {
		t.Errorf("VersionUnknown %d, want 2 (empty and garbage)", p.VersionUnknown)
	}
}

func TestFleet_IncludesMacsWithoutSerialOrHostname(t *testing.T) {
	reset(t)
	bare := mac{id: machineID(1), preflightAt: now.AddDate(0, 0, -30)}
	seed(t, bare)
	run(t)
	p := mustPointer(t)
	if p.Macs != 1 {
		t.Fatalf("Macs %d, want 1: a Mac without serial number or hostname is still a Mac", p.Macs)
	}
	r := rows(t, p, "Health#Stale")
	if len(r) != 1 || s(r[0], "MachineID") != bare.id {
		t.Fatalf("Stale rows %v, want the bare Mac", sks(r))
	}
	for _, attr := range []string{"Hostname", "SerialNum"} {
		if v, ok := r[0][attr]; ok {
			t.Errorf("%s stored as %v for a Mac that reports none; leave it out", attr, v)
		}
	}
}
