// Package clitime formats the times the CLI prints for people: in the operator's local time first, then UTC in
// brackets, then relative to now, so an operator outside UTC does not have to convert. An operator whose zone is UTC
// sees UTC once. Audit record summaries are stored and read from anywhere, so they keep plain RFC 3339 UTC instead.
package clitime

import (
	"fmt"
	"time"
)

const (
	localLayout = "2006-01-02 15:04 MST"
	utcLayout   = "2006-01-02 15:04:05 UTC"
)

// Expiry shows when something ends, e.g. "2026-11-09 20:21 JST (2026-11-09 11:21:42 UTC), in 2h 0m", or
// "2026-11-09 11:21:42 UTC, in 2h 0m" for an operator in UTC; the zero time is never. An end at or before now has
// expired.
func Expiry(t, now time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := t.Sub(now)
	relative := "in " + Duration(d)
	if d <= 0 {
		relative = "expired " + Duration(-d) + " ago"
	}
	return absolute(t) + ", " + relative
}

// Since shows when something happened, e.g. "2026-11-09 15:02 JST (2026-11-09 06:02:56 UTC), 26s ago". A time after
// now (a clock ahead of this one) shows as 0s ago.
func Since(t, now time.Time) string {
	return absolute(t) + ", " + Duration(now.Sub(t)) + " ago"
}

// absolute shows t in local time with UTC in brackets, or UTC alone when the local zone at t is UTC, where the two
// would repeat each other
func absolute(t time.Time) string {
	utc := t.UTC().Format(utcLayout)
	local := t.In(time.Local)
	if name, offset := local.Zone(); name == "UTC" && offset == 0 {
		return utc
	}
	return fmt.Sprintf("%s (%s)", local.Format(localLayout), utc)
}

// Duration shows a length of time to the coarsest useful unit, rounded down; a negative one is 0s
func Duration(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}
