package config

import (
	"regexp"
	"strings"
	"testing"
)

// santaEffectiveRegex mirrors SNTConfigurator expressionForPattern in Santa 2026.8: patterns not starting with
// "^" get one prepended, and the regex is then searched for anywhere in the path.
func santaEffectiveRegex(t *testing.T, pattern string) *regexp.Regexp {
	t.Helper()
	if !strings.HasPrefix(pattern, "^") {
		pattern = "^" + pattern
	}
	return regexp.MustCompile(pattern)
}

// Review finding H1: Santa anchors the combined pattern once, so every alternative must stay anchored.
func TestReview_CombinedPathRegexesStayAnchoredInSanta(t *testing.T) {
	for _, patterns := range [][]string{
		{"/tmp/", "/private/"},
		{"^/tmp/", "/private/"},
		{"/tmp/", "^/private/"},
	} {
		combined, err := combinePathRegexes(patterns)
		if err != nil {
			t.Fatal(err)
		}
		re := santaEffectiveRegex(t, combined)
		for path, want := range map[string]bool{
			"/tmp/x":                  true,
			"/private/var/x":          true,
			"/Users/bob/private/evil": false,
			"/Users/bob/tmp/x":        false,
		} {
			if got := re.MatchString(path); got != want {
				t.Errorf("patterns %q combined to %q: Santa would match %q = %v, want %v", patterns, combined, path, got, want)
			}
		}
	}
}

// Review finding M4: `config update` applies a client mode only when -c is given, so its help must not show a default.
func TestReview_UpdateClientModeFlagHasNoDefault(t *testing.T) {
	cmd, _, err := ConfigCmd.Find([]string{"update"})
	if err != nil {
		t.Fatal(err)
	}
	flag := cmd.Flags().Lookup("client-mode")
	if flag == nil {
		t.Fatal("config update has no --client-mode flag")
	}
	if flag.DefValue != "" {
		t.Errorf("config update --client-mode shows default %q, but no client mode is applied unless the flag is given", flag.DefValue)
	}
}

// Review finding M2: Santa uses ICU regexes; Go's RE2 validation must not make valid ICU patterns impossible to set.
func TestReview_RegexValidationCanBeSkippedForICUOnlySyntax(t *testing.T) {
	for _, name := range []string{"set", "update"} {
		cmd, _, err := ConfigCmd.Find([]string{name})
		if err != nil {
			t.Fatal(err)
		}
		if cmd.Flags().Lookup("skip-regex-validation") == nil {
			t.Errorf("config %s has no --skip-regex-validation flag", name)
		}
	}
	combined := combinePathRegexesUnchecked([]string{`^/Users/(?!admin/)[^/]+/Downloads/`, `/tmp/`})
	if !strings.Contains(combined, `(?!admin/)`) || !strings.Contains(combined, `/tmp/`) {
		t.Fatalf("unchecked combination lost a pattern: %q", combined)
	}
	if !strings.HasPrefix(combined, "^") {
		t.Fatalf("unchecked combination must be anchored for Santa: %q", combined)
	}
	if got := combinePathRegexesUnchecked(nil); got != "" {
		t.Fatalf("no patterns must clear the setting, got %q", got)
	}
}
