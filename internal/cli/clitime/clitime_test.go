package clitime

// Times the CLI prints are shown in the operator's local time, then UTC in brackets, then relative to now, so an operator
// outside UTC does not have to convert (owner feedback from the first production deployment: "Expires:
// 2026-11-09T11:21:42Z" alone was confusing, and local time should come first, as on rudolph-web). An operator whose
// zone is UTC sees UTC once.

import (
	"testing"
	"time"
)

func withLocal(t *testing.T, loc *time.Location) {
	t.Helper()
	old := time.Local
	time.Local = loc
	t.Cleanup(func() { time.Local = old })
}

func TestExpiry(t *testing.T) {
	withLocal(t, time.FixedZone("TST", 11*3600))
	now := time.Date(2026, 11, 9, 9, 21, 42, 0, time.UTC)
	for _, c := range []struct {
		at   time.Time
		want string
	}{
		{now.Add(2 * time.Hour), "2026-11-09 22:21 TST (2026-11-09 11:21:42 UTC), in 2h 0m"},
		{now.Add(90 * time.Second), "2026-11-09 20:23 TST (2026-11-09 09:23:12 UTC), in 1m"},
		{now.Add(30 * time.Second), "2026-11-09 20:22 TST (2026-11-09 09:22:12 UTC), in 30s"},
		{now.Add(72 * time.Hour), "2026-11-12 20:21 TST (2026-11-12 09:21:42 UTC), in 3 days"},
		{now.Add(-5 * time.Minute), "2026-11-09 20:16 TST (2026-11-09 09:16:42 UTC), expired 5m ago"},
		{now, "2026-11-09 20:21 TST (2026-11-09 09:21:42 UTC), expired 0s ago"},
	} {
		if got := Expiry(c.at, now); got != c.want {
			t.Errorf("Expiry(%s) = %q, want %q", c.at, got, c.want)
		}
	}
	if got := Expiry(time.Time{}, now); got != "never" {
		t.Errorf("Expiry(zero) = %q, want never", got)
	}
}

func TestSince(t *testing.T) {
	withLocal(t, time.FixedZone("TST", 11*3600))
	now := time.Date(2026, 11, 9, 6, 3, 22, 0, time.UTC)
	at := time.Date(2026, 11, 9, 6, 2, 56, 0, time.UTC)
	if got, want := Since(at, now), "2026-11-09 17:02 TST (2026-11-09 06:02:56 UTC), 26s ago"; got != want {
		t.Errorf("Since = %q, want %q", got, want)
	}
}

func TestOperatorInUTCSeesUTCOnce(t *testing.T) {
	for _, loc := range []*time.Location{time.UTC, time.FixedZone("UTC", 0)} {
		withLocal(t, loc)
		now := time.Date(2026, 11, 9, 9, 21, 42, 0, time.UTC)
		if got, want := Expiry(now.Add(2*time.Hour), now), "2026-11-09 11:21:42 UTC, in 2h 0m"; got != want {
			t.Errorf("Expiry in %s = %q, want %q", loc, got, want)
		}
		if got, want := Since(now.Add(-26*time.Second), now), "2026-11-09 09:21:16 UTC, 26s ago"; got != want {
			t.Errorf("Since in %s = %q, want %q", loc, got, want)
		}
	}
}

func TestLocalTimeComesFirstInEveryZone(t *testing.T) {
	// Daylight saving: New York is UTC-4 in September and UTC-5 in December.
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip(err)
	}
	withLocal(t, ny)
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if got, want := Expiry(time.Date(2026, 9, 17, 11, 55, 0, 0, time.UTC), now), "2026-09-17 07:55 EDT (2026-09-17 11:55:00 UTC), in 16 days"; got != want {
		t.Errorf("September: %q, want %q", got, want)
	}
	if got, want := Expiry(time.Date(2026, 12, 17, 11, 55, 0, 0, time.UTC), now), "2026-12-17 06:55 EST (2026-12-17 11:55:00 UTC), in 107 days"; got != want {
		t.Errorf("December: %q, want %q", got, want)
	}
}
