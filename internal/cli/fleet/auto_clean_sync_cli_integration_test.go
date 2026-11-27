//go:build integration

package fleet

// Automatic clean syncs for rules hash outliers: the fleet job requests a clean sync for a Mac whose rules hash differs
// from its set's majority, once per rule change, before listing it (docs/fleet-health.md). They are on unless turned
// off with
//
//	rudolph fleet auto-clean-sync off
//	rudolph fleet auto-clean-sync on
//
// with the persistent --yes and --dry-run of `rudolph fleet`, following `fleet min-santa-version`:
//
//   - It prints whether they are on now, then what it will do, then asks "Apply changes?" unless --yes; --dry-run
//     writes and records nothing.
//   - It writes only through fleethealth.SetAutoCleanSync: off stores AutoCleanSync "off", on removes the attribute;
//     the stale threshold, any holiday and the minimum Santa version are left as they are.
//   - Each change is audited with Action "fleet.auto-clean-sync" and Target "fleet"; the Summary says automatic clean
//     syncs were turned off or on.
//   - Asking for the state already in force prints that nothing changed, exits 0, writes and records nothing.
//   - Refused before any prompt or write: no argument, more than one, or anything other than on or off (the error
//     names both).
//
// `rudolph fleet show` prints one more line: "Automatic clean syncs: on (default)" or "Automatic clean syncs: off
// (stored)".

import (
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	modelaudit "github.com/airbnb/rudolph/pkg/model/audit"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func autoCleanSyncEntries(t *testing.T) []modelaudit.Entry {
	t.Helper()
	all, err := modelaudit.List(ddblocal.Client(t), time.Now().Add(-time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	var got []modelaudit.Entry
	for _, e := range all {
		if e.Action == "fleet.auto-clean-sync" {
			got = append(got, e)
		}
	}
	return got
}

func storedAutoCleanSync(t *testing.T) (string, bool) {
	t.Helper()
	return str(holidayItem(t), "AutoCleanSync")
}

func TestFleetAutoCleanSync_OffAndOn(t *testing.T) {
	ddblocal.Reset(t)
	out, err := fleetCLI(t, "auto-clean-sync", "off")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	prompt := strings.Index(out, "Apply changes?")
	if prompt < 0 {
		t.Fatalf("no confirmation asked:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(out[:prompt]), "automatic clean sync") {
		t.Errorf("before the prompt the output does not say what changes:\n%s", out)
	}
	if got, ok := storedAutoCleanSync(t); !ok || got != "off" {
		t.Errorf("stored AutoCleanSync %q (present %v), want off", got, ok)
	}
	entries := autoCleanSyncEntries(t)
	if len(entries) != 1 {
		t.Fatalf("%d fleet.auto-clean-sync audit entries, want 1", len(entries))
	}
	if e := entries[0]; e.Target != "fleet" || !strings.HasPrefix(e.Actor, "cli:") || !strings.Contains(strings.ToLower(e.Summary), "automatic clean sync") || !strings.Contains(e.Summary, "off") {
		t.Errorf("audit entry %+v, want Target fleet, Actor cli:..., a Summary saying automatic clean syncs were turned off", e)
	}

	out, err = fleetCLI(t, "auto-clean-sync", "on")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, ok := storedAutoCleanSync(t); ok {
		t.Error("on left AutoCleanSync stored")
	}
	entries = autoCleanSyncEntries(t)
	if len(entries) != 2 {
		t.Fatalf("%d fleet.auto-clean-sync audit entries, want 2", len(entries))
	}
	if e := entries[0]; e.Target != "fleet" || !strings.Contains(strings.ToLower(e.Summary), "automatic clean sync") || !strings.Contains(e.Summary, " on") {
		t.Errorf("newest audit entry %+v, want a Summary saying automatic clean syncs were turned on", e)
	}
}

func TestFleetAutoCleanSync_KeepsTheRestOfTheSetting(t *testing.T) {
	ddblocal.Reset(t)
	until := day(30)
	putFleetHealth(t, map[string]awstypes.AttributeValue{"StaleDays": nAttr("14"), "HolidayStaleDays": nAttr("60"), "HolidayUntil": sAttr(until), "MinSantaVersion": sAttr("2026.10")})
	for _, arg := range []string{"off", "on"} {
		if out, err := fleetCLI(t, "auto-clean-sync", arg); err != nil {
			t.Fatalf("%s: %v\n%s", arg, err, out)
		}
		item := holidayItem(t)
		if got, _ := num(item, "StaleDays"); got != 14 {
			t.Errorf("after %s StaleDays %d, want 14 kept", arg, got)
		}
		if got, _ := num(item, "HolidayStaleDays"); got != 60 {
			t.Errorf("after %s HolidayStaleDays %d, want 60 kept", arg, got)
		}
		if got, _ := str(item, "HolidayUntil"); got != until {
			t.Errorf("after %s HolidayUntil %q, want %q kept", arg, got, until)
		}
		if got, _ := str(item, "MinSantaVersion"); got != "2026.10" {
			t.Errorf("after %s MinSantaVersion %q, want 2026.10 kept", arg, got)
		}
	}
	if n := len(holidayEntries(t)) + len(minVersionEntries(t)); n != 0 {
		t.Errorf("auto-clean-sync recorded %d holiday or minimum version entries", n)
	}
}

func TestFleetAutoCleanSync_AlreadyInForce(t *testing.T) {
	for name, c := range map[string]struct {
		stored awstypes.AttributeValue // nil: nothing stored
		arg    string
	}{
		"on, nothing stored": {nil, "on"},
		"off, stored off":    {sAttr("off"), "off"},
	} {
		t.Run(name, func(t *testing.T) {
			ddblocal.Reset(t)
			if c.stored != nil {
				putFleetHealth(t, map[string]awstypes.AttributeValue{"AutoCleanSync": c.stored})
			}
			before := holidayItem(t)
			out, err := fleetCLI(t, "auto-clean-sync", c.arg)
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if !strings.Contains(strings.ToLower(out), "nothing changed") {
				t.Errorf("the output does not say nothing changed:\n%s", out)
			}
			if strings.Contains(out, "Apply changes?") {
				t.Errorf("asked to confirm a change that changes nothing:\n%s", out)
			}
			if after := holidayItem(t); len(after) != len(before) {
				t.Errorf("the item changed: %v -> %v", before, after)
			}
			if n := len(autoCleanSyncEntries(t)); n != 0 {
				t.Errorf("recorded %d audit entries for no change", n)
			}
		})
	}
}

func TestFleetAutoCleanSync_RefusedArguments(t *testing.T) {
	for name, args := range map[string][]string{
		"nothing":    {"auto-clean-sync"},
		"two values": {"auto-clean-sync", "on", "off"},
		"another":    {"auto-clean-sync", "maybe"},
		"a number":   {"auto-clean-sync", "0"},
	} {
		t.Run(name, func(t *testing.T) {
			ddblocal.Reset(t)
			out, err := fleetCLI(t, args...)
			if err == nil {
				t.Fatalf("accepted:\n%s", out)
			}
			if strings.Contains(err.Error(), "unknown command") {
				t.Fatalf("refused as an unknown command, not by the command: %v", err)
			}
			for _, want := range []string{"on", "off"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
			if strings.Contains(out, "Apply changes?") {
				t.Errorf("asked to confirm before refusing:\n%s", out)
			}
			if item := holidayItem(t); len(item) != 0 {
				t.Errorf("a refused command wrote %v", item)
			}
			if n := len(autoCleanSyncEntries(t)); n != 0 {
				t.Errorf("a refused command was audited (%d entries)", n)
			}
		})
	}
}

func TestFleetAutoCleanSync_DryRunAndDeclined(t *testing.T) {
	for name, c := range map[string]struct {
		args  []string
		stdin string
		fails bool
	}{
		"dry run":   {[]string{"auto-clean-sync", "off", "--dry-run"}, "yes\n", false},
		"declined":  {[]string{"auto-clean-sync", "off"}, "no\n", true},
		"no answer": {[]string{"auto-clean-sync", "off"}, "", true},
	} {
		t.Run(name, func(t *testing.T) {
			ddblocal.Reset(t)
			out, err := runFleetCLI(t, c.stdin, c.args...)
			if strings.Contains(err2s(err), "unknown command") {
				t.Fatalf("refused as an unknown command: %v", err)
			}
			if c.fails && err == nil {
				t.Errorf("a declined change exited 0:\n%s", out)
			}
			if !c.fails && err != nil {
				t.Errorf("a dry run failed: %v\n%s", err, out)
			}
			if !c.fails && !strings.Contains(out, "Dry run") {
				t.Errorf("a dry run does not say nothing is written:\n%s", out)
			}
			if item := holidayItem(t); len(item) != 0 {
				t.Errorf("%s wrote %v", name, item)
			}
			if n := len(autoCleanSyncEntries(t)); n != 0 {
				t.Errorf("%s was audited (%d entries)", name, n)
			}
		})
	}
}

func TestFleetAutoCleanSync_Yes(t *testing.T) {
	ddblocal.Reset(t)
	out, err := runFleetCLI(t, "", "auto-clean-sync", "off", "--yes")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(out, "Apply changes?") {
		t.Errorf("asked to confirm with --yes:\n%s", out)
	}
	if got, _ := storedAutoCleanSync(t); got != "off" {
		t.Errorf("AutoCleanSync %q, want off", got)
	}
}

func TestFleetShow_AutoCleanSync(t *testing.T) {
	for name, c := range map[string]struct {
		stored awstypes.AttributeValue
		line   string
	}{
		"nothing stored": {nil, "Automatic clean syncs: on (default)"},
		"off":            {sAttr("off"), "Automatic clean syncs: off (stored)"},
		// The job reads anything but off as on, so the display must not claim otherwise.
		"an unusable value": {sAttr("garbage"), "Automatic clean syncs: on (default)"},
	} {
		t.Run(name, func(t *testing.T) {
			ddblocal.Reset(t)
			if c.stored != nil {
				putFleetHealth(t, map[string]awstypes.AttributeValue{"AutoCleanSync": c.stored})
			}
			out, err := runFleetCLI(t, "", "show")
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			found := false
			for _, line := range strings.Split(out, "\n") {
				found = found || strings.TrimSpace(line) == c.line
			}
			if !found {
				t.Errorf("no line %q in:\n%s", c.line, out)
			}
		})
	}
}
