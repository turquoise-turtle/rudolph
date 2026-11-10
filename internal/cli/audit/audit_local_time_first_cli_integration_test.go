//go:build integration

package audit

// The audit table shows local time before UTC, as every other time the CLI prints (owner request after the first
// production deployment). Summaries keep plain UTC: they are stored and read later from anywhere.

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/cli/group"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
)

func TestAuditTable_LocalTimeComesBeforeUTC(t *testing.T) {
	old := time.Local
	time.Local = time.FixedZone("TST", 11*3600)
	t.Cleanup(func() { time.Local = old })

	ddblocal.Reset(t)
	run(t, group.GroupCmd, "", "create", "eng")
	out := run(t, AuditCmd, "", "--since", "1h")

	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		t.Fatalf("want a header and an entry:\n%s", out)
	}
	if header := strings.Fields(lines[0]); len(header) < 4 || strings.Join(header[:4], " ") != "LOCAL TIME TIME (UTC)" {
		t.Errorf("header %q: want LOCAL TIME first, then TIME (UTC)", lines[0])
	}
	if row := regexp.MustCompile(`^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d TST\s+\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ\s`); !row.MatchString(lines[1]) {
		t.Errorf("entry %q: want local time, then UTC", lines[1])
	}
}

func TestAuditNoEntries_SinceIsLocalTimeFirst(t *testing.T) {
	old := time.Local
	time.Local = time.FixedZone("TST", 11*3600)
	t.Cleanup(func() { time.Local = old })

	ddblocal.Reset(t)
	out := run(t, AuditCmd, "", "--since", "1h")
	if !regexp.MustCompile(`No audit entries since \d{4}-\d\d-\d\d \d\d:\d\d TST \(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d UTC\)`).MatchString(out) {
		t.Errorf("empty audit: %q; want the since time in local time first, then UTC in brackets", out)
	}
}
