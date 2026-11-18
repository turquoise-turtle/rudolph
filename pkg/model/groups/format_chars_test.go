package groups

import "testing"

// A Mac reports its directory groups, and rudolph-web offers a reported name as a new group and puts group names in
// copy-paste commands. Bidi overrides, zero-width and other invisible format characters would make a name display as
// something it is not (stored-XSS review, 2026-11-18), so they are refused like control characters.
func TestNormalise_RefusesInvisibleFormatCharacters(t *testing.T) {
	for _, name := range []string{
		"eng\u202e", "\u202egne", "en\u200bg", "eng\u2066", "eng\u2069", "\ufeffeng", "en\u00adg", "eng\u200e", "eng\u061c",
	} {
		if got, err := Normalise(name); err == nil {
			t.Errorf("Normalise(%q) = %q, want an error", name, got)
		}
	}
	for _, ok := range []string{"jürgen", "工程", "équipe-ops", "lab 2"} {
		if _, err := Normalise(ok); err != nil {
			t.Errorf("Normalise(%q): %v, want it accepted", ok, err)
		}
	}
	got, dropped := NormaliseList([]string{"eng", "en\u200bg", "ops\u202e"})
	if len(got) != 1 || got[0] != "eng" || dropped != 2 {
		t.Errorf("NormaliseList = %q, dropped %d; want [eng], 2", got, dropped)
	}
}
