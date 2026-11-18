// Package termsafe makes values that Macs reported, or that were stored by other tools, safe to write to a terminal.
//
// A host name, user name, group name or audit summary can carry escape sequences that clear the screen, rewrite
// earlier lines, set the window title or plant links, and bidi or zero-width characters that make text read as
// something it is not. The CLI writes such characters as visible Go-style escapes (\x1b, \n, ‮) instead.
package termsafe

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	blackFlag  = 0x1f3f4
	tagFirst   = 0xe0020
	tagLast    = 0xe007e
	cancelTag  = 0xe007f
	zwnj, zwj  = 0x200c, 0x200d
	brailleGap = 0x2800
)

// String returns s with every character a terminal would act on, or that hides or reorders text, written as a visible
// escape: C0 controls and DEL as \xNN (\n, \r and \t by name), C1 controls, invisible format characters (Unicode Cf),
// line and paragraph separators, and characters that render as blank text without being spaces (Hangul fillers and
// the blank braille pattern) as \uXXXX (\UXXXXXXXX above U+FFFF), and bytes that are not UTF-8 as \xNN.
//
// Zero-width (non-)joiners are kept, as emoji sequences and several scripts need them, and so are the tag characters
// of a subdivision flag (U+1F3F4, tags, then the cancel tag U+E007F); tags anywhere else are escaped. Everything else,
// backslashes included, is left as it is.
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

// flagTags returns the length in bytes of the tag characters at the start of s that complete a subdivision flag: one
// or more tags ending with the cancel tag. It returns 0 when s does not start with such a sequence.
func flagTags(s string) int {
	n := 0
	for n < len(s) {
		r, size := utf8.DecodeRuneInString(s[n:])
		switch {
		case r == cancelTag && n > 0:
			return n + size
		case r >= tagFirst && r <= tagLast:
			n += size
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
	switch r {
	case zwnj, zwj:
		return false
	case 0x115f, 0x1160, 0x3164, 0xffa0, brailleGap:
		// Hangul fillers and the blank braille pattern are letters or symbols that render as nothing, so a value can
		// look shorter or blank while it is not.
		return true
	}
	return unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp)
}
