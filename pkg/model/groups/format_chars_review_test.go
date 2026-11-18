package groups

import "testing"

// Review of the format-character rule (2026-11-18): ZWNJ (U+200C) and ZWJ (U+200D) are part of ordinary spelling in
// Persian and several Indic scripts, so names using them stay valid; only the other invisible format characters are
// refused.
func TestNormalise_KeepsJoinersUsedInOrdinaryNames(t *testing.T) {
	for _, name := range []string{"می\u200cخواهم", "क्\u200dष", "eng\u200cops"} {
		if _, err := Normalise(name); err != nil {
			t.Errorf("Normalise(%q): %v, want it accepted", name, err)
		}
	}
	got, dropped := NormaliseList([]string{"می\u200cخواهم", "eng"})
	if len(got) != 2 || dropped != 0 {
		t.Errorf("NormaliseList = %q, dropped %d; want both kept", got, dropped)
	}
}
