//go:build integration

package rule

// Rule expiries are printed in local time, then UTC in brackets, then time remaining (owner feedback from the first production
// deployment), wherever a rule command shows them.

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
)

var expiryLine = regexp.MustCompile(`Expires:\s+\d{4}-\d\d-\d\d \d\d:\d\d TST \(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d UTC\), in (1h 59m|2h 0m)`)

func withTestZone(t *testing.T) {
	t.Helper()
	old := time.Local
	time.Local = time.FixedZone("TST", 11*3600)
	t.Cleanup(func() { time.Local = old })
}

func expiresLines(out string) []string {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "Expires:") {
			lines = append(lines, strings.TrimSpace(l))
		}
	}
	return lines
}

func TestExpiryShownInLocalTime_RuleAllowAndGet(t *testing.T) {
	withTestZone(t)
	ddblocal.Reset(t)
	out, err := run(t, "allow", "-t", "binary", "-i", sha, "--machine", machine, "--expires", "2h", "--yes")
	if err != nil {
		t.Fatalf("allow: %v\n%s", err, out)
	}
	if !expiryLine.MatchString(out) {
		t.Errorf("rule allow's confirmation shows expiry %q; want local time, UTC in brackets and time remaining, e.g. \"Expires: 2026-11-09 22:21 TST (2026-11-09 11:21:42 UTC), in 2h 0m\"\n%s", expiresLines(out), out)
	}

	out, err = run(t, "get", "-t", "binary", "-i", sha, "--machine", machine)
	if err != nil {
		t.Fatalf("get: %v\n%s", err, out)
	}
	if !expiryLine.MatchString(out) {
		t.Errorf("rule get shows expiry %q; want UTC, local time and time remaining\n%s", expiresLines(out), out)
	}

	out, err = run(t, "update", "-t", "binary", "-i", sha, "--machine", machine, "--expires", "2h", "--yes")
	if err != nil {
		t.Fatalf("update: %v\n%s", err, out)
	}
	if !expiryLine.MatchString(out) {
		t.Errorf("rule update's confirmation shows expiry %q; want UTC, local time and time remaining\n%s", expiresLines(out), out)
	}
}
