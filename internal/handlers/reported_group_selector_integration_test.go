//go:build integration

package handlers

// From the stored-XSS review (2026-11-19): a Mac must not join a registered group by reporting its name with an
// invisible character added. Normalising the selector away instead of refusing the name would make "eng" plus a
// variation selector count as "eng", so a directory group that merely looks like a registered one would gain its
// rules. The plain name still makes the Mac a member.

import (
	"slices"
	"testing"

	"github.com/airbnb/rudolph/pkg/types"
)

func TestPreflight_ReportedNameWithAVariationSelectorDoesNotJoinTheGroup(t *testing.T) {
	// Built from their numbers: written literally they would be invisible here too.
	for _, r := range []rune{0xfe00, 0xfe0e, 0xfe0f, 0xe0100, 0x034f, 0x180b} {
		transport := groupsServer(t)
		registerGroups(t, map[string]bool{"eng": true})
		putGroupRule(t, "eng", sha1, types.RuleTypeBinary, types.RulePolicyBlocklist, "")

		h := newHost(t, machineA, transport)
		h.PrimaryUserGroups = []string{"eng" + string(r), "e" + string(r) + "ng"}
		syncNormal(t, h)
		if got := h.Policy("BINARY", sha1); got != "" {
			t.Errorf("reported eng with U+%04X: the Mac got the group rule (%q)", r, got)
		}
		if got := reportedAttribute(t, machineA); slices.Contains(got, "eng") {
			t.Errorf("reported eng with U+%04X: Reported %+q, want eng absent", r, got)
		}
		if f := readMemberRow(t, "eng", machineA); f.exists {
			t.Errorf("reported eng with U+%04X: a member row for eng was written (%+v)", r, f)
		}

		h.PrimaryUserGroups = []string{"eng"}
		syncNormal(t, h)
		if got := h.Policy("BINARY", sha1); got != "BLOCKLIST" {
			t.Errorf("reported plain eng after U+%04X: group rule %q, want BLOCKLIST", r, got)
		}
		if got := reportedAttribute(t, machineA); !slices.Contains(got, "eng") {
			t.Errorf("reported plain eng after U+%04X: Reported %+q, want eng", r, got)
		}
	}
}
