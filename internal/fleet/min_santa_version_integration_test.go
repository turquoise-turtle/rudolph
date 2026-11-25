//go:build integration

package fleet_test

// Editable minimum Santa version (owner decision, 2026-11-25). Every run takes the minimum from the fleet health
// setting it already reads with one GetItem, the store table's FleetHealth / Config item, through
// fleethealth.FromItem(item).MinSantaVersionInForce(): the stored MinSantaVersion when there is a usable one, otherwise
// fleethealth.DefaultMinSantaVersion ("2026.9"). FLEET_MIN_SANTA_VERSION is no longer read (cmd/fleet logs that it is
// ignored), and fleet.Options has no MinSantaVersion (min_santa_version_test.go). A stored value therefore takes effect
// at the next run, with no redeploy.
//
//   - A release: a Mac whose release is below it is listed under Version, compared numerically as before.
//   - "off": no Mac is listed under Version (no rows, Health.Version 0, still present in Health), and none counts
//     towards Attention because of its version. Every other category is unchanged. VersionUnknown is still counted:
//     an empty or unparseable version is a data-quality signal (Santa not reporting, or a sensor row written by
//     something else), not a judgement against the minimum, so it stays visible when the check is off.
//   - Unusable (not a string, not a release, not "off"): ignored, and the default applies; the run does not fail.
//
// The pointer's MinSantaVersion records the minimum the run used: the release, or "off".

import (
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/airbnb/rudolph/pkg/model/fleethealth"
)

// fleetHealth stores the FleetHealth / Config item with attrs, replacing any earlier one.
func fleetHealth(t *testing.T, attrs map[string]any) {
	t.Helper()
	item := map[string]any{"PK": "FleetHealth", "SK": "Config"}
	for k, v := range attrs {
		item[k] = v
	}
	putStore(t, item)
}

func versionListed(t *testing.T, p pointer) map[string]bool {
	t.Helper()
	listed := map[string]bool{}
	for id := range byMachine(rows(t, p, "Health#Version")) {
		listed[id] = true
	}
	return listed
}

func TestFleet_MinSantaVersionDefaultsToFleethealths(t *testing.T) {
	reset(t)
	old := mac{id: machineID(1), hostname: "old", santaVersion: "2026.8.1"}
	current := mac{id: machineID(2), hostname: "current", santaVersion: "2026.9.0"}
	seed(t, old, current)
	run(t)
	p := mustPointer(t)
	if p.MinSantaVersion != "2026.9" {
		t.Errorf("pointer MinSantaVersion %q, want the default 2026.9", p.MinSantaVersion)
	}
	if p.MinSantaVersion != fleethealth.DefaultMinSantaVersion {
		t.Errorf("pointer MinSantaVersion %q is not fleethealth.DefaultMinSantaVersion (%q)", p.MinSantaVersion, fleethealth.DefaultMinSantaVersion)
	}
	if listed := versionListed(t, p); !listed[old.id] || listed[current.id] {
		t.Errorf("Version lists %v, want only the 2026.8.1 Mac", sortedKeys(listed))
	}
}

func TestFleet_MinSantaVersionFromTheStore(t *testing.T) {
	macs := []mac{
		{id: machineID(1), hostname: "v2026-8-1", santaVersion: "2026.8.1"},
		{id: machineID(2), hostname: "v2026-9-0", santaVersion: "2026.9.0"},
		{id: machineID(3), hostname: "v2026-10-0", santaVersion: "2026.10.0"},
		{id: machineID(4), hostname: "v2026-10-2", santaVersion: "2026.10.2"},
		{id: machineID(5), hostname: "v2026-11", santaVersion: "2026.11"},
	}
	for _, c := range []struct {
		stored string
		want   string // the pointer's MinSantaVersion
		listed []int  // fixtures listed under Version
	}{
		{"2026.10", "2026.10", []int{1, 2}},
		// A patch release compares as its year.month, as a Mac's own version does.
		{"2026.10.1", "2026.10.1", []int{1, 2}},
		// Below the default: lowering the minimum is allowed and takes effect.
		{"2026.8", "2026.8", nil},
		{"2026.11", "2026.11", []int{1, 2, 3, 4}},
		// Normalised as SetMinSantaVersion would store it, should someone edit the item by hand.
		{" 2026.10 ", "2026.10", []int{1, 2}},
	} {
		t.Run(c.stored, func(t *testing.T) {
			reset(t)
			fleetHealth(t, map[string]any{"MinSantaVersion": c.stored})
			seed(t, macs...)
			run(t)
			p := mustPointer(t)
			if p.MinSantaVersion != c.want {
				t.Errorf("pointer MinSantaVersion %q, want %q", p.MinSantaVersion, c.want)
			}
			want := map[string]bool{}
			for _, n := range c.listed {
				want[machineID(n)] = true
			}
			if got := versionListed(t, p); strings.Join(sortedKeys(got), " ") != strings.Join(sortedKeys(want), " ") {
				t.Errorf("Version lists %v, want %v", sortedKeys(got), sortedKeys(want))
			}
			if p.Health["Version"] != len(c.listed) || p.Attention != len(c.listed) {
				t.Errorf("Health.Version %d, Attention %d; want both %d", p.Health["Version"], p.Attention, len(c.listed))
			}
		})
	}
}

// The minimum, the normal threshold and a holiday share the item; each applies on its own.
func TestFleet_MinSantaVersionBesideAHoliday(t *testing.T) {
	reset(t)
	fleetHealth(t, map[string]any{"StaleDays": 14, "HolidayStaleDays": 60, "HolidayUntil": "2026-12-20", "MinSantaVersion": "2026.10"})
	away := mac{id: machineID(1), hostname: "away", santaVersion: "2026.10.0", preflightAt: now.AddDate(0, 0, -30)}
	old := mac{id: machineID(2), hostname: "old", santaVersion: "2026.9.0"}
	seed(t, away, old)
	run(t)
	p := mustPointer(t)
	if p.StaleDays != 60 || !p.Holiday || p.HolidayUntil != "2026-12-20" {
		t.Errorf("pointer StaleDays %d, Holiday %v, HolidayUntil %q; want 60, true, 2026-12-20", p.StaleDays, p.Holiday, p.HolidayUntil)
	}
	if p.MinSantaVersion != "2026.10" {
		t.Errorf("pointer MinSantaVersion %q, want 2026.10", p.MinSantaVersion)
	}
	if p.Health["Stale"] != 0 || p.Health["Version"] != 1 || !versionListed(t, p)[old.id] {
		t.Errorf("Stale %d, Version %d (lists %v); want 0 and 1, the 2026.9.0 Mac", p.Health["Stale"], p.Health["Version"], sortedKeys(versionListed(t, p)))
	}
}

func TestFleet_MinSantaVersionOff(t *testing.T) {
	for _, stored := range []string{"off", "OFF", " Off "} {
		t.Run(stored, func(t *testing.T) {
			reset(t)
			fleetHealth(t, map[string]any{"MinSantaVersion": stored})
			oldOnly := mac{id: machineID(1), hostname: "old-only", santaVersion: "2025.12"}
			oldAndStale := mac{id: machineID(2), hostname: "old-and-stale", santaVersion: "2026.8.1", preflightAt: now.AddDate(0, 0, -30)}
			sipOff := mac{id: machineID(3), hostname: "sip-off", sip: sipFlags(0x7f)}
			unknown := mac{id: machineID(4), hostname: "unknown-version", santaVersion: "garbage"}
			empty := mac{id: machineID(5), hostname: "empty-version", santaVersion: "-"}
			seed(t, oldOnly, oldAndStale, sipOff, unknown, empty)
			run(t)
			p := mustPointer(t)
			if p.MinSantaVersion != "off" {
				t.Errorf("pointer MinSantaVersion %q, want off", p.MinSantaVersion)
			}
			if r := rows(t, p, "Health#Version"); len(r) != 0 {
				t.Errorf("Version lists %v with the check off, want none", sks(r))
			}
			if got, ok := p.Health["Version"]; !ok || got != 0 {
				t.Errorf("Health.Version %d (present %v), want 0 and present: the six categories are always counted", got, ok)
			}
			// Other categories are unchanged.
			if p.Health["Stale"] != 1 || !byMachineHas(rows(t, p, "Health#Stale"), oldAndStale.id) {
				t.Errorf("Health.Stale %d, want 1 (the Mac 30 days away)", p.Health["Stale"])
			}
			if p.Health["SIP"] != 1 || !byMachineHas(rows(t, p, "Health#SIP"), sipOff.id) {
				t.Errorf("Health.SIP %d, want 1", p.Health["SIP"])
			}
			// Attention: the stale Mac and the SIP Mac. The Mac whose only finding is its version is not.
			if p.Attention != 2 {
				t.Errorf("Attention %d, want 2: a Mac whose only finding is an old version does not need attention with the check off", p.Attention)
			}
			// Still reported with the check off: it is about data quality, not the minimum.
			if p.VersionUnknown != 2 {
				t.Errorf("VersionUnknown %d, want 2 (garbage and empty) with the check off", p.VersionUnknown)
			}
			if p.Macs != 5 || p.Readiness["All"].Macs != 5 {
				t.Errorf("Macs %d, readiness All %d; want 5 and 5", p.Macs, p.Readiness["All"].Macs)
			}
		})
	}
}

func byMachineHas(items []map[string]types.AttributeValue, id string) bool {
	_, ok := byMachine(items)[id]
	return ok
}

// A hand-edited value the job cannot use falls back to the default; the run must not fail, or the lists would go
// stale over a typo.
func TestFleet_UnusableStoredMinSantaVersionFallsBackToTheDefault(t *testing.T) {
	for name, value := range map[string]any{
		"not a release": "garbage",
		"a year alone":  "2026",
		"empty":         "",
		"too long":      "2026.10." + strings.Repeat("0", 24) + "1",
		"a number":      2026.10,
		"a BOOL":        false,
	} {
		t.Run(name, func(t *testing.T) {
			reset(t)
			fleetHealth(t, map[string]any{"MinSantaVersion": value})
			old := mac{id: machineID(1), hostname: "old", santaVersion: "2026.8.1"}
			current := mac{id: machineID(2), hostname: "current", santaVersion: "2026.9.0"}
			seed(t, old, current)
			r := newRecorder(t)
			if err := fleetRun(options(r, now)); err != nil {
				t.Fatalf("an unusable stored minimum failed the run: %v", err)
			}
			p := mustPointer(t)
			if p.MinSantaVersion != "2026.9" {
				t.Errorf("pointer MinSantaVersion %q, want the default 2026.9", p.MinSantaVersion)
			}
			if listed := versionListed(t, p); !listed[old.id] || listed[current.id] {
				t.Errorf("Version lists %v, want only the 2026.8.1 Mac", sortedKeys(listed))
			}
		})
	}
}

// Changed in the store between two runs, the minimum applies from the next run: no option, environment variable or
// redeploy is involved.
func TestFleet_MinSantaVersionTakesEffectAtTheNextRun(t *testing.T) {
	reset(t)
	v9 := mac{id: machineID(1), hostname: "v2026-9", santaVersion: "2026.9.0"}
	v8 := mac{id: machineID(2), hostname: "v2026-8", santaVersion: "2026.8.1"}
	seed(t, v9, v8)

	steps := []struct {
		attrs   map[string]any // nil: no item
		pointer string
		listed  []string
	}{
		{nil, "2026.9", []string{v8.id}},
		{map[string]any{"MinSantaVersion": "2026.10"}, "2026.10", []string{v8.id, v9.id}},
		{map[string]any{"MinSantaVersion": "off"}, "off", nil},
		{map[string]any{"MinSantaVersion": "2026.8"}, "2026.8", nil},
		// Cleared (rudolph fleet min-santa-version --default): back to the default.
		{map[string]any{"StaleDays": 10}, "2026.9", []string{v8.id}},
	}
	for i, step := range steps {
		if step.attrs != nil {
			fleetHealth(t, step.attrs)
		}
		runAt(t, now.Add(time.Duration(i)*time.Hour))
		p := mustPointer(t)
		if p.MinSantaVersion != step.pointer {
			t.Errorf("run %d: pointer MinSantaVersion %q, want %q", i+1, p.MinSantaVersion, step.pointer)
		}
		got := versionListed(t, p)
		want := map[string]bool{}
		for _, id := range step.listed {
			want[id] = true
		}
		if strings.Join(sortedKeys(got), " ") != strings.Join(sortedKeys(want), " ") {
			t.Errorf("run %d (pointer %q): Version lists %v, want %v", i+1, step.pointer, sortedKeys(got), sortedKeys(want))
		}
	}
}
