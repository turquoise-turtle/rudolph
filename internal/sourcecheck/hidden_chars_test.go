// Package sourcecheck holds repository-wide checks on the source and docs themselves.
package sourcecheck

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode"
)

// hidden reports runes that make text display differently from what it is ("Trojan Source"): every default-ignorable
// code point (bidi controls, zero-width characters, the byte order mark, the soft hyphen, variation selectors, tag
// characters and the rest) and the blank-looking Hangul fillers and braille pattern. It is the table the CLI's display
// escaper and group names are held to (stored-XSS review, 2026-11-18 and 2026-11-19), whose own tooling wrote some of
// these raw into docs and comments, so every tracked text file is checked. ZWNJ, ZWJ and the emoji presentation
// selector are allowed, as emoji and several scripts need them; tag characters are allowed only where they complete a
// subdivision flag (see flagTagsAt).
func hidden(r rune) bool {
	switch r {
	case 0x200c, 0x200d, 0xfe0f:
		return false
	case 0x115f, 0x1160, 0x3164, 0xffa0, 0x2800:
		return true
	}
	// Go has no table for the derived Default_Ignorable_Code_Point, so its sources are checked instead. What Unicode
	// subtracts from them (U+FFF9-FFFB, U+13430-1343F, the prepended concatenation marks) stays included: those are
	// format characters that hide or rearrange text just the same, and the CLI escapes them too.
	return !unicode.Is(unicode.White_Space, r) &&
		unicode.In(r, unicode.Other_Default_Ignorable_Code_Point, unicode.Cf, unicode.Variation_Selector)
}

// flagTagsAt returns how many runes from line[i] on are the tag characters of a subdivision flag that line[i-1] (the
// black flag U+1F3F4) starts: 2 to 7 tag letters a-z or digits, then the cancel tag U+E007F. It returns 0 for any
// other tag text, which would show as a plain flag while spelling something hidden.
func flagTagsAt(line []rune, i int) int {
	if i == 0 || line[i-1] != 0x1f3f4 {
		return 0
	}
	for n := 0; i+n < len(line) && n <= 7; n++ {
		r := line[i+n]
		switch {
		case r == 0xe007f && n >= 2:
			return n + 1
		case (r >= 0xe0061 && r <= 0xe007a) || (r >= 0xe0030 && r <= 0xe0039):
		default:
			return 0
		}
	}
	return 0
}

var textExt = map[string]bool{".go": true, ".md": true, ".tf": true, ".tfvars": true, ".html": true, ".js": true,
	".css": true, ".sh": true, ".py": true, ".json": true, ".txt": true, ".yml": true, ".yaml": true, ".hcl": true,
	".plist": true, ".mobileconfig": true, ".example": true}

var skipDir = map[string]bool{".git": true, ".claude": true, ".dynamodb-local": true, "node_modules": true, "vendor": true,
	".terraform": true, "build": true, ".local": true}

func TestNoHiddenCharactersInSourceOrDocs(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (skipDir[d.Name()] || strings.HasPrefix(d.Name(), ".terraform")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !textExt[filepath.Ext(path)] && d.Name() != "Makefile" {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for line := 1; sc.Scan(); line++ {
			runes := []rune(sc.Text())
			for i := 0; i < len(runes); i++ {
				if n := flagTagsAt(runes, i); n > 0 {
					i += n - 1
					continue
				}
				if r := runes[i]; hidden(r) {
					t.Errorf("%s:%d contains U+%04X written raw; write it as an escape (\\u%04x) instead", path, line, r, r)
					break
				}
			}
		}
		return sc.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The check must cover every character the CLI escapes, or source and docs could carry one the display never would.
func TestHiddenCoversEveryDefaultIgnorableAndBlankCharacter(t *testing.T) {
	missed := 0
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !unicode.In(r, unicode.Other_Default_Ignorable_Code_Point, unicode.Cf, unicode.Variation_Selector) ||
			unicode.Is(unicode.White_Space, r) {
			continue
		}
		want := r != 0x200c && r != 0x200d && r != 0xfe0f
		if hidden(r) != want {
			if missed++; missed <= 20 {
				t.Errorf("hidden(U+%04X) = %t, want %t", r, !want, want)
			}
		}
	}
	for _, r := range []rune{0x115f, 0x1160, 0x3164, 0xffa0, 0x2800} {
		if !hidden(r) {
			t.Errorf("hidden(U+%04X) = false; the blank-looking character must be reported", r)
		}
	}
}

func TestFlagTagsAtAcceptsOnlyWholeSubdivisionFlags(t *testing.T) {
	tags := func(s string) []rune {
		var out []rune
		for _, c := range s {
			out = append(out, 0xe0000+c)
		}
		return out
	}
	flag := rune(0x1f3f4)
	cancel := rune(0xe007f)
	for name, tc := range map[string]struct {
		line []rune
		want int
	}{
		"Wales":                 {append(append([]rune{flag}, tags("gbwls")...), cancel), 6},
		"two tags":              {append(append([]rune{flag}, tags("ab")...), cancel), 3},
		"one tag":               {append(append([]rune{flag}, tags("a")...), cancel), 0},
		"eight tags":            {append(append([]rune{flag}, tags("abcdefgh")...), cancel), 0},
		"upper-case tag":        {append(append([]rune{flag}, tags("GBWLS")...), cancel), 0},
		"no cancel tag":         {append([]rune{flag}, tags("gbwls")...), 0},
		"tags without the flag": {append(append([]rune{'x'}, tags("gbwls")...), cancel), 0},
	} {
		if got := flagTagsAt(tc.line, 1); got != tc.want {
			t.Errorf("%s: flagTagsAt = %d, want %d", name, got, tc.want)
		}
	}
}
