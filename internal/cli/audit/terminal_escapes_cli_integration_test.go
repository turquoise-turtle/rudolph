//go:build integration

package audit

import (
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/pkg/clock"
	modelaudit "github.com/airbnb/rudolph/pkg/model/audit"
)

// Audit entries carry host names (the CLI actor is cli:<user>@<host>), Entra names and group names. `rudolph audit`
// shows terminal control, bidi and zero-width characters in them as visible escapes instead of writing them
// (stored-XSS review, 2026-11-18).
func TestAudit_EscapesTerminalControl(t *testing.T) {
	ddblocal.Reset(t)
	err := modelaudit.Record(clock.ConcreteTimeProvider{}, ddblocal.Client(t), modelaudit.Entry{
		Actor: "cli:admin@evil\x1b[2J\x1b]8;;https://x.example\x07", Action: "rule.add", Target: "group:eng",
		Summary: "added \u202etxt.exe\nFAKE: forged entry",
	})
	if err != nil {
		t.Fatal(err)
	}
	out := run(t, AuditCmd, "", "--since", time.Now().Add(-time.Hour).UTC().Format(time.RFC3339))
	for i, r := range out {
		if r != '\n' && r != '\t' && (r < 0x20 || (r >= 0x7f && r <= 0x9f) || r == 0x202e) {
			t.Fatalf("output has %U at byte %d:\n%q", r, i, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "FAKE:") {
			t.Errorf("a newline in a summary started a line of its own: %q", line)
		}
	}
	for _, want := range []string{`evil\x1b[2J`, `\u202etxt.exe\nFAKE: forged entry`} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not show %q:\n%s", want, out)
		}
	}
}
