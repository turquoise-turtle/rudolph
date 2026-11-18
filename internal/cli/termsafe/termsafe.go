// Package termsafe makes values that Macs reported, or that were stored by other tools, safe to write to a terminal.
//
// A host name, user name, group name or audit summary can carry escape sequences that clear the screen, rewrite
// earlier lines, set the window title or plant links, and bidi or zero-width characters that make text read as
// something it is not. The CLI writes such characters as visible Go-style escapes (\x1b, \n, \u202e) instead.
package termsafe

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	blackFlag = 0x1f3f4
	tagLowerA = 0xe0061
	tagLowerZ = 0xe007a
	tagDigit0 = 0xe0030
	tagDigit9 = 0xe0039
	cancelTag = 0xe007f

	// A subdivision flag's tags spell a region code (Unicode CLDR): 2 to 7 lower-case letters and digits
	minFlagTags, maxFlagTags = 2, 7
	zwnj, zwj                = 0x200c, 0x200d
	// Hearts, keycaps and many other emoji need the emoji presentation selector (VS16) to show as emoji.
	emojiSelector = 0xfe0f
	brailleGap    = 0x2800
)

// String returns s with every character a terminal would act on, or that hides or reorders text, written as a visible
// escape: C0 controls and DEL as \xNN (\n, \r and \t by name), C1 controls, invisible format characters (Unicode Cf),
// line and paragraph separators, every other default-ignorable code point (variation selectors, the combining
// grapheme joiner, Khmer inherent vowels, and reserved code points such as U+2065, U+FFF0-FFF8 and the rest of the
// U+E0000-E0FFF block), and characters that render as blank text without being spaces (Hangul fillers and the blank
// braille pattern) as \uXXXX (\UXXXXXXXX above U+FFFF), and bytes that are not UTF-8 as \xNN.
//
// Zero-width (non-)joiners are kept, as emoji sequences and several scripts need them, and so are the emoji
// presentation selector U+FE0F and the tag characters of a subdivision flag (U+1F3F4, 2 to 7 tag letters a-z or
// digits, then the cancel tag U+E007F); tags anywhere else are escaped. Everything else, backslashes included, is left
// as it is.
func String(s string) string {
	if strings.IndexFunc(s, needsEscape) < 0 && utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case r == blackFlag:
			flag := size + flagTags(s[i+size:])
			b.WriteString(s[i : i+flag])
			size = flag
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case needsEscape(r) && r > 0xffff:
			fmt.Fprintf(&b, `\U%08x`, r)
		case needsEscape(r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteRune(r)
		}
		i += size
	}
	return b.String()
}

// flagTags returns the length in bytes of the tag characters at the start of s that complete a subdivision flag: the
// 2 to 7 tag letters (a-z) and digits of a region code such as gbwls, then the cancel tag. It returns 0 for any other
// tag text, which would print as a plain flag while hiding what it spells, so its tags are escaped.
func flagTags(s string) int {
	n, count := 0, 0
	for n < len(s) {
		r, size := utf8.DecodeRuneInString(s[n:])
		switch {
		case r == cancelTag && count >= minFlagTags:
			return n + size
		case count < maxFlagTags && ((r >= tagLowerA && r <= tagLowerZ) || (r >= tagDigit0 && r <= tagDigit9)):
			n += size
			count++
		default:
			return 0
		}
	}
	return 0
}

// Strings applies String to each value, returning a new slice.
func Strings(values []string) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = String(v)
	}
	return out
}

// Error returns err with its message escaped by String, for errors that quote stored or file-supplied values. It
// unwraps to err, so errors.Is and errors.As still see the original.
func Error(err error) error {
	if err == nil {
		return nil
	}
	return escapedError{err}
}

type escapedError struct{ err error }

func (e escapedError) Error() string { return String(e.err.Error()) }
func (e escapedError) Unwrap() error { return e.err }

func needsEscape(r rune) bool {
	if r < utf8.RuneSelf {
		// Most values are ASCII, so they skip the table lookups below.
		return r < 0x20 || r == 0x7f
	}
	switch r {
	case zwnj, zwj, emojiSelector:
		return false
	case 0x115f, 0x1160, 0x3164, 0xffa0, brailleGap:
		// Hangul fillers and the blank braille pattern are letters or symbols that render as nothing, so a value can
		// look shorter or blank while it is not.
		return true
	}
	// Unicode derives Default_Ignorable_Code_Point (DerivedCoreProperties.txt) as Other_Default_Ignorable_Code_Point +
	// Cf + Variation_Selector, minus White_Space, U+FFF9-FFFB, U+13430-1343F and Prepended_Concatenation_Mark. Go has
	// no table for the derived property, so its three sources are checked instead. What Unicode subtracts is all Cf,
	// and stays escaped: those characters still hide or rearrange text in a terminal, which renders no annotations,
	// hieroglyph layout or number signs. No White_Space character is in either other source.
	return unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp,
		unicode.Other_Default_Ignorable_Code_Point, unicode.Variation_Selector)
}
