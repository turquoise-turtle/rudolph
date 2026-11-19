package termsafe

import (
	"fmt"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// Variation selectors and the other default-ignorable code points render as nothing, so a run of them after a visible
// letter can carry hidden data ("emoji smuggling": one selector per byte) or make two different values look the same.
// Owner approved on 2026-11-19 that the CLI escapes every Default_Ignorable_Code_Point, keeping only what real text
// needs: ZWNJ and ZWJ, the emoji presentation selector U+FE0F, and the tag characters of a subdivision flag.
//
// The source spells these characters as rune numbers rather than string escapes, so no invisible character can slip
// into the file itself (internal/sourcecheck).

// runes builds a string from code points.
func runes(rs ...rune) string { return string(rs) }

// escaped is the visible escape String writes for r: \u and four lower-case hex digits up to U+FFFF, \U and eight above.
func escaped(r rune) string {
	if r > 0xffff {
		return fmt.Sprintf(`\U%08x`, r)
	}
	return fmt.Sprintf(`\u%04x`, r)
}

func TestString_EscapesDefaultIgnorables(t *testing.T) {
	for _, r := range []rune{
		0xfe00, 0xfe01, 0xfe0e, // variation selectors 1, 2 and 15 (text presentation)
		0xe0100, 0xe0150, 0xe01ef, // variation selectors supplement
		0x034f,                         // combining grapheme joiner
		0x180b, 0x180c, 0x180d, 0x180f, // Mongolian free variation selectors
		0x17b4, 0x17b5, // Khmer inherent vowels
		0xfff0, 0xfff8, // reserved default-ignorables
		0x2065,           // reserved in the invisible-operator block
		0x1d173, 0x1d17a, // musical symbol formatting
		0x1bca0, 0x1bca3, // shorthand format controls
		0xe0000, 0xe0080, 0xe01f0, 0xe0fff, // reserved in the tag and selector planes
	} {
		in := "a" + string(r) + "b"
		want := "a" + escaped(r) + "b"
		got := String(in)
		if got != want {
			t.Errorf("String(%+q) = %+q, want %+q", in, got, want)
		}
		if strings.ContainsRune(got, r) {
			t.Errorf("String(%+q) still has %U raw", in, r)
		}
	}
}

// Every variation selector except U+FE0F, and every code point Unicode lists as Other_Default_Ignorable_Code_Point,
// is escaped wherever it appears. Only the first few misses are listed, as the tables hold thousands of code points.
func TestString_EscapesEveryVariationSelectorAndOtherDefaultIgnorable(t *testing.T) {
	var missed []rune
	total := 0
	for _, table := range []*unicode.RangeTable{unicode.Variation_Selector, unicode.Other_Default_Ignorable_Code_Point} {
		var all []rune
		for _, r16 := range table.R16 {
			for r := rune(r16.Lo); r <= rune(r16.Hi); r += rune(r16.Stride) {
				all = append(all, r)
			}
		}
		for _, r32 := range table.R32 {
			for r := rune(r32.Lo); r <= rune(r32.Hi); r += rune(r32.Stride) {
				all = append(all, r)
			}
		}
		for _, r := range all {
			if r == 0xfe0f {
				continue
			}
			total++
			if String("x"+string(r)) != "x"+escaped(r) {
				missed = append(missed, r)
			}
		}
	}
	if len(missed) > 0 {
		shown := missed[:min(len(missed), 8)]
		t.Errorf("%d of %d default-ignorable code points are not escaped, including %U", len(missed), total, shown)
	}
}

// A visible letter followed by selectors that encode bytes: U+FE00-FE0F for 0-15, U+E0100-E01EF for 16-255.
func TestString_EscapesSmuggledSelectorPayload(t *testing.T) {
	payload := []byte("rm -rf ~\x01")
	var in, want strings.Builder
	in.WriteString("A")
	want.WriteString("A")
	for _, c := range payload {
		r := rune(0xfe00) + rune(c)
		if c >= 16 {
			r = rune(0xe0100) + rune(c) - 16
		}
		in.WriteRune(r)
		want.WriteString(escaped(r))
	}
	in.WriteString(".local")
	want.WriteString(".local")

	got := String(in.String())
	if got != want.String() {
		t.Errorf("String(%+q) = %+q, want %+q", in.String(), got, want.String())
	}
	if i := strings.IndexFunc(got, func(r rune) bool { return unicode.Is(unicode.Variation_Selector, r) }); i >= 0 {
		r, _ := utf8.DecodeRuneInString(got[i:])
		t.Errorf("String(%+q) still has selector %U raw at byte %d", in.String(), r, i)
	}
	if !strings.HasPrefix(got, `A\U000e0162\U000e015d`) {
		t.Errorf("String(%+q) = %+q, want it to start with the escapes of r and m", in.String(), got)
	}
	if !strings.Contains(got, `\ufe01.local`) {
		t.Errorf("String(%+q) = %+q, want the 0x01 byte as \\ufe01", in.String(), got)
	}
}

// Emoji, joined scripts, subdivision flags and ordinary non-ASCII text stay as they are.
func TestString_KeepsTextThatNeedsJoinersAndEmojiSelector(t *testing.T) {
	const (
		vs16 = 0xfe0f
		zwj  = 0x200d
		zwnj = 0x200c
	)
	for _, s := range []string{
		runes(0x2764, vs16),      // red heart, emoji presentation
		runes('1', vs16, 0x20e3), // keycap 1
		runes(0x1f468, zwj, 0x1f469, zwj, 0x1f467, zwj, 0x1f466),             // family
		runes(0x2764, vs16, zwj, 0x1f525),                                    // heart on fire
		runes(0x1f3f3, vs16, zwj, 0x1f308),                                   // rainbow flag
		runes(0x0645, 0x06cc, zwnj, 0x062e, 0x0648, 0x0627, 0x0647, 0x0645),  // Persian "I want", ZWNJ after the prefix
		runes(0x1f3f4, 0xe0067, 0xe0062, 0xe0077, 0xe006c, 0xe0073, 0xe007f), // Wales
		runes(0x1f3f4, 0xe0067, 0xe0062, 0xe0073, 0xe0063, 0xe0074, 0xe007f), // Scotland
		"Jürgen’s Mac café naïve 工程 東京 서울",
		runes('e', 0x0301), // e with a combining acute accent
	} {
		if got := String(s); got != s {
			t.Errorf("String(%+q) = %+q, want it unchanged", s, got)
		}
	}
}
