package termsafe

import (
	"fmt"
	"strings"
	"testing"
	"unicode"
)

// derivedDefaultIgnorable is Unicode's Default_Ignorable_Code_Point (DerivedCoreProperties.txt), derived from Go's own
// tables the way Unicode derives it: Other_Default_Ignorable_Code_Point + Cf + Variation_Selector, minus White_Space,
// U+FFF9-FFFB, U+13430-1343F and Prepended_Concatenation_Mark. The review (2026-11-19) found characters of this set
// that the display left raw, so the whole set is walked rather than a sample.
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

func TestString_EscapesTheWholeDerivedDefaultIgnorableSet(t *testing.T) {
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
	kept := map[rune]bool{0x200c: true, 0x200d: true, 0xfe0f: true}
	for _, r := range set {
		// Between letters, so that no tag can be read as part of a subdivision flag.
		in := "a" + string(r) + "b"
		got := String(in)
		if kept[r] {
			if got != in {
				fail("String with U+%04X = %+q, want it kept as it is", r, got)
			}
			continue
		}
		escape := fmt.Sprintf(`\u%04x`, r)
		if r > 0xffff {
			escape = fmt.Sprintf(`\U%08x`, r)
		}
		if got != "a"+escape+"b" {
			fail("String with U+%04X = %+q, want %+q", r, got, "a"+escape+"b")
		}
		if strings.ContainsRune(got, r) {
			fail("String leaves U+%04X raw", r)
		}
	}
}
