//go:build integration

package config

// File access override per device group, the CLI side (the stored attribute and how preflight applies it are in
// internal/handlers/group_file_access_override_integration_test.go):
//
//	rudolph config update --group <name> --override-file-access-action <none|audit_only|disable>
//	rudolph config update --ungrouped --override-file-access-action <none|audit_only|disable>
//	rudolph config update --group <name> --inherit override-file-access-action      (removes the group's override)
//	rudolph config get --group <name> | --ungrouped
//
// A group now has two settings, its client mode and its file access override; they can be set or inherited in one
// update. Values are parsed as `--override-file-access-action` is for a machine or the global configuration
// (types.ParseFileAccessAction: case-insensitive, legacy spellings accepted) and stored as the enum name, so an unknown
// value is refused before anything is written. No confirmation flag is needed: unlike a lockdown, the override only
// relaxes or restores FAA enforcement. The override is written to the group's registry entry
// (Groups.<name>.OverrideFileAccessAction, a string), leaving the rest of the entry, client mode included, as it was,
// and MembershipVersion is bumped, in one conditional update (the group must still exist). Each change is audited as
// action "config.update" with target "group:<name>" (or "ungrouped") and a summary like
// "OverrideFileAccessAction=AUDIT_ONLY" or "OverrideFileAccessAction inherited". `config get --group`, `group list` and
// `group get` show it.

import (
	"context"
	"strings"
	"testing"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// groupFileAccess returns the group's stored OverrideFileAccessAction, "" when it has none, and fails the test when it
// is stored as anything but a string.
func groupFileAccess(t *testing.T, group string) string {
	t.Helper()
	v, ok := groupEntry(t, group)["OverrideFileAccessAction"]
	if !ok {
		return ""
	}
	s, isString := v.(*awstypes.AttributeValueMemberS)
	if !isString {
		t.Fatalf("Groups.%s.OverrideFileAccessAction is a %T, want a string", group, v)
	}
	return s.Value
}

// lineOf returns the first line of out that, trimmed, starts with prefix.
func lineOf(out, prefix string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), prefix) {
			return l
		}
	}
	return ""
}

func TestGroupFileAccessCLI_SetAndClear(t *testing.T) {
	ddblocal.Reset(t)
	groupCLI(t, "create", "pilot", "--description", "FAA pilot")
	mustConfigCLI(t, "update", "--group", "pilot", "-c", "lockdown", "--confirm-group-lockdown")
	before := membershipVersion(t)

	// Case-insensitive, as for a machine; no confirmation flag needed
	mustConfigCLI(t, "update", "--group", "Pilot", "--override-file-access-action", "Audit_Only")
	if got := groupFileAccess(t, "pilot"); got != "AUDIT_ONLY" {
		t.Fatalf("Groups.pilot.OverrideFileAccessAction = %q, want AUDIT_ONLY", got)
	}
	entry := groupEntry(t, "pilot")
	if mode, _ := entry["ClientMode"].(*awstypes.AttributeValueMemberN); mode == nil || mode.Value != "2" {
		t.Errorf("setting the override changed the group's ClientMode: %v, want 2 (LOCKDOWN)", entry["ClientMode"])
	}
	if desc, _ := entry["Description"].(*awstypes.AttributeValueMemberS); desc == nil || desc.Value != "FAA pilot" {
		t.Errorf("the rest of the entry changed: Description = %v", entry["Description"])
	}
	if _, ok := entry["RulesVersion"]; !ok {
		t.Error("the entry lost its RulesVersion")
	}
	if after := membershipVersion(t); after == before {
		t.Errorf("MembershipVersion stayed %s; setting a group's override must bump it", after)
	}

	got := mustConfigCLI(t, "get", "--group", "pilot")
	if line := lineOf(got, "OverrideFileAccessAction"); !strings.Contains(line, "AUDIT_ONLY") {
		t.Errorf("config get --group pilot: OverrideFileAccessAction line %q, want AUDIT_ONLY\n%s", line, got)
	}
	if !strings.Contains(lineOf(got, "ClientMode"), "LOCKDOWN") {
		t.Errorf("config get --group pilot no longer shows the client mode:\n%s", got)
	}
	if out := groupCLI(t, "list"); !strings.Contains(lineOf(out, "pilot"), "AUDIT_ONLY") {
		t.Errorf("group list does not show pilot's override:\n%s", out)
	}
	if out := groupCLI(t, "get", "pilot"); !strings.Contains(lineOf(out, "OverrideFileAccessAction"), "AUDIT_ONLY") {
		t.Errorf("group get pilot does not show an OverrideFileAccessAction line with AUDIT_ONLY:\n%s", out)
	}

	mustConfigCLI(t, "update", "--group", "pilot", "--override-file-access-action", "disable")
	if got := groupFileAccess(t, "pilot"); got != "DISABLE" {
		t.Errorf("after --override-file-access-action disable: %q, want DISABLE", got)
	}

	mustConfigCLI(t, "update", "--group", "pilot", "--inherit", "override-file-access-action")
	if got := groupFileAccess(t, "pilot"); got != "" {
		t.Errorf("--inherit override-file-access-action left %q", got)
	}
	if groupEntry(t, "pilot") == nil {
		t.Fatal("clearing the override removed the group")
	}
	if mode, _ := groupEntry(t, "pilot")["ClientMode"].(*awstypes.AttributeValueMemberN); mode == nil || mode.Value != "2" {
		t.Errorf("clearing the override changed the group's ClientMode: %v", groupEntry(t, "pilot")["ClientMode"])
	}
	if out := groupCLI(t, "list"); strings.Contains(lineOf(out, "pilot"), "AUDIT_ONLY") || strings.Contains(lineOf(out, "pilot"), "DISABLE") {
		t.Errorf("group list still shows an override for pilot:\n%s", out)
	}

	// The client mode's entry, then one per override change
	entries := groupModeEntries(t)
	if len(entries) != 4 {
		t.Fatalf("%d config.update audit entries, want 4: %+v", len(entries), entries)
	}
	var summaries []string
	for _, e := range entries {
		if e.Target != "group:pilot" {
			t.Errorf("audit target %q, want group:pilot", e.Target)
		}
		summaries = append(summaries, e.Summary)
	}
	joined := strings.Join(summaries, " | ")
	for _, want := range []string{"OverrideFileAccessAction=AUDIT_ONLY", "OverrideFileAccessAction=DISABLE", "OverrideFileAccessAction inherited"} {
		if !strings.Contains(joined, want) {
			t.Errorf("audit summaries %q, want %s", joined, want)
		}
	}
}

// Every spelling `--override-file-access-action` accepts for a machine is accepted, and stored as the enum name.
func TestGroupFileAccessCLI_ValuesAreStoredAsEnumNames(t *testing.T) {
	ddblocal.Reset(t)
	groupCLI(t, "create", "pilot")
	for _, c := range []struct{ arg, want string }{
		{"none", "NONE"},
		{"NONE", "NONE"},
		{"audit_only", "AUDIT_ONLY"},
		{"auditonly", "AUDIT_ONLY"},
		{"AuditOnly", "AUDIT_ONLY"},
		{"disable", "DISABLE"},
	} {
		mustConfigCLI(t, "update", "--group", "pilot", "--override-file-access-action", c.arg)
		if got := groupFileAccess(t, "pilot"); got != c.want {
			t.Errorf("--override-file-access-action %s stored %q, want %s", c.arg, got, c.want)
		}
	}
}

func TestGroupFileAccessCLI_RefusedArguments(t *testing.T) {
	ddblocal.Reset(t)
	groupCLI(t, "create", "pilot")
	mustConfigCLI(t, "update", "--group", "pilot", "--override-file-access-action", "audit_only")
	baseline := len(groupModeEntries(t))
	for name, args := range map[string][]string{
		"unknown value":            {"update", "--group", "pilot", "--override-file-access-action", "block"},
		"empty value":              {"update", "--group", "pilot", "--override-file-access-action", ""},
		"unknown value ungrouped":  {"update", "--ungrouped", "--override-file-access-action", "bogus"},
		"set and inherit":          {"update", "--group", "pilot", "--override-file-access-action", "none", "--inherit", "override-file-access-action"},
		"unregistered group":       {"update", "--group", "nope", "--override-file-access-action", "none"},
		"override and setting":     {"update", "--group", "pilot", "--override-file-access-action", "none", "--batch-size", "10"},
		"override and temporary":   {"update", "--group", "pilot", "--override-file-access-action", "none", "--for", "2h"},
		"override and clean sync":  {"update", "--group", "pilot", "--override-file-access-action", "none", "--clean-sync"},
		"set replaces a config":    {"set", "--group", "pilot", "--override-file-access-action", "none"},
		"reserved name as group":   {"update", "--group", "rudolph:ungrouped", "--override-file-access-action", "none"},
		"inherit for no ungrouped": {"update", "--ungrouped", "--inherit", "override-file-access-action"},
	} {
		if out, err := configCLI(t, args...); err == nil {
			t.Errorf("%s: %v was accepted:\n%s", name, args, out)
		}
	}
	if got := groupFileAccess(t, "pilot"); got != "AUDIT_ONLY" {
		t.Errorf("a refused update changed pilot's override to %q", got)
	}
	if entry := groupEntry(t, "rudolph:ungrouped"); entry != nil {
		t.Errorf("a refused update registered rudolph:ungrouped: %v", entry)
	}
	if n := len(groupModeEntries(t)) - baseline; n != 0 {
		t.Errorf("refused updates wrote %d audit entries", n)
	}
}

func TestGroupFileAccessCLI_Ungrouped(t *testing.T) {
	ddblocal.Reset(t)
	mustConfigCLI(t, "update", "--ungrouped", "--override-file-access-action", "disable")
	if got := groupFileAccess(t, "rudolph:ungrouped"); got != "DISABLE" {
		t.Fatalf("rudolph:ungrouped OverrideFileAccessAction = %q, want DISABLE (registered on demand)", got)
	}
	if got := mustConfigCLI(t, "get", "--ungrouped"); !strings.Contains(lineOf(got, "OverrideFileAccessAction"), "DISABLE") {
		t.Errorf("config get --ungrouped:\n%s", got)
	}
	entries := groupModeEntries(t)
	if len(entries) != 1 || entries[0].Target != "ungrouped" || !strings.Contains(entries[0].Summary, "OverrideFileAccessAction=DISABLE") {
		t.Errorf("audit entries %+v, want one with target ungrouped and OverrideFileAccessAction=DISABLE", entries)
	}

	mustConfigCLI(t, "update", "--ungrouped", "--inherit", "override-file-access-action")
	if got := groupFileAccess(t, "rudolph:ungrouped"); got != "" {
		t.Errorf("--ungrouped --inherit override-file-access-action left %q", got)
	}
}

// The client mode and the override can be changed together; the change is one update and one audit entry.
func TestGroupFileAccessCLI_WithTheClientMode(t *testing.T) {
	ddblocal.Reset(t)
	groupCLI(t, "create", "pilot")
	mustConfigCLI(t, "update", "--group", "pilot", "-c", "monitor", "--override-file-access-action", "audit_only")
	entry := groupEntry(t, "pilot")
	if mode, _ := entry["ClientMode"].(*awstypes.AttributeValueMemberN); mode == nil || mode.Value != "1" {
		t.Errorf("ClientMode = %v, want 1 (MONITOR)", entry["ClientMode"])
	}
	if got := groupFileAccess(t, "pilot"); got != "AUDIT_ONLY" {
		t.Errorf("OverrideFileAccessAction = %q, want AUDIT_ONLY", got)
	}
	entries := groupModeEntries(t)
	if len(entries) != 1 || !strings.Contains(entries[0].Summary, "ClientMode=MONITOR") || !strings.Contains(entries[0].Summary, "OverrideFileAccessAction=AUDIT_ONLY") {
		t.Errorf("audit entries %+v, want one with ClientMode=MONITOR and OverrideFileAccessAction=AUDIT_ONLY", entries)
	}

	// Setting one and inheriting the other
	mustConfigCLI(t, "update", "--group", "pilot", "--inherit", "client-mode", "--override-file-access-action", "disable")
	if _, ok := groupEntry(t, "pilot")["ClientMode"]; ok {
		t.Error("--inherit client-mode left the group's ClientMode")
	}
	if got := groupFileAccess(t, "pilot"); got != "DISABLE" {
		t.Errorf("OverrideFileAccessAction = %q, want DISABLE", got)
	}

	// Inheriting both
	mustConfigCLI(t, "update", "--group", "pilot", "--inherit", "client-mode", "--inherit", "override-file-access-action")
	if got := groupFileAccess(t, "pilot"); got != "" {
		t.Errorf("inheriting both left OverrideFileAccessAction %q", got)
	}
	if groupEntry(t, "pilot") == nil {
		t.Fatal("inheriting both removed the group")
	}
}

// A value edited by hand into the registry that is not a file access action is shown as unreadable, not as if it
// were in force, and does not stop the group from being listed or changed.
func TestGroupFileAccessCLI_UnreadableStoredValue(t *testing.T) {
	ddblocal.Reset(t)
	groupCLI(t, "create", "pilot")
	_, err := ddblocal.Raw(t).UpdateItem(context.Background(), &awsdynamodb.UpdateItemInput{
		TableName:                 aws.String(ddblocal.TableName),
		Key:                       map[string]awstypes.AttributeValue{"PK": &awstypes.AttributeValueMemberS{Value: "GroupRegistry"}, "SK": &awstypes.AttributeValueMemberS{Value: "Current"}},
		UpdateExpression:          aws.String("SET #g.#n.#a = :a"),
		ExpressionAttributeNames:  map[string]string{"#g": "Groups", "#n": "pilot", "#a": "OverrideFileAccessAction"},
		ExpressionAttributeValues: map[string]awstypes.AttributeValue{":a": &awstypes.AttributeValueMemberN{Value: "2"}},
	})
	if err != nil {
		t.Fatal(err)
	}

	got := mustConfigCLI(t, "get", "--group", "pilot")
	line := lineOf(got, "OverrideFileAccessAction")
	if !strings.Contains(line, "unreadable") && !strings.Contains(line, "cannot be read") {
		t.Errorf("config get --group: OverrideFileAccessAction line %q, want it marked unreadable\n%s", line, got)
	}
	if out := groupCLI(t, "list"); !strings.Contains(lineOf(out, "pilot"), "unreadable") {
		t.Errorf("group list: want pilot's override shown as unreadable\n%s", out)
	}
	mustConfigCLI(t, "update", "--group", "pilot", "--override-file-access-action", "none")
	if got := groupFileAccess(t, "pilot"); got != "NONE" {
		t.Errorf("after setting NONE over an unreadable value: %q", got)
	}
}
