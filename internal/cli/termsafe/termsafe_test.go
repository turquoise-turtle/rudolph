package termsafe

import (
	"errors"
	"fmt"
	"testing"
)

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
		{"wales \U0001F3F4\U000E0067\U000E0062\U000E0077\U000E006C\U000E0073\U000E007F!", "wales \U0001F3F4\U000E0067\U000E0062\U000E0077\U000E006C\U000E0073\U000E007F!"},
		{"black flag \U0001F3F4 alone", "black flag \U0001F3F4 alone"},
		{"\U0001F3F4\U000E0067\U000E0062", "\U0001F3F4" + `\U000e0067\U000e0062`},
		{"\U0001F3F4\U000E007F", "\U0001F3F4" + `\U000e007f`},
		{"stray\U000E0067\U000E007F", `stray\U000e0067\U000e007f`},
		{"\U0001F3F4\U000E0067\U000E007F", "\U0001F3F4" + `\U000e0067\U000e007f`},
		{"\U0001F3F4\U000E0075\U000E0073\U000E0063\U000E0061\U000E007F", "\U0001F3F4\U000E0075\U000E0073\U000E0063\U000E0061\U000E007F"},
		{"\U0001F3F4\U000E0067\U000E0062\U000E0065\U000E006E\U000E0067\U000E0078\U000E0079\U000E0079\U000E007F", "\U0001F3F4" + `\U000e0067\U000e0062\U000e0065\U000e006e\U000e0067\U000e0078\U000e0079\U000e0079\U000e007f`},
		{"\U0001F3F4\U000E0049\U000E0067\U000E007F", "\U0001F3F4" + `\U000e0049\U000e0067\U000e007f`},
		{"\U0001F3F4\U000E0020\U000E0067\U000E007F", "\U0001F3F4" + `\U000e0020\U000e0067\U000e007f`},
		{"\u3164\u115f\u1160\uffa0\u2800", `\u3164\u115f\u1160\uffa0\u2800`},
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

func TestError(t *testing.T) {
	base := errors.New("rule \x1b[2J")
	err := Error(fmt.Errorf("wrapped: %w", base))
	if err.Error() != `wrapped: rule \x1b[2J` || !errors.Is(err, base) {
		t.Errorf("Error = %q, Is base %t", err, errors.Is(err, base))
	}
	if Error(nil) != nil {
		t.Error("Error(nil) is not nil")
	}
}
