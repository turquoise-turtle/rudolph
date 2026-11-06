package types

import "testing"

// Which clients are sent what (docs/sync.md, client versions). Releases older than the 2025.12 floor are unsupported,
// but they compile path regexes as received and skip unknown policies just like 2025.12, so they are treated the same:
// sending them an empty regex would match every path.
func TestSantaVersion_Adaptations(t *testing.T) {
	for _, c := range []struct {
		reported                         string
		clearsEmptyRegex, knowsSilentGUI bool
	}{
		{"2026.9.0", true, true},
		{"2026.10.0", true, true},
		{"2026.8", true, true},
		{"2026.8.1", true, true},
		{"2026.7.0", false, true},
		{"2026.6.0", false, true},
		{"2026.5.2", false, false},
		{"2025.12.0", false, false},
		{"2025.12", false, false},
		{"2024.2", false, false},
		{"2021.1", false, false},
		{"", true, true},
		{"not a version", true, true},
	} {
		v := ParseSantaVersion(c.reported)
		if got := v.ClearsRegexWhenEmpty(); got != c.clearsEmptyRegex {
			t.Errorf("%q: ClearsRegexWhenEmpty %v, want %v", c.reported, got, c.clearsEmptyRegex)
		}
		if got := v.KnowsSilentGUITTYPolicies(); got != c.knowsSilentGUI {
			t.Errorf("%q: KnowsSilentGUITTYPolicies %v, want %v", c.reported, got, c.knowsSilentGUI)
		}
	}
}
