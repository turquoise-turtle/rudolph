//go:build integration

package machine

import (
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/clitest"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
)

// Review of the terminal escaping (2026-11-18): subdivision flags are U+1F3F4 followed by tag characters (Cf). They
// are ordinary emoji, so they are printed as they are, as docs/cli.md says.
func TestMachineShow_KeepsSubdivisionFlagEmoji(t *testing.T) {
	ddblocal.Reset(t)
	host := "Gareth's Mac \U0001F3F4\U000E0067\U000E0062\U000E0077\U000E006C\U000E0073\U000E007F"
	seedMac(t, macA, host, "C02ABC123", time.Now())
	out, err := clitest.Run(t, env(), MachineCmd, "", "show", "-m", macA)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	mustContain(t, out, host)
}
