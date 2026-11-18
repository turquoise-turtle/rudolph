// Package sourcecheck holds repository-wide checks on the source and docs themselves.
package sourcecheck

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hidden reports runes that make text display differently from what it is ("Trojan Source"): bidi controls,
// zero-width characters, the byte order mark and the soft hyphen. Escaping them is the point of the stored-XSS
// review (2026-11-18); its own tooling wrote some of them raw into docs and comments, so every tracked text file is
// checked.
func hidden(r rune) bool {
	switch {
	case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
		return true
	case r == 0x200b, r == 0x200e, r == 0x200f, r == 0x2060, r == 0xfeff, r == 0x061c, r == 0x00ad:
		return true
	}
	return false
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
			for _, r := range sc.Text() {
				if hidden(r) {
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
