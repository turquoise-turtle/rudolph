//go:build integration

package fleet

// Editable minimum Santa version (owner decision, 2026-11-25). The fleet job lists a Mac under Version when its Santa
// release is below the minimum, which is FleetHealth / Config's MinSantaVersion, else the default 2026.9
// (fleethealth.DefaultMinSantaVersion). It is changed with
//
//	rudolph fleet min-santa-version <version>     a release such as 2026.10 or 2026.10.1
//	rudolph fleet min-santa-version off           turns the Version check off
//	rudolph fleet min-santa-version --default     clears the stored value, so the default applies
//
// with the persistent --yes and --dry-run of `rudolph fleet`, following `fleet holiday`:
//
//   - It first prints the value in force and where it comes from: "the default 2026.9", a stored release, or off.
//     Then what it will store, then asks "Apply changes?" unless --yes; --dry-run writes and records nothing.
//   - It writes only through fleethealth.SetMinSantaVersion / ClearMinSantaVersion, so the stale threshold and any
//     holiday are left as they are, and the stored value is normalised ("OFF" is stored as "off", spaces trimmed).
//   - Each change is audited with Action "fleet.min-santa-version" and Target "fleet" (the holiday command's target),
//     with these summaries, <was> being "the default 2026.9", "2026.10" (a stored release) or "off":
//     set:      "set the minimum Santa version to <version> (was <was>)"
//     off:      "turned the minimum Santa version check off (was <was>)"
//     default:  "cleared the stored minimum Santa version (was <was>); the default 2026.9 applies"
//     where an unusable stored value being cleared is "an unusable value".
//   - --default with nothing stored prints that nothing changed, exits 0 and records nothing.
//   - Refused before any prompt or write, with nothing stored or audited: a value that is neither a release nor off
//     (the error names "off" and an example release), a version or off together with --default, neither, or more than
//     one argument.
//
// `rudolph fleet show` (read-only, no arguments) prints the fleet health setting in force, one line each:
//
//	Stale threshold: <days> days                 with ", holiday: <days> days until <YYYY-MM-DD>" while one is on
//	Minimum Santa version: <version> (<source>)  source "default" or "stored"; "off (stored)" when off

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/clitest"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	modelaudit "github.com/airbnb/rudolph/pkg/model/audit"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func runFleetCLI(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	return clitest.Run(t, env, FleetCmd, stdin, args...)
}

func putRawItem(t *testing.T, item map[string]awstypes.AttributeValue) {
	t.Helper()
	if _, err := ddblocal.Raw(t).PutItem(context.Background(), &awsdynamodb.PutItemInput{TableName: aws.String(ddblocal.TableName), Item: item}); err != nil {
		t.Fatal(err)
	}
}

func minVersionEntries(t *testing.T) []modelaudit.Entry {
	t.Helper()
	all, err := modelaudit.List(ddblocal.Client(t), time.Now().Add(-time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	var got []modelaudit.Entry
	for _, e := range all {
		if e.Action == "fleet.min-santa-version" {
			got = append(got, e)
		}
	}
	return got
}

// putFleetHealth stores the item with the given string and number attributes, replacing any earlier one.
func putFleetHealth(t *testing.T, attrs map[string]awstypes.AttributeValue) {
	t.Helper()
	item := map[string]awstypes.AttributeValue{
		"PK": &awstypes.AttributeValueMemberS{Value: "FleetHealth"},
		"SK": &awstypes.AttributeValueMemberS{Value: "Config"},
	}
	for k, v := range attrs {
		item[k] = v
	}
	putRawItem(t, item)
}

func sAttr(v string) awstypes.AttributeValue { return &awstypes.AttributeValueMemberS{Value: v} }
func nAttr(v string) awstypes.AttributeValue { return &awstypes.AttributeValueMemberN{Value: v} }

func storedMin(t *testing.T) (string, bool) {
	t.Helper()
	return str(holidayItem(t), "MinSantaVersion")
}

func TestFleetMinSantaVersion_Set(t *testing.T) {
	for name, c := range map[string]struct {
		before      awstypes.AttributeValue // nil: none stored
		arg         string
		stored      string
		summary     string
		currentText []string // what the output says is in force before the change
	}{
		"from the default": {nil, "2026.10", "2026.10",
			"set the minimum Santa version to 2026.10 (was the default 2026.9)", []string{"2026.9", "default"}},
		"from a stored release": {sAttr("2026.10"), "2026.11", "2026.11",
			"set the minimum Santa version to 2026.11 (was 2026.10)", []string{"2026.10"}},
		"from off": {sAttr("off"), "2026.10", "2026.10",
			"set the minimum Santa version to 2026.10 (was off)", []string{"off"}},
		"below the default": {nil, "2026.8", "2026.8",
			"set the minimum Santa version to 2026.8 (was the default 2026.9)", []string{"2026.9", "default"}},
		"a patch release": {nil, "2026.10.1", "2026.10.1",
			"set the minimum Santa version to 2026.10.1 (was the default 2026.9)", []string{"2026.9", "default"}},
		"trimmed": {nil, " 2026.10 ", "2026.10",
			"set the minimum Santa version to 2026.10 (was the default 2026.9)", []string{"2026.9", "default"}},
		"off": {nil, "off", "off",
			"turned the minimum Santa version check off (was the default 2026.9)", []string{"2026.9", "default"}},
		"OFF": {sAttr("2026.10"), "OFF", "off",
			"turned the minimum Santa version check off (was 2026.10)", []string{"2026.10"}},
	} {
		t.Run(name, func(t *testing.T) {
			ddblocal.Reset(t)
			if c.before != nil {
				putFleetHealth(t, map[string]awstypes.AttributeValue{"MinSantaVersion": c.before})
			}
			out, err := fleetCLI(t, "min-santa-version", c.arg)
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			// What is in force is printed before the question, then the new value.
			prompt := strings.Index(out, "Apply changes?")
			if prompt < 0 {
				t.Fatalf("no confirmation asked:\n%s", out)
			}
			for _, want := range c.currentText {
				if !strings.Contains(out[:prompt], want) {
					t.Errorf("before the prompt the output does not say %q is in force:\n%s", want, out)
				}
			}
			if !strings.Contains(out[:prompt], c.stored) {
				t.Errorf("before the prompt the output does not name the new value %q:\n%s", c.stored, out)
			}
			if got, ok := storedMin(t); !ok || got != c.stored {
				t.Errorf("stored MinSantaVersion %q (present %v), want %q", got, ok, c.stored)
			}
			entries := minVersionEntries(t)
			if len(entries) != 1 {
				t.Fatalf("%d fleet.min-santa-version audit entries, want 1", len(entries))
			}
			if e := entries[0]; e.Target != "fleet" || e.Summary != c.summary || !strings.HasPrefix(e.Actor, "cli:") {
				t.Errorf("audit entry %+v, want Target fleet, Actor cli:..., Summary %q", e, c.summary)
			}
		})
	}
}

// Setting the minimum leaves the stale threshold and the holiday as they are.
func TestFleetMinSantaVersion_KeepsTheStaleThresholdAndHoliday(t *testing.T) {
	ddblocal.Reset(t)
	until := day(30)
	putFleetHealth(t, map[string]awstypes.AttributeValue{"StaleDays": nAttr("14"), "HolidayStaleDays": nAttr("60"), "HolidayUntil": sAttr(until)})
	for _, args := range [][]string{{"min-santa-version", "2026.10"}, {"min-santa-version", "off"}, {"min-santa-version", "--default"}} {
		if out, err := fleetCLI(t, args...); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		item := holidayItem(t)
		if got, _ := num(item, "StaleDays"); got != 14 {
			t.Errorf("after %v StaleDays %d, want 14 kept", args, got)
		}
		if got, _ := num(item, "HolidayStaleDays"); got != 60 {
			t.Errorf("after %v HolidayStaleDays %d, want 60 kept", args, got)
		}
		if got, _ := str(item, "HolidayUntil"); got != until {
			t.Errorf("after %v HolidayUntil %q, want %q kept", args, got, until)
		}
	}
	if n := len(holidayEntries(t)); n != 0 {
		t.Errorf("changing the minimum Santa version recorded %d fleet.holiday entries, want 0", n)
	}
}

func TestFleetMinSantaVersion_Default(t *testing.T) {
	for name, c := range map[string]struct {
		before  awstypes.AttributeValue
		summary string
		was     string // in the output before the prompt
	}{
		"a stored release": {sAttr("2026.10"), "cleared the stored minimum Santa version (was 2026.10); the default 2026.9 applies", "2026.10"},
		"off":              {sAttr("off"), "cleared the stored minimum Santa version (was off); the default 2026.9 applies", "off"},
		// Edited by hand: the job ignores it, and --default is how an operator removes it.
		"an unusable string": {sAttr("garbage"), "cleared the stored minimum Santa version (was an unusable value); the default 2026.9 applies", ""},
		"an unusable number": {nAttr("2026"), "cleared the stored minimum Santa version (was an unusable value); the default 2026.9 applies", ""},
	} {
		t.Run(name, func(t *testing.T) {
			ddblocal.Reset(t)
			putFleetHealth(t, map[string]awstypes.AttributeValue{"MinSantaVersion": c.before})
			out, err := fleetCLI(t, "min-santa-version", "--default")
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			if c.was != "" {
				if prompt := strings.Index(out, "Apply changes?"); prompt < 0 || !strings.Contains(out[:prompt], c.was) {
					t.Errorf("the output does not say %q is in force before asking:\n%s", c.was, out)
				}
			}
			if !strings.Contains(out, "2026.9") {
				t.Errorf("the output does not say the default 2026.9 applies:\n%s", out)
			}
			if _, ok := holidayItem(t)["MinSantaVersion"]; ok {
				t.Error("--default left MinSantaVersion")
			}
			entries := minVersionEntries(t)
			if len(entries) != 1 {
				t.Fatalf("%d fleet.min-santa-version audit entries, want 1", len(entries))
			}
			if e := entries[0]; e.Target != "fleet" || e.Summary != c.summary {
				t.Errorf("audit entry %+v, want Target fleet and Summary %q", e, c.summary)
			}
		})
	}
}

func TestFleetMinSantaVersion_DefaultWithNothingStored(t *testing.T) {
	for name, attrs := range map[string]map[string]awstypes.AttributeValue{
		"no item":                   nil,
		"an item without the value": {"StaleDays": nAttr("14")},
	} {
		t.Run(name, func(t *testing.T) {
			ddblocal.Reset(t)
			if attrs != nil {
				putFleetHealth(t, attrs)
			}
			out, err := fleetCLI(t, "min-santa-version", "--default")
			if err != nil {
				t.Fatalf("--default with nothing stored failed: %v\n%s", err, out)
			}
			if !strings.Contains(strings.ToLower(out), "nothing changed") {
				t.Errorf("the output does not say nothing changed:\n%s", out)
			}
			if n := len(minVersionEntries(t)); n != 0 {
				t.Errorf("--default with nothing stored was audited (%d entries)", n)
			}
			item := holidayItem(t)
			if attrs == nil && len(item) != 0 {
				t.Errorf("--default with nothing stored created the item: %v", item)
			}
			if _, ok := item["MinSantaVersion"]; ok {
				t.Error("--default with nothing stored wrote MinSantaVersion")
			}
		})
	}
}

func TestFleetMinSantaVersion_RefusedArguments(t *testing.T) {
	long := "2026.10." + strings.Repeat("0", 24) + "1" // 33 bytes
	for name, c := range map[string]struct {
		args    []string
		mention []string // what the error must name
	}{
		"not a release":         {[]string{"min-santa-version", "garbage"}, []string{"off", "2026."}},
		"a year alone":          {[]string{"min-santa-version", "2026"}, []string{"off", "2026."}},
		"a v prefix":            {[]string{"min-santa-version", "v2026.10"}, []string{"off", "2026."}},
		"empty":                 {[]string{"min-santa-version", ""}, []string{"off", "2026."}},
		"too long":              {[]string{"min-santa-version", long}, []string{"off", "2026."}},
		"latest":                {[]string{"min-santa-version", "latest"}, []string{"off", "2026."}},
		"a version and default": {[]string{"min-santa-version", "2026.10", "--default"}, []string{"--default"}},
		"off and default":       {[]string{"min-santa-version", "off", "--default"}, []string{"--default"}},
		"nothing":               {[]string{"min-santa-version"}, []string{"--default"}},
		"two versions":          {[]string{"min-santa-version", "2026.10", "2026.11"}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			ddblocal.Reset(t)
			putFleetHealth(t, map[string]awstypes.AttributeValue{"MinSantaVersion": sAttr("2026.10")})
			out, err := fleetCLI(t, c.args...)
			if err == nil {
				t.Fatalf("accepted:\n%s", out)
			}
			if strings.Contains(err.Error(), "unknown") {
				t.Fatalf("refused as unknown, not by the command: %v", err)
			}
			for _, want := range c.mention {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
			if strings.Contains(out, "Apply changes?") {
				t.Errorf("asked to confirm before refusing:\n%s", out)
			}
			if got, _ := storedMin(t); got != "2026.10" {
				t.Errorf("a refused command changed MinSantaVersion to %q", got)
			}
			if n := len(minVersionEntries(t)); n != 0 {
				t.Errorf("a refused command was audited (%d entries)", n)
			}
		})
	}
}

func TestFleetMinSantaVersion_DryRunAndDeclined(t *testing.T) {
	for name, c := range map[string]struct {
		args  []string
		stdin string
		fails bool
	}{
		"dry run set":      {[]string{"min-santa-version", "2026.11", "--dry-run"}, "yes\n", false},
		"dry run off":      {[]string{"min-santa-version", "off", "--dry-run"}, "yes\n", false},
		"dry run default":  {[]string{"min-santa-version", "--default", "--dry-run"}, "yes\n", false},
		"declined set":     {[]string{"min-santa-version", "2026.11"}, "no\n", true},
		"declined default": {[]string{"min-santa-version", "--default"}, "no\n", true},
		"no answer to set": {[]string{"min-santa-version", "2026.11"}, "", true},
	} {
		t.Run(name, func(t *testing.T) {
			ddblocal.Reset(t)
			putFleetHealth(t, map[string]awstypes.AttributeValue{"MinSantaVersion": sAttr("2026.10")})
			out, err := runFleetCLI(t, c.stdin, c.args...)
			if c.fails && err == nil {
				t.Errorf("a declined change exited 0:\n%s", out)
			}
			if !c.fails && err != nil {
				t.Errorf("a dry run failed: %v\n%s", err, out)
			}
			if strings.Contains(err2s(err), "unknown") {
				t.Fatalf("refused as unknown: %v", err)
			}
			if !c.fails && !strings.Contains(out, "Dry run") {
				t.Errorf("a dry run does not say nothing is written:\n%s", out)
			}
			if got, ok := storedMin(t); !ok || got != "2026.10" {
				t.Errorf("MinSantaVersion %q (present %v) after %s, want 2026.10 unchanged", got, ok, name)
			}
			if n := len(minVersionEntries(t)); n != 0 {
				t.Errorf("%s was audited (%d entries)", name, n)
			}
		})
	}
}

// --yes applies without asking.
func TestFleetMinSantaVersion_Yes(t *testing.T) {
	ddblocal.Reset(t)
	out, err := runFleetCLI(t, "", "min-santa-version", "2026.10", "--yes")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(out, "Apply changes?") {
		t.Errorf("asked to confirm with --yes:\n%s", out)
	}
	if got, _ := storedMin(t); got != "2026.10" {
		t.Errorf("MinSantaVersion %q, want 2026.10", got)
	}
}

func TestFleetShow(t *testing.T) {
	until := day(30)
	for name, c := range map[string]struct {
		attrs map[string]awstypes.AttributeValue
		lines []string
	}{
		"nothing stored": {nil, []string{
			"Stale threshold: 10 days",
			"Minimum Santa version: 2026.9 (default)",
		}},
		"a stored release and a holiday": {map[string]awstypes.AttributeValue{
			"StaleDays": nAttr("14"), "HolidayStaleDays": nAttr("60"), "HolidayUntil": sAttr(until), "MinSantaVersion": sAttr("2026.10"),
		}, []string{
			"Stale threshold: 14 days, holiday: 60 days until " + until,
			"Minimum Santa version: 2026.10 (stored)",
		}},
		"off": {map[string]awstypes.AttributeValue{"MinSantaVersion": sAttr("off")}, []string{
			"Stale threshold: 10 days",
			"Minimum Santa version: off (stored)",
		}},
		// The job ignores an unusable value, so the display must not claim it is in force.
		"an unusable value": {map[string]awstypes.AttributeValue{"MinSantaVersion": sAttr("garbage")}, []string{
			"Minimum Santa version: 2026.9 (default)",
		}},
	} {
		t.Run(name, func(t *testing.T) {
			ddblocal.Reset(t)
			if c.attrs != nil {
				putFleetHealth(t, c.attrs)
			}
			before := holidayItem(t)
			out, err := runFleetCLI(t, "", "show")
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			got := map[string]bool{}
			for _, line := range strings.Split(out, "\n") {
				got[strings.TrimSpace(line)] = true
			}
			for _, want := range c.lines {
				if !got[want] {
					t.Errorf("no line %q in:\n%s", want, out)
				}
			}
			if after := holidayItem(t); len(after) != len(before) {
				t.Errorf("show changed the item: %v -> %v", before, after)
			}
			if n := len(minVersionEntries(t)) + len(holidayEntries(t)); n != 0 {
				t.Errorf("show recorded %d audit entries", n)
			}
		})
	}
}

func err2s(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
