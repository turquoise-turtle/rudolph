package termsafe

import "testing"

func TestString(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", ""},
		{"plain-host.local", "plain-host.local"},
		{"Jürgen’s Mac 👨\u200d👩\u200d👧 工程 م\u200cن", "Jürgen’s Mac 👨\u200d👩\u200d👧 工程 م\u200cن"},
		{`C:\path\x1b`, `C:\path\x1b`},
		{"evil\x1b]0;pwned\x07", `evil\x1b]0;pwned\x07`},
		{"a\nb\rc\td", `a\nb\rc\td`},
		{"\x00\x7f", `\x00\x7f`},
		{"C02\u009b31m", `C02\u009b31m`},
		{"\u202etxt\u2066\u2069\u200e\u200f\u061c", `\u202etxt\u2066\u2069\u200e\u200f\u061c`},
		{"z\u200bw\u2060j\ufeff\u00ad", `z\u200bw\u2060j\ufeff\u00ad`},
		{"line\u2028para\u2029", `line\u2028para\u2029`},
		{"tag\U000E0041", `tag\U000e0041`},
		{"bad\xffbyte", `bad\xffbyte`},
	} {
		if got := String(c.in); got != c.want {
			t.Errorf("String(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestStrings(t *testing.T) {
	in := []string{"eng", "o\x1bps"}
	got := Strings(in)
	if len(got) != 2 || got[0] != "eng" || got[1] != `o\x1bps` || in[1] != "o\x1bps" {
		t.Errorf("Strings(%q) = %q", in, got)
	}
}
