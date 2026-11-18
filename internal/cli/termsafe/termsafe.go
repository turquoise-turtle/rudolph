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

// String returns s with every character a terminal would act on, or that hides or reorders text, written as a visible
// escape: C0 controls and DEL as \xNN (\n, \r and \t by name), C1 controls, invisible format characters (Unicode Cf)
// and line and paragraph separators as \uXXXX (\UXXXXXXXX above U+FFFF), and bytes that are not UTF-8 as \xNN.
// Zero-width (non-)joiners are kept, as emoji sequences and several scripts need them; everything else, backslashes
// included, is left as it is.
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

// Strings applies String to each value, returning a new slice.
func Strings(values []string) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = String(v)
	}
	return out
}

func needsEscape(r rune) bool {
	switch {
	case r == 0x200c || r == 0x200d:
		return false
	case unicode.IsControl(r), unicode.Is(unicode.Cf, r), unicode.Is(unicode.Zl, r), unicode.Is(unicode.Zp, r):
		return true
	}
	return false
}
