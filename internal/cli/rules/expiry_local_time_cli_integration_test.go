//go:build integration

package rules

// `rules -m` prints each machine rule's expiry in local time, then UTC in brackets, then time remaining (owner feedback from the first
// production deployment).

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/cli/rule"
	"github.com/airbnb/rudolph/internal/testutil/clitest"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
)

func TestExpiryShownInLocalTime_RulesMachine(t *testing.T) {
	old := time.Local
	time.Local = time.FixedZone("TST", 11*3600)
	t.Cleanup(func() { time.Local = old })

	ddblocal.Reset(t)
	const mac = "AAAAAAAA-1111-4111-8111-AAAAAAAAAAAA"
	sha := strings.Repeat("ef", 32)
	if out, err := clitest.Run(t, qolEnv, rule.RuleCmd, "", "allow", "-t", "binary", "-i", sha, "--machine", mac, "--expires", "2h", "--yes"); err != nil {
		t.Fatalf("allow: %v\n%s", err, out)
	}
	out, err := clitest.Run(t, qolEnv, RulesCmd, "", "-m", mac)
	if err != nil {
		t.Fatalf("rules -m: %v\n%s", err, out)
	}
	want := regexp.MustCompile(`Expires: \d{4}-\d\d-\d\d \d\d:\d\d TST \(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d UTC\), in (1h 59m|2h 0m)`)
	if !want.MatchString(out) {
		t.Errorf("rules -m shows the expiry without local time and time remaining:\n%s", out)
	}
}
