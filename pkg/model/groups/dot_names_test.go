package groups

import "testing"

// rudolph-web serves a group at /groups/<name>; "." and ".." are path segments a browser and the router resolve
// away, so such a group would have no reachable page (rudolph-web review). They are refused like other invalid
// names, and ignored when reported.
func TestNormalise_RefusesDotNames(t *testing.T) {
	for _, name := range []string{".", "..", " . ", " .. "} {
		if got, err := Normalise(name); err == nil {
			t.Errorf("Normalise(%q) = %q, want an error", name, got)
		}
	}
	for _, ok := range []string{".eng", "eng.", "a..b", "..."} {
		if _, err := Normalise(ok); err != nil {
			t.Errorf("Normalise(%q): %v, want it accepted", ok, err)
		}
	}
	got, dropped := NormaliseList([]string{"eng", ".", ".."})
	if len(got) != 1 || got[0] != "eng" || dropped != 2 {
		t.Errorf("NormaliseList = %v, dropped %d; want [eng], 2", got, dropped)
	}
}
