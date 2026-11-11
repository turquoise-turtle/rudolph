//go:build integration

package config

// Client mode per device group, the CLI side (the stored attribute and how preflight applies it are in
// internal/handlers/group_client_mode_integration_test.go):
//
//	rudolph config update --group <name> -c <monitor|lockdown|standalone> [--confirm-group-lockdown]
//	rudolph config update --ungrouped -c <mode> [--confirm-group-lockdown]
//	rudolph config update --group <name> --inherit client-mode      (removes the group's mode)
//	rudolph config get --group <name> | --ungrouped
//
// Only the client mode can be set for a group: any other setting flag, --clean-sync, a temporary mode (--for, --until,
// --end-temporary-mode) and `config set --group` are refused. LOCKDOWN and STANDALONE need --confirm-group-lockdown, as
// a group may hold many Macs. The group must be registered; --ungrouped registers the reserved group "rudolph:ungrouped"
// when it is not. The mode is written to the group's registry entry (Groups.<name>.ClientMode, a number), leaving the
// rest of the entry as it was, and MembershipVersion is bumped, in one conditional update (the group must still exist).
// Each change is audited as action "config.update" with target "group:<name>" (or "ungrouped") and a summary like
// "ClientMode=LOCKDOWN" or "ClientMode inherited". `config get --machine` marks a mode that comes from a group with
// "(group <name>)". `group list` shows each group's client mode.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/cli/group"
	"github.com/airbnb/rudolph/internal/testutil/clitest"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	modelaudit "github.com/airbnb/rudolph/pkg/model/audit"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func registryItem(t *testing.T) map[string]awstypes.AttributeValue {
	t.Helper()
	out, err := ddblocal.Raw(t).GetItem(context.Background(), &awsdynamodb.GetItemInput{
		TableName:      aws.String(ddblocal.TableName),
		Key:            map[string]awstypes.AttributeValue{"PK": &awstypes.AttributeValueMemberS{Value: "GroupRegistry"}, "SK": &awstypes.AttributeValueMemberS{Value: "Current"}},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.Item
}

func groupEntry(t *testing.T, name string) map[string]awstypes.AttributeValue {
	t.Helper()
	groups, ok := registryItem(t)["Groups"].(*awstypes.AttributeValueMemberM)
	if !ok {
		return nil
	}
	e, _ := groups.Value[name].(*awstypes.AttributeValueMemberM)
	if e == nil {
		return nil
	}
	return e.Value
}

func membershipVersion(t *testing.T) string {
	t.Helper()
	v, _ := registryItem(t)["MembershipVersion"].(*awstypes.AttributeValueMemberN)
	if v == nil {
		return ""
	}
	return v.Value
}

func groupCLI(t *testing.T, args ...string) string {
	t.Helper()
	out, err := clitest.Run(t, env, group.GroupCmd, "yes\n", args...)
	if err != nil {
		t.Fatalf("group %v: %v\n%s", args, err, out)
	}
	return out
}

func configCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return clitest.Run(t, env, ConfigCmd, "yes\n", args...)
}

func mustConfigCLI(t *testing.T, args ...string) string {
	t.Helper()
	out, err := configCLI(t, args...)
	if err != nil {
		t.Fatalf("config %v: %v\n%s", args, err, out)
	}
	return out
}

func groupModeEntries(t *testing.T) []modelaudit.Entry {
	t.Helper()
	all, err := modelaudit.List(ddblocal.Client(t), time.Now().Add(-time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	var got []modelaudit.Entry
	for _, e := range all {
		if e.Action == "config.update" {
			got = append(got, e)
		}
	}
	return got
}

func TestGroupModeCLI_SetAndClear(t *testing.T) {
	ddblocal.Reset(t)
	groupCLI(t, "create", "pilot", "--description", "Lockdown pilot")
	before := membershipVersion(t)

	mustConfigCLI(t, "update", "--group", "Pilot", "-c", "lockdown", "--confirm-group-lockdown")
	entry := groupEntry(t, "pilot")
	if mode, _ := entry["ClientMode"].(*awstypes.AttributeValueMemberN); mode == nil || mode.Value != "2" {
		t.Fatalf("Groups.pilot.ClientMode = %v, want 2 (LOCKDOWN)", entry["ClientMode"])
	}
	if desc, _ := entry["Description"].(*awstypes.AttributeValueMemberS); desc == nil || desc.Value != "Lockdown pilot" {
		t.Errorf("the rest of the entry changed: Description = %v", entry["Description"])
	}
	if _, ok := entry["RulesVersion"]; !ok {
		t.Error("the entry lost its RulesVersion")
	}
	if after := membershipVersion(t); after == before {
		t.Errorf("MembershipVersion stayed %s; setting a group's mode must bump it", after)
	}

	got := mustConfigCLI(t, "get", "--group", "pilot")
	if !strings.Contains(got, "LOCKDOWN") {
		t.Errorf("config get --group pilot does not show LOCKDOWN:\n%s", got)
	}
	if out := groupCLI(t, "list"); !strings.Contains(out, "LOCKDOWN") {
		t.Errorf("group list does not show pilot's mode:\n%s", out)
	}

	mustConfigCLI(t, "update", "--group", "pilot", "--inherit", "client-mode")
	if _, ok := groupEntry(t, "pilot")["ClientMode"]; ok {
		t.Error("--inherit client-mode left the group's ClientMode")
	}
	if groupEntry(t, "pilot") == nil {
		t.Fatal("clearing the mode removed the group")
	}

	entries := groupModeEntries(t)
	if len(entries) != 2 {
		t.Fatalf("%d config.update audit entries, want 2: %+v", len(entries), entries)
	}
	var summaries []string
	for _, e := range entries {
		if e.Target != "group:pilot" {
			t.Errorf("audit target %q, want group:pilot", e.Target)
		}
		summaries = append(summaries, e.Summary)
	}
	joined := strings.Join(summaries, " | ")
	if !strings.Contains(joined, "ClientMode=LOCKDOWN") || !strings.Contains(joined, "ClientMode inherited") {
		t.Errorf("audit summaries %q, want ClientMode=LOCKDOWN and ClientMode inherited", joined)
	}
}

func TestGroupModeCLI_LockdownAndStandaloneNeedConfirmation(t *testing.T) {
	ddblocal.Reset(t)
	groupCLI(t, "create", "pilot")
	for _, mode := range []string{"lockdown", "standalone"} {
		if out, err := configCLI(t, "update", "--group", "pilot", "-c", mode); err == nil {
			t.Errorf("%s without --confirm-group-lockdown was accepted:\n%s", mode, out)
		}
	}
	if _, ok := groupEntry(t, "pilot")["ClientMode"]; ok {
		t.Error("a refused update stored a mode")
	}
	mustConfigCLI(t, "update", "--group", "pilot", "-c", "monitor")
	if mode, _ := groupEntry(t, "pilot")["ClientMode"].(*awstypes.AttributeValueMemberN); mode == nil || mode.Value != "1" {
		t.Errorf("MONITOR needs no confirmation: ClientMode = %v, want 1", groupEntry(t, "pilot")["ClientMode"])
	}
}

func TestGroupModeCLI_RefusedArguments(t *testing.T) {
	ddblocal.Reset(t)
	groupCLI(t, "create", "pilot")
	for name, args := range map[string][]string{
		"unregistered group":     {"update", "--group", "nope", "-c", "monitor"},
		"another setting":        {"update", "--group", "pilot", "--batch-size", "10"},
		"mode and setting":       {"update", "--group", "pilot", "-c", "monitor", "--allowed-paths", "^/opt/"},
		"clean sync":             {"update", "--group", "pilot", "--clean-sync"},
		"temporary mode":         {"update", "--group", "pilot", "-c", "monitor", "--for", "2h"},
		"end temporary":          {"update", "--group", "pilot", "--end-temporary-mode"},
		"inherit another":        {"update", "--group", "pilot", "--inherit", "batch-size"},
		"group and global":       {"update", "--group", "pilot", "--global", "-c", "monitor"},
		"group and machine":      {"update", "--group", "pilot", "-m", machine, "-c", "monitor"},
		"group and ungrouped":    {"update", "--group", "pilot", "--ungrouped", "-c", "monitor"},
		"set replaces a config":  {"set", "--group", "pilot", "-c", "monitor"},
		"nothing to change":      {"update", "--group", "pilot"},
		"get of unknown group":   {"get", "--group", "nope"},
		"reserved name as group": {"update", "--group", "rudolph:ungrouped", "-c", "monitor"},
	} {
		if out, err := configCLI(t, args...); err == nil {
			t.Errorf("%s: %v was accepted:\n%s", name, args, out)
		}
	}
	if _, ok := groupEntry(t, "pilot")["ClientMode"]; ok {
		t.Error("a refused update stored a mode")
	}
	if n := len(groupModeEntries(t)); n != 0 {
		t.Errorf("refused updates wrote %d audit entries", n)
	}
}

func TestGroupModeCLI_Ungrouped(t *testing.T) {
	ddblocal.Reset(t)
	mustConfigCLI(t, "update", "--ungrouped", "-c", "monitor")
	if mode, _ := groupEntry(t, "rudolph:ungrouped")["ClientMode"].(*awstypes.AttributeValueMemberN); mode == nil || mode.Value != "1" {
		t.Fatalf("rudolph:ungrouped ClientMode = %v, want 1 (registered on demand)", groupEntry(t, "rudolph:ungrouped")["ClientMode"])
	}
	if got := mustConfigCLI(t, "get", "--ungrouped"); !strings.Contains(got, "MONITOR") {
		t.Errorf("config get --ungrouped:\n%s", got)
	}
	entries := groupModeEntries(t)
	if len(entries) != 1 || entries[0].Target != "ungrouped" {
		t.Errorf("audit entries %+v, want one with target ungrouped", entries)
	}
}

func TestGroupModeCLI_MachineConfigShowsTheGroupSource(t *testing.T) {
	ddblocal.Reset(t)
	mustConfigCLI(t, "set", "--global", "-c", "monitor")
	groupCLI(t, "create", "pilot")
	groupCLI(t, "add", "--machine", machine, "pilot")
	mustConfigCLI(t, "update", "--group", "pilot", "-c", "lockdown", "--confirm-group-lockdown")

	out := mustConfigCLI(t, "get", "-m", machine)
	var line string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "ClientMode") {
			line = l
		}
	}
	if !strings.Contains(line, "LOCKDOWN") || !strings.Contains(line, "(group pilot)") {
		t.Errorf("config get -m: ClientMode line %q, want LOCKDOWN marked (group pilot)\n%s", line, out)
	}

	// The Mac's own mode wins and is marked as the machine's
	mustConfigCLI(t, "update", "-m", machine, "-c", "monitor")
	out = mustConfigCLI(t, "get", "-m", machine)
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "ClientMode") {
			line = l
		}
	}
	if !strings.Contains(line, "MONITOR") || strings.Contains(line, "(group") {
		t.Errorf("config get -m with the Mac's own MONITOR: ClientMode line %q\n%s", line, out)
	}
}
