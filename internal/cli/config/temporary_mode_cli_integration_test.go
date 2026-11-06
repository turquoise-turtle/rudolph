//go:build integration

package config

// Temporary client modes (internal/handlers/temporary_mode_integration_test.go explains the stored attributes):
//
//	rudolph config update --machine <id> -c <monitor|lockdown|standalone> --for <duration>   (e.g. 2h, 90m)
//	rudolph config update --machine <id> -c <mode> --until <RFC 3339 time>
//	rudolph config update --machine <id> --end-temporary-mode
//
// --for or --until is required for a temporary mode and at most 30 days ahead; neither is allowed for the global
// configuration. A temporary mode leaves the machine's own ClientMode as it is. A temporary lockdown or standalone
// asks for the same confirmation as setting that mode on a machine. config get shows the temporary mode and its end.

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/clitest"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func numberAttr(t *testing.T, row map[string]awstypes.AttributeValue, attr string) (int64, bool) {
	t.Helper()
	v, ok := row[attr].(*awstypes.AttributeValueMemberN)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(v.Value, 10, 64)
	if err != nil {
		t.Fatalf("%s = %q", attr, v.Value)
	}
	return n, true
}

func TestTemporaryMode_ForADuration(t *testing.T) {
	ddblocal.Reset(t)
	if out, err := clitest.Run(t, env, ConfigCmd, "yes\n", "set", "--machine", machine, "-c", "lockdown", "--batch-size", "50"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	before := time.Now()
	if out, err := clitest.Run(t, env, ConfigCmd, "", "update", "--machine", machine, "-c", "monitor", "--for", "2h"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	row := configRow(t, "Machine#"+machine)
	if mode, ok := numberAttr(t, row, "TemporaryClientMode"); !ok || mode != 1 {
		t.Errorf("TemporaryClientMode = %d (present %v), want 1 (MONITOR)", mode, ok)
	}
	expires, ok := numberAttr(t, row, "TemporaryClientModeExpiresAt")
	want := before.Add(2 * time.Hour).Unix()
	if !ok || expires < want-5 || expires > want+60 {
		t.Errorf("TemporaryClientModeExpiresAt = %d (present %v), want about %d", expires, ok, want)
	}
	if mode, _ := numberAttr(t, row, "ClientMode"); mode != 2 {
		t.Errorf("the machine's own ClientMode changed to %d; a temporary mode must leave it at 2 (LOCKDOWN)", mode)
	}

	out, err := clitest.Run(t, env, ConfigCmd, "", "get", "--machine", machine)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(strings.ToLower(out), "temporary") || !strings.Contains(out, "MONITOR") {
		t.Errorf("config get does not show the temporary mode:\n%s", out)
	}

	if out, err := clitest.Run(t, env, ConfigCmd, "", "update", "--machine", machine, "--end-temporary-mode"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	row = configRow(t, "Machine#"+machine)
	for _, attr := range []string{"TemporaryClientMode", "TemporaryClientModeExpiresAt"} {
		if _, ok := row[attr]; ok {
			t.Errorf("--end-temporary-mode left %s", attr)
		}
	}
	if mode, _ := numberAttr(t, row, "ClientMode"); mode != 2 {
		t.Errorf("--end-temporary-mode changed ClientMode to %d", mode)
	}
}

// A temporary mode on a Mac without a configuration of its own creates a row holding only the temporary mode, so every
// other setting keeps being inherited.
func TestTemporaryMode_UntilATimeOnAnUnconfiguredMac(t *testing.T) {
	ddblocal.Reset(t)
	until := time.Now().Add(3 * time.Hour).UTC().Truncate(time.Second)
	if out, err := clitest.Run(t, env, ConfigCmd, "yes\n", "update", "--machine", machine, "-c", "lockdown", "--until", until.Format(time.RFC3339)); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	row := configRow(t, "Machine#"+machine)
	if mode, _ := numberAttr(t, row, "TemporaryClientMode"); mode != 2 {
		t.Errorf("TemporaryClientMode = %d, want 2 (LOCKDOWN)", mode)
	}
	if expires, _ := numberAttr(t, row, "TemporaryClientModeExpiresAt"); expires != until.Unix() {
		t.Errorf("TemporaryClientModeExpiresAt = %d, want %d", expires, until.Unix())
	}
	if _, ok := row["ClientMode"]; ok {
		t.Errorf("a temporary mode on an unconfigured Mac also stored ClientMode %v: it must keep inheriting", row["ClientMode"])
	}
}

func TestTemporaryMode_RefusedArguments(t *testing.T) {
	for name, args := range map[string][]string{
		"for without a mode":       {"update", "--machine", machine, "--for", "2h"},
		"for on the global config": {"update", "--global", "-c", "monitor", "--for", "2h"},
		"for and until together":   {"update", "--machine", machine, "-c", "monitor", "--for", "2h", "--until", time.Now().Add(time.Hour).Format(time.RFC3339)},
		"zero duration":            {"update", "--machine", machine, "-c", "monitor", "--for", "0s"},
		"negative duration":        {"update", "--machine", machine, "-c", "monitor", "--for", "-1h"},
		"more than 30 days":        {"update", "--machine", machine, "-c", "monitor", "--for", "721h"},
		"under a minute":           {"update", "--machine", machine, "-c", "monitor", "--for", "500ms"},
		"until in the past":        {"update", "--machine", machine, "-c", "monitor", "--until", time.Now().Add(-time.Hour).Format(time.RFC3339)},
		"until not a time":         {"update", "--machine", machine, "-c", "monitor", "--until", "tomorrow"},
	} {
		t.Run(name, func(t *testing.T) {
			ddblocal.Reset(t)
			if out, err := clitest.Run(t, env, ConfigCmd, "yes\n", args...); err == nil {
				t.Fatalf("accepted:\n%s", out)
			}
			if _, ok := configRow(t, "Machine#"+machine)["TemporaryClientMode"]; ok {
				t.Error("a refused temporary mode was stored")
			}
			if _, ok := configRow(t, "GlobalConfig")["TemporaryClientMode"]; ok {
				t.Error("a temporary mode was stored on the global config")
			}
		})
	}
}

// A temporary lockdown blocks unknown executions on that Mac: it asks for confirmation like `-c lockdown` does.
func TestTemporaryMode_LockdownNeedsConfirmation(t *testing.T) {
	ddblocal.Reset(t)
	if out, err := clitest.Run(t, env, ConfigCmd, "no\n", "update", "--machine", machine, "-c", "lockdown", "--for", "1h"); err == nil {
		t.Fatalf("a declined temporary lockdown succeeded:\n%s", out)
	}
	if _, ok := configRow(t, "Machine#"+machine)["TemporaryClientMode"]; ok {
		t.Error("a declined temporary lockdown was stored")
	}
}

// From the review: while a temporary mode runs, changing the machine's own mode does not take effect until it ends.
// config update says so instead of looking as if the new mode applies at the next sync.
func TestTemporaryMode_UpdateOfTheOwnModeWarnsWhileOneRuns(t *testing.T) {
	ddblocal.Reset(t)
	if out, err := clitest.Run(t, env, ConfigCmd, "", "update", "--machine", machine, "-c", "monitor", "--for", "2h"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, args := range [][]string{
		{"update", "--machine", machine, "-c", "lockdown"},
		{"update", "--machine", machine, "--inherit", "client-mode"},
	} {
		out, err := clitest.Run(t, env, ConfigCmd, "yes\n", args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		if !strings.Contains(strings.ToLower(out), "temporary") {
			t.Errorf("%v gave no warning about the running temporary mode:\n%s", args, out)
		}
	}
}
