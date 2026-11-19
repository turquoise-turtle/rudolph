//go:build integration

package group

import (
	"strings"
	"testing"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
)

// `group list` now escapes variation selectors, so a group named with one would show as escapes rather than as the
// name typed; new names must refuse them (owner decision, 2026-11-19). The characters are built from their numbers
// because written literally they would be invisible here too.
func TestGroupCLI_CreateRefusesAVariationSelector(t *testing.T) {
	for _, r := range []rune{0xfe00, 0xfe0f, 0xe0100} {
		ddblocal.Reset(t)
		name := "e" + string(r) + "ng"
		if out, err := group(t, "create", name); err == nil {
			t.Errorf("created a group named %+q:\n%s", name, out)
		}
		if out := mustGroup(t, "list"); strings.Contains(out, "ng") {
			t.Errorf("group list shows a group after a refused create of %+q:\n%s", name, out)
		}
	}
	// A name with a joiner, as Persian spelling needs, is still created.
	mustGroup(t, "create", "eng"+string(rune(0x200c))+"ops")
}
