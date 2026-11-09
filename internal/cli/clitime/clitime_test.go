package clitime

// Times the CLI prints are shown in UTC, in the operator's local time and relative to now, so an operator outside UTC
// does not have to convert (owner feedback from the first production deployment: "Expires: 2026-11-09T11:21:42Z" alone
// was confusing). machine show already used this shape for times in the past.

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
		{now.Add(2 * time.Hour), "2026-11-09T11:21:42Z (2026-11-09 22:21 TST local, in 2h 0m)"},
		{now.Add(90 * time.Second), "2026-11-09T09:23:12Z (2026-11-09 20:23 TST local, in 1m)"},
		{now.Add(30 * time.Second), "2026-11-09T09:22:12Z (2026-11-09 20:22 TST local, in 30s)"},
		{now.Add(72 * time.Hour), "2026-11-12T09:21:42Z (2026-11-12 20:21 TST local, in 3 days)"},
		{now.Add(-5 * time.Minute), "2026-11-09T09:16:42Z (2026-11-09 20:16 TST local, expired 5m ago)"},
		{now, "2026-11-09T09:21:42Z (2026-11-09 20:21 TST local, expired 0s ago)"},
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
	if got, want := Since(at, now), "2026-11-09T06:02:56Z (2026-11-09 17:02 TST local, 26s ago)"; got != want {
		t.Errorf("Since = %q, want %q", got, want)
	}
}
