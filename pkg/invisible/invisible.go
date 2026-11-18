// Package invisible classifies characters that a terminal or browser would act on, or that hide or reorder text. The
// CLI's display escaper (internal/cli/termsafe) escapes them and group names refuse them, so both share one table.
package invisible

import (
	"unicode"
	"unicode/utf8"
)

const (
	zwnj, zwj  = 0x200c, 0x200d
	brailleGap = 0x2800
)

// Rune reports whether r is a control character (C0, DEL or C1), an invisible format character (Unicode Cf), a line
// or paragraph separator, any other default-ignorable code point (variation selectors, the combining grapheme joiner,
// Khmer inherent vowels, reserved code points such as U+2065, U+FFF0-FFF8 and the rest of U+E0000-E0FFF), or a
// character that renders as blank text without being a space (Hangul fillers and the blank braille pattern).
//
// The zero-width non-joiner and joiner are not: emoji sequences, Persian and several Indic scripts need them in
// ordinary text.
func Rune(r rune) bool {
	if r < utf8.RuneSelf {
		// Most values are ASCII, so they skip the table lookups below.
		return r < 0x20 || r == 0x7f
	}
	switch r {
	case zwnj, zwj:
		return false
	case 0x115f, 0x1160, 0x3164, 0xffa0, brailleGap:
		// Hangul fillers and the blank braille pattern are letters or symbols that render as nothing, so a value can
		// look shorter or blank while it is not.
		return true
	}
	// Unicode derives Default_Ignorable_Code_Point (DerivedCoreProperties.txt) as Other_Default_Ignorable_Code_Point +
	// Cf + Variation_Selector, minus White_Space, U+FFF9-FFFB, U+13430-1343F and Prepended_Concatenation_Mark. Go has
	// no table for the derived property, so its three sources are checked instead. What Unicode subtracts is all Cf,
	// and stays included: those characters still hide or rearrange text in a terminal, which renders no annotations,
	// hieroglyph layout or number signs. No White_Space character is in either other source.
	return unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp,
		unicode.Other_Default_Ignorable_Code_Point, unicode.Variation_Selector)
}
