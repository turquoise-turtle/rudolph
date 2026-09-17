package config

import (
	"regexp"
	"testing"
)

// Shortcoming L2 (upstream #51): multiple path regexes given to the CLI must become one regex that Santa
// can apply, matching any of them. Each --allowed-paths/--blocked-paths flag value is one regex, so regexes
// containing commas (e.g. quantifiers) survive intact.
func TestCombinePathRegexes_L2(t *testing.T) {
	combined, err := combinePathRegexes([]string{
		`^/Applications/Foo\.app/`,
		`^/opt/tools/v[0-9]{1,3}/bin/`,
	})
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(combined)

	for path, want := range map[string]bool{
		"/Applications/Foo.app/Contents/MacOS/Foo": true,
		"/opt/tools/v12/bin/run":                   true,
		"/opt/tools/v1234/bin/run":                 false,
		"/Applications/Bar.app/Contents/MacOS/Bar": false,
		"/tmp/Applications/Foo.app/x":              false,
	} {
		if got := re.MatchString(path); got != want {
			t.Errorf("combined regex %q: match(%q) = %v, want %v", combined, path, got, want)
		}
	}
}

func TestCombinePathRegexes_EmptyAndInvalid_L2(t *testing.T) {
	if got, err := combinePathRegexes(nil); err != nil || got != "" {
		t.Errorf("no regexes should clear the setting, got %q, %v", got, err)
	}
	if _, err := combinePathRegexes([]string{`^/ok/`, `^/broken/(`}); err == nil {
		t.Error("an invalid regex must be rejected before it is sent to every Mac")
	}
}
