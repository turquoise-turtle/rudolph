package groups

import (
	"testing"
	"unicode"
)

// derivedDefaultIgnorable is Unicode's Default_Ignorable_Code_Point (DerivedCoreProperties.txt), derived from Go's own
// tables the way Unicode derives it: Other_Default_Ignorable_Code_Point + Cf + Variation_Selector, minus White_Space,
// U+FFF9-FFFB, U+13430-1343F and Prepended_Concatenation_Mark. New group names must not need escaping for display
// (owner decision, 2026-11-19), so the whole set is walked rather than a sample.
func derivedDefaultIgnorable() []rune {
	var set []rune
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !unicode.In(r, unicode.Other_Default_Ignorable_Code_Point, unicode.Cf, unicode.Variation_Selector) {
			continue
		}
		if unicode.Is(unicode.White_Space, r) || (r >= 0xfff9 && r <= 0xfffb) || (r >= 0x13430 && r <= 0x1343f) ||
			unicode.Is(unicode.Prepended_Concatenation_Mark, r) {
			continue
		}
		set = append(set, r)
	}
	return set
}

func TestNormalise_RefusesTheWholeDerivedDefaultIgnorableSet(t *testing.T) {
	set := derivedDefaultIgnorable()
	if len(set) < 4000 {
		// Over 4,000 code points, most of them the reserved U+E0000-E0FFF block; far fewer means the derivation broke.
		t.Fatalf("derived %d default-ignorable code points; the derivation is wrong", len(set))
	}
	failures := 0
	fail := func(format string, args ...any) {
		// A broken classification fails thousands of code points; the first few and the count are enough to read.
		if failures++; failures <= 20 {
			t.Errorf(format, args...)
		}
	}
	defer func() {
		if failures > 20 {
			t.Errorf("... %d code points fail in all", failures)
		}
	}()
	for _, r := range set {
		name := "e" + string(r) + "ng"
		got, err := Normalise(name)
		switch r {
		case 0x200c, 0x200d:
			// Persian and several Indic scripts need the joiners in ordinary spelling.
			if err != nil {
				fail("Normalise with U+%04X: %v, want it accepted", r, err)
			}
		default:
			if err == nil {
				fail("Normalise with U+%04X = %+q, want an error", r, got)
			}
		}
	}
}
