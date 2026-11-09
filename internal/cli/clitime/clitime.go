// Package clitime formats the times the CLI prints in UTC, in the operator's local time and relative to now, so an
// operator outside UTC does not have to convert. Audit record summaries are stored, so they keep plain UTC instead.
package clitime

import (
	"fmt"
	"time"
)

const localLayout = "2006-01-02 15:04 MST"

// Expiry shows when something ends, e.g. "2026-11-09T11:21:42Z (2026-11-09 20:21 JST local, in 2h 0m)"; the zero
// time is never. An end at or before now has expired.
func Expiry(t, now time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := t.Sub(now)
	relative := "in " + Duration(d)
	if d <= 0 {
		relative = "expired " + Duration(-d) + " ago"
	}
	return fmt.Sprintf("%s (%s local, %s)", t.UTC().Format(time.RFC3339), t.In(time.Local).Format(localLayout), relative)
}

// Since shows when something happened, e.g. "2026-11-09T06:02:56Z (2026-11-09 15:02 JST local, 26s ago)". A time
// after now (a clock ahead of this one) shows as 0s ago.
func Since(t, now time.Time) string {
	return fmt.Sprintf("%s (%s local, %s ago)", t.UTC().Format(time.RFC3339), t.In(time.Local).Format(localLayout), Duration(now.Sub(t)))
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
