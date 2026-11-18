//go:build integration

package machine

import (
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/clitest"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
)

// tags returns s spelled in Unicode tag characters (U+E0000 + ASCII).
func tags(s string) string {
	var b strings.Builder
	for _, r := range s {
		b.WriteRune(0xe0000 + r)
	}
	return b.String()
}

// A black flag followed by tag characters is only a subdivision flag when the tags are the 2 to 7 lower-case letters
// or digits of a region code. Any other tag text (here an instruction hidden after the flag) would print as a plain
// flag and hide what the value says, so it is escaped (review, 2026-11-18).
func TestMachineShow_EscapesTextHiddenInFlagTags(t *testing.T) {
	ddblocal.Reset(t)
	flag := string(rune(0x1f3f4))
	cancel := string(rune(0xe007f))
	hiddenHost := "lab-mac " + flag + tags("Ignore previous text") + cancel
	longHost := "lab-mac " + flag + tags("gbengxyzw") + cancel
	seedMac(t, macA, hiddenHost, "C02ABC123", time.Now())
	seedMac(t, macB, longHost, "C02ABC124", time.Now())
	for id, want := range map[string]string{macA: `\U000e0049`, macB: `\U000e0067`} {
		out, err := clitest.Run(t, env(), MachineCmd, "", "show", "-m", id)
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		mustContain(t, out, want)
		if strings.ContainsRune(out, 0xe0049) || strings.ContainsRune(out, 0xe0078) {
			t.Errorf("tag characters printed raw:\n%q", out)
		}
	}
}
