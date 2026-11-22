//go:build integration

package fleet_test

// Stale threshold and holiday mode (owner decision 2, 2026-11-22). The job reads the store table's `FleetHealth` /
// `Config` item with one GetItem on every run: StaleDays is the normal threshold (10 days when the item or the
// attribute is absent); while today (UTC) is on or before HolidayUntil (a date, inclusive), HolidayStaleDays is the
// threshold instead, and the day after it the normal one applies again by itself. The threshold in force decides
// Stale, Incomplete (a preflight older than it is Stale only) and the Unmeasured "stale" reason. The pointer records
// it: StaleDays (the threshold used), Holiday (BOOL) and, while holiday mode is on, HolidayUntil.

import (
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
)

func TestFleet_StaleThresholdDefaultsToTenDays(t *testing.T) {
	reset(t)
	nine := mac{id: machineID(1), hostname: "nine", preflightAt: now.AddDate(0, 0, -9)}
	eleven := mac{id: machineID(2), hostname: "eleven", preflightAt: now.AddDate(0, 0, -11)}
	seed(t, nine, eleven)
	rec := run(t)
	p := mustPointer(t)
	if p.StaleDays != 10 {
		t.Errorf("pointer StaleDays %d, want the default 10", p.StaleDays)
	}
	if v, ok := b(p.raw, "Holiday"); !ok || v {
		t.Errorf("pointer Holiday %v (present %v), want false", v, ok)
	}
	if _, ok := p.raw["HolidayUntil"]; ok {
		t.Error("pointer has HolidayUntil without holiday mode")
	}
	stale := byMachine(rows(t, p, "Health#Stale"))
	if _, ok := stale[nine.id]; ok {
		t.Error("a Mac last synced 9 days ago is Stale with the default 10-day threshold")
	}
	if _, ok := stale[eleven.id]; !ok {
		t.Error("a Mac last synced 11 days ago is not Stale with the default 10-day threshold")
	}
	read := false
	for _, c := range rec.calls {
		if c.op == "GetItem" && c.table == ddblocal.TableName && len(c.keys) == 1 && c.keys[0] == "FleetHealth" {
			read = true
		}
	}
	if !read {
		t.Error("the job did not GetItem FleetHealth / Config from the store table")
	}
}

func TestFleet_NormalStaleDaysFromTheConfig(t *testing.T) {
	reset(t)
	holiday(t, 20, 0, "")
	eleven := mac{id: machineID(1), hostname: "eleven", preflightAt: now.AddDate(0, 0, -11)}
	seed(t, eleven)
	run(t)
	p := mustPointer(t)
	if p.StaleDays != 20 {
		t.Errorf("pointer StaleDays %d, want 20", p.StaleDays)
	}
	if p.Health["Stale"] != 0 {
		t.Errorf("Health.Stale %d with a 20-day threshold, want 0", p.Health["Stale"])
	}
}

func TestFleet_HolidayThresholdUntilItsDate(t *testing.T) {
	away := mac{id: machineID(1), hostname: "away", preflightAt: now.AddDate(0, 0, -30)}
	// Reached Rudolph 15 days ago without finishing, completed 20 days ago: under a 60-day threshold it is talking to
	// Rudolph but not finishing (Incomplete); under 10 days it is Stale.
	stuck := mac{id: machineID(2), hostname: "stuck", preflightAt: now.AddDate(0, 0, -15), noPostflight: true, completed: []time.Time{now.AddDate(0, 0, -20)}}

	for _, c := range []struct {
		name                       string
		until                      string
		holiday                    bool
		staleDays                  int
		awayStale, stuckIncomplete bool
	}{
		{"until a later day", "2026-12-20", true, 60, false, true},
		{"until today (inclusive)", today, true, 60, false, true},
		{"ended yesterday", daysAgo(1), false, 10, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			reset(t)
			holiday(t, 0, 60, c.until)
			seed(t, away, stuck)
			run(t)
			p := mustPointer(t)
			if p.StaleDays != c.staleDays {
				t.Errorf("pointer StaleDays %d, want %d", p.StaleDays, c.staleDays)
			}
			if v, _ := b(p.raw, "Holiday"); v != c.holiday {
				t.Errorf("pointer Holiday %v, want %v", v, c.holiday)
			}
			if c.holiday && p.HolidayUntil != c.until {
				t.Errorf("pointer HolidayUntil %q, want %q", p.HolidayUntil, c.until)
			}
			if !c.holiday {
				if _, ok := p.raw["HolidayUntil"]; ok {
					t.Error("pointer has HolidayUntil after the holiday ended")
				}
			}
			stale := byMachine(rows(t, p, "Health#Stale"))
			if _, got := stale[away.id]; got != c.awayStale {
				t.Errorf("a Mac away for 30 days: Stale %v, want %v", got, c.awayStale)
			}
			if _, got := byMachine(rows(t, p, "Health#Incomplete"))[stuck.id]; got != c.stuckIncomplete {
				t.Errorf("a Mac stuck 15 days ago: Incomplete %v, want %v", got, c.stuckIncomplete)
			}
			if why := s(byMachine(rows(t, p, "Ready#All"))[away.id], "Why"); (why == "stale") != c.awayStale {
				t.Errorf("readiness Why %q for the away Mac, want stale exactly when it is Stale (%v)", why, c.awayStale)
			}
		})
	}
}

// A holiday item and a normal StaleDays together: after the holiday the stored normal threshold applies, not 10.
func TestFleet_HolidayEndsIntoTheStoredNormalThreshold(t *testing.T) {
	reset(t)
	holiday(t, 14, 60, daysAgo(1))
	seed(t, mac{id: machineID(1), hostname: "twelve", preflightAt: now.AddDate(0, 0, -12)})
	run(t)
	p := mustPointer(t)
	if p.StaleDays != 14 || p.Health["Stale"] != 0 {
		t.Errorf("after the holiday: StaleDays %d, Stale %d; want 14 and 0", p.StaleDays, p.Health["Stale"])
	}
}
