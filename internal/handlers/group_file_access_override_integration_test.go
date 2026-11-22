//go:build integration

package handlers

// File access override per device group (owner request, 2026-11-23): a safety valve while moving File Access
// Authorization rules from audit to enforcement. If an enforced rule breaks something, a whole group can be put back to
// audit-only, or have FAA turned off, at once.
//
// A registered group (and the reserved "rudolph:ungrouped") may hold the attribute `OverrideFileAccessAction` in its
// entry of the registry's Groups map (`GroupRegistry`/`Current`), next to its `ClientMode`: a string holding the enum
// name Santa expects, `NONE`, `AUDIT_ONLY` or `DISABLE`, as on configuration rows. Setting or clearing it bumps
// MembershipVersion (the CLI's job, see internal/cli/config/group_file_access_override_cli_integration_test.go).
//
// The `override_file_access_action` preflight sends a Mac is the first of:
//  1. the OverrideFileAccessAction stored on its own configuration row;
//  2. the most relaxed value among its groups that have one: DISABLE, then AUDIT_ONLY, then NONE. A Mac in no group
//     is in "rudolph:ungrouped", which can hold one like any group;
//  3. the global configuration's;
//  4. none: the field is left out and Santa keeps its current setting.
//
// Between groups the most relaxed value wins, the opposite of client mode's strictest-wins rule: the switch exists to
// loosen enforcement quickly, so a Mac in an "audit" group and an "enforce" group must get audit-only. A group's value
// beats the global one either way, so a group can also be held at NONE while the fleet audits.
//
// A Mac's groups are worked out as for client modes and rules (groups.Effective): assigned groups, plus the groups of
// the list this preflight reports that accept reported membership. The client mode and the file access override are
// resolved independently: a Mac's own or temporary client mode does not stop its groups' override, and its own override
// does not stop its groups' client mode. A stored value that is not a file access action is ignored with a log line,
// as if the group had none; the legacy spelling the machine configuration accepts ("AuditOnly") counts as AUDIT_ONLY.
// Without a group override anywhere in the registry, preflight reads nothing more than before.

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/internal/testutil/santaproto"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"google.golang.org/protobuf/types/dynamicpb"
)

// setGroupFileAccess stores a file access override on a registered group's entry, as the CLI does (nil removes it; an
// int stores a number, to test undecodable values).
func setGroupFileAccess(t *testing.T, group string, action any) {
	t.Helper()
	in := &awsdynamodb.UpdateItemInput{
		TableName:                aws.String(ddblocal.TableName),
		Key:                      map[string]awstypes.AttributeValue{"PK": &awstypes.AttributeValueMemberS{Value: "GroupRegistry"}, "SK": &awstypes.AttributeValueMemberS{Value: "Current"}},
		ExpressionAttributeNames: map[string]string{"#g": "Groups", "#n": group, "#a": "OverrideFileAccessAction"},
	}
	one := &awstypes.AttributeValueMemberN{Value: "1"}
	set := func(v awstypes.AttributeValue) {
		in.UpdateExpression = aws.String("SET #g.#n.#a = :a ADD MembershipVersion :one")
		in.ExpressionAttributeValues = map[string]awstypes.AttributeValue{":one": one, ":a": v}
	}
	switch v := action.(type) {
	case nil:
		in.UpdateExpression = aws.String("REMOVE #g.#n.#a ADD MembershipVersion :one")
		in.ExpressionAttributeValues = map[string]awstypes.AttributeValue{":one": one}
	case types.FileAccessAction:
		set(&awstypes.AttributeValueMemberS{Value: string(v)})
	case string:
		set(&awstypes.AttributeValueMemberS{Value: v})
	case int:
		set(&awstypes.AttributeValueMemberN{Value: strconv.Itoa(v)})
	default:
		t.Fatalf("setGroupFileAccess: unsupported %T", action)
	}
	if _, err := ddblocal.Raw(t).UpdateItem(context.Background(), in); err != nil {
		t.Fatalf("set the file access override of %s: %v", group, err)
	}
	handlers = newHandlers()
}

// setGlobalFileAccess writes the universal default as the global configuration, with this override ("" for none).
func setGlobalFileAccess(t *testing.T, action types.FileAccessAction) {
	t.Helper()
	cfg := machineconfiguration.GetUniversalDefaultConfig()
	cfg.OverrideFileAccessAction = action
	if err := configService(t).SetGlobalConfig(cfg); err != nil {
		t.Fatal(err)
	}
	handlers = newHandlers()
}

// setMachineFileAccess gives the machine its own override, on a machine row holding only that setting.
func setMachineFileAccess(t *testing.T, machineID string, action types.FileAccessAction) {
	t.Helper()
	if _, err := configService(t).UpdateMachineConfig(machineID, machineconfiguration.MachineConfigurationUpdateRequest{OverrideFileAccessAction: &action}); err != nil {
		t.Fatal(err)
	}
	handlers = newHandlers()
}

var sentFileAccessPattern = regexp.MustCompile(`"override_file_access_action":"([A-Za-z_]+)"`)

// sentFileAccess is the override_file_access_action a preflight response told Santa to use, "" when left out.
func sentFileAccess(t *testing.T, preflight *dynamicpb.Message) string {
	t.Helper()
	m := sentFileAccessPattern.FindStringSubmatch(santaproto.Canonical(t, preflight))
	if m == nil {
		return ""
	}
	return m[1]
}

func TestGroupFileAccess_MembersGetTheGroupsOverride(t *testing.T) {
	transport := groupsServer(t)
	registerGroups(t, map[string]bool{"faa-audit": true, "staff": true})
	setGroupFileAccess(t, "faa-audit", types.FileAccessActionAuditOnly)

	reporter := newHost(t, machineA, transport)
	reporter.PrimaryUserGroups = []string{"FAA-Audit"} // normalised
	if got := sentFileAccess(t, reporter.MustSync(t).Preflight); got != "AUDIT_ONLY" {
		t.Errorf("a Mac reporting faa-audit: %q, want the group's AUDIT_ONLY in the same sync", got)
	}

	assignGroups(t, machineB, "faa-audit")
	assigned := newHost(t, machineB, transport)
	if got := sentFileAccess(t, assigned.MustSync(t).Preflight); got != "AUDIT_ONLY" {
		t.Errorf("a Mac assigned faa-audit: %q, want AUDIT_ONLY", got)
	}

	other := newHost(t, "CCCCCCCC-3333-4333-8333-CCCCCCCCCCCC", transport)
	other.PrimaryUserGroups = []string{"staff"} // a group without an override
	if got := sentFileAccess(t, other.MustSync(t).Preflight); got != "" {
		t.Errorf("a Mac in a group without an override, no global one: %q, want the field left out", got)
	}

	// Leaving the group: the Mac stops reporting it
	reporter.PrimaryUserGroups = nil
	if got := sentFileAccess(t, reporter.MustSync(t).Preflight); got != "" {
		t.Errorf("after leaving faa-audit: %q, want the field left out", got)
	}

	// Clearing the group's override
	setGroupFileAccess(t, "faa-audit", nil)
	if got := sentFileAccess(t, assigned.MustSync(t).Preflight); got != "" {
		t.Errorf("after the group's override was cleared: %q, want the field left out", got)
	}
}

// A group's value beats the global one, in both directions: a group can be loosened while the fleet enforces, or held
// at NONE (enforcing) while the fleet audits.
func TestGroupFileAccess_AGroupBeatsTheGlobalOverride(t *testing.T) {
	for _, c := range []struct {
		name   string
		global types.FileAccessAction
		group  types.FileAccessAction
	}{
		{"loosen an enforcing fleet", types.FileAccessActionNone, types.FileAccessActionAuditOnly},
		{"turn FAA off for a group", types.FileAccessActionAuditOnly, types.FileAccessActionDisable},
		{"hold a group at NONE while the fleet audits", types.FileAccessActionAuditOnly, types.FileAccessActionNone},
	} {
		t.Run(c.name, func(t *testing.T) {
			transport := groupsServer(t)
			setGlobalFileAccess(t, c.global)
			registerGroups(t, map[string]bool{"lab": true, "staff": true})
			setGroupFileAccess(t, "lab", c.group)

			in := newHost(t, machineA, transport)
			in.PrimaryUserGroups = []string{"lab"}
			if got := sentFileAccess(t, in.MustSync(t).Preflight); got != string(c.group) {
				t.Errorf("a Mac in lab (%s) on a %s fleet: %q, want the group's %s", c.group, c.global, got, c.group)
			}
			out := newHost(t, machineB, transport)
			out.PrimaryUserGroups = []string{"staff"}
			if got := sentFileAccess(t, out.MustSync(t).Preflight); got != string(c.global) {
				t.Errorf("a Mac outside lab: %q, want the global %s", got, c.global)
			}
		})
	}
}

// Between groups the MOST RELAXED override wins: DISABLE, then AUDIT_ONLY, then NONE. This is deliberately the opposite
// of client mode's strictest-wins rule (TestGroupMode_TheStrictestGroupWins): the per-group override is a safety valve
// for backing out of FAA enforcement, so putting a group back to audit-only must take effect on every Mac in it, even
// one that is also in a group that enforces.
func TestGroupFileAccess_TheMostRelaxedGroupWins(t *testing.T) {
	for _, c := range []struct {
		name    string
		actions map[string]types.FileAccessAction
		// global is the global override, never the one wanted, so that only the groups' value can pass
		global types.FileAccessAction
		want   string
	}{
		{"audit-only beats none", map[string]types.FileAccessAction{"a-audit": types.FileAccessActionAuditOnly, "z-enforce": types.FileAccessActionNone}, types.FileAccessActionNone, "AUDIT_ONLY"},
		{"audit-only beats none, whatever the names", map[string]types.FileAccessAction{"a-enforce": types.FileAccessActionNone, "z-audit": types.FileAccessActionAuditOnly}, types.FileAccessActionNone, "AUDIT_ONLY"},
		{"disable beats audit-only", map[string]types.FileAccessAction{"a-disable": types.FileAccessActionDisable, "z-audit": types.FileAccessActionAuditOnly}, types.FileAccessActionNone, "DISABLE"},
		{"disable beats audit-only, whatever the names", map[string]types.FileAccessAction{"a-audit": types.FileAccessActionAuditOnly, "z-disable": types.FileAccessActionDisable}, types.FileAccessActionNone, "DISABLE"},
		{"disable beats none", map[string]types.FileAccessAction{"a-enforce": types.FileAccessActionNone, "z-disable": types.FileAccessActionDisable}, types.FileAccessActionNone, "DISABLE"},
		{"all three", map[string]types.FileAccessAction{"a-enforce": types.FileAccessActionNone, "m-disable": types.FileAccessActionDisable, "z-audit": types.FileAccessActionAuditOnly}, types.FileAccessActionNone, "DISABLE"},
		// Groups that set nothing take no part: one group's NONE is the groups' value, and beats the global AUDIT_ONLY
		{"a group without an override takes no part", map[string]types.FileAccessAction{"a-enforce": types.FileAccessActionNone}, types.FileAccessActionAuditOnly, "NONE"},
	} {
		t.Run(c.name, func(t *testing.T) {
			transport := groupsServer(t)
			setGlobalFileAccess(t, c.global)
			accept := map[string]bool{"no-override": true}
			var names []string
			for name := range c.actions {
				accept[name] = true
				names = append(names, name)
			}
			registerGroups(t, accept)
			for name, action := range c.actions {
				setGroupFileAccess(t, name, action)
			}
			h := newHost(t, machineA, transport)
			h.PrimaryUserGroups = append(names, "no-override")
			if got := sentFileAccess(t, h.MustSync(t).Preflight); got != c.want {
				t.Errorf("%q, want %s", got, c.want)
			}
		})
	}
}

// The Mac's own override beats any group: how a single Mac is exempted from (or kept out of) its group's switch.
func TestGroupFileAccess_TheMachinesOwnOverrideWins(t *testing.T) {
	transport := groupsServer(t)
	setGlobalFileAccess(t, types.FileAccessActionAuditOnly)
	registerGroups(t, map[string]bool{"faa-off": true})
	setGroupFileAccess(t, "faa-off", types.FileAccessActionDisable)

	setMachineFileAccess(t, machineA, types.FileAccessActionNone)
	a := newHost(t, machineA, transport)
	a.PrimaryUserGroups = []string{"faa-off"}
	if got := sentFileAccess(t, a.MustSync(t).Preflight); got != "NONE" {
		t.Errorf("the Mac's own NONE in a DISABLE group: %q, want NONE", got)
	}

	// A machine row that does not set the override, here one holding only a batch size, does not stop the group's
	putItem(t, map[string]any{"PK": "Machine#" + machineB, "SK": "Config", "DataType": "MachineConfiguration", "BatchSize": 25})
	handlers = newHandlers()
	b := newHost(t, machineB, transport)
	b.PrimaryUserGroups = []string{"faa-off"}
	if got := sentFileAccess(t, b.MustSync(t).Preflight); got != "DISABLE" {
		t.Errorf("a Mac whose own row sets only BatchSize: %q, want the group's DISABLE", got)
	}

	// Nor does a full row written by `config set --machine` without an override
	const machineC = "CCCCCCCC-3333-4333-8333-CCCCCCCCCCCC"
	if err := configService(t).SetMachineConfig(machineC, machineconfiguration.GetUniversalDefaultConfig()); err != nil {
		t.Fatal(err)
	}
	handlers = newHandlers()
	c := newHost(t, machineC, transport)
	c.PrimaryUserGroups = []string{"faa-off"}
	if got := sentFileAccess(t, c.MustSync(t).Preflight); got != "DISABLE" {
		t.Errorf("a Mac whose `config set --machine` row has no override: %q, want the group's DISABLE", got)
	}

	// Removing the Mac's own override hands it back to the group
	if _, err := configService(t).UpdateMachineConfig(machineA, machineconfiguration.MachineConfigurationUpdateRequest{Inherit: []string{"OverrideFileAccessAction"}}); err != nil {
		t.Fatal(err)
	}
	handlers = newHandlers()
	if got := sentFileAccess(t, a.MustSync(t).Preflight); got != "DISABLE" {
		t.Errorf("after the Mac's own override was removed: %q, want the group's DISABLE", got)
	}
}

// The client mode and the file access override are resolved separately: a Mac's own or temporary client mode, which
// makes its groups' client modes irrelevant, must not make its groups' override irrelevant too, and the other way round.
func TestGroupFileAccess_IndependentOfTheClientMode(t *testing.T) {
	transport := groupsServer(t)
	setGlobalClientMode(t, types.Monitor)
	registerGroups(t, map[string]bool{"pilot": true})
	setGroupMode(t, "pilot", types.Lockdown)
	setGroupFileAccess(t, "pilot", types.FileAccessActionAuditOnly)

	both := newHost(t, machineA, transport)
	both.PrimaryUserGroups = []string{"pilot"}
	res := both.MustSync(t)
	if got := sentMode(t, res.Preflight); got != "LOCKDOWN" {
		t.Errorf("a Mac in pilot: client mode %s, want the group's LOCKDOWN", got)
	}
	if got := sentFileAccess(t, res.Preflight); got != "AUDIT_ONLY" {
		t.Errorf("a Mac in pilot: override %q, want the group's AUDIT_ONLY", got)
	}

	own := machineconfiguration.GetUniversalDefaultConfig()
	own.ClientMode = types.Monitor
	if err := configService(t).SetMachineConfig(machineB, own); err != nil {
		t.Fatal(err)
	}
	handlers = newHandlers()
	ownMode := newHost(t, machineB, transport)
	ownMode.PrimaryUserGroups = []string{"pilot"}
	res = ownMode.MustSync(t)
	if got := sentMode(t, res.Preflight); got != "MONITOR" {
		t.Errorf("a Mac with its own MONITOR: client mode %s, want MONITOR", got)
	}
	if got := sentFileAccess(t, res.Preflight); got != "AUDIT_ONLY" {
		t.Errorf("a Mac with its own client mode: override %q, want the group's AUDIT_ONLY all the same", got)
	}

	const machineC = "CCCCCCCC-3333-4333-8333-CCCCCCCCCCCC"
	setTemporaryMode(t, machineC, types.Monitor, time.Now().Add(time.Hour))
	handlers = newHandlers()
	temporary := newHost(t, machineC, transport)
	temporary.PrimaryUserGroups = []string{"pilot"}
	res = temporary.MustSync(t)
	if got := sentMode(t, res.Preflight); got != "MONITOR" {
		t.Errorf("a Mac with a temporary MONITOR: client mode %s, want MONITOR", got)
	}
	if got := sentFileAccess(t, res.Preflight); got != "AUDIT_ONLY" {
		t.Errorf("a Mac with a temporary client mode: override %q, want the group's AUDIT_ONLY all the same", got)
	}

	// The Mac's own override leaves the group's client mode alone
	setMachineFileAccess(t, machineA, types.FileAccessActionNone)
	res = both.MustSync(t)
	if got := sentMode(t, res.Preflight); got != "LOCKDOWN" {
		t.Errorf("a Mac with its own override: client mode %s, want the group's LOCKDOWN all the same", got)
	}
	if got := sentFileAccess(t, res.Preflight); got != "NONE" {
		t.Errorf("a Mac with its own override: %q, want its NONE", got)
	}
}

func TestGroupFileAccess_UngroupedMacs(t *testing.T) {
	transport := groupsServer(t)
	setGlobalFileAccess(t, types.FileAccessActionNone)
	registerGroups(t, map[string]bool{"eng": true, ungrouped: false})
	setGroupFileAccess(t, ungrouped, types.FileAccessActionAuditOnly)

	h := newHost(t, machineA, transport)
	h.PrimaryUserGroups = []string{"some-directory-group"} // not registered: still ungrouped
	if got := sentFileAccess(t, h.MustSync(t).Preflight); got != "AUDIT_ONLY" {
		t.Errorf("an ungrouped Mac: %q, want the ungrouped AUDIT_ONLY", got)
	}
	h.PrimaryUserGroups = []string{"eng"}
	if got := sentFileAccess(t, h.MustSync(t).Preflight); got != "NONE" {
		t.Errorf("after joining eng (no override): %q, want the global NONE, not the ungrouped override", got)
	}
}

func TestGroupFileAccess_GroupsThatDoNotCount(t *testing.T) {
	transport := groupsServer(t)
	setGlobalFileAccess(t, types.FileAccessActionNone)
	registerGroups(t, map[string]bool{"assigned-only": false, "eng": true})
	setGroupFileAccess(t, "assigned-only", types.FileAccessActionDisable)

	h := newHost(t, machineA, transport)
	h.PrimaryUserGroups = []string{"assigned-only", "unregistered", "eng"}
	if got := sentFileAccess(t, h.MustSync(t).Preflight); got != "NONE" {
		t.Errorf("%q, want the global NONE: a reported group that accepts only assigned members does not count", got)
	}
	assignGroups(t, machineA, "assigned-only")
	if got := sentFileAccess(t, h.MustSync(t).Preflight); got != "DISABLE" {
		t.Errorf("once assigned to assigned-only: %q, want its DISABLE", got)
	}
}

// A value edited by hand into the registry that is not a file access action is ignored with a log line, as preflight
// does for the machine configuration: it neither fails the sync nor stops the registry (and so every group's rules and
// client mode) from being read. The legacy spelling the machine configuration accepts is normalised.
func TestGroupFileAccess_UndecodableValuesAreIgnored(t *testing.T) {
	transport := groupsServer(t)
	setGlobalClientMode(t, types.Monitor)
	setGlobalFileAccess(t, types.FileAccessActionNone)
	registerGroups(t, map[string]bool{"bad-string": true, "bad-number": true, "lockdown": true, "legacy": true, "valid-elsewhere": true})
	setGroupFileAccess(t, "bad-string", "BLOCK_EVERYTHING")
	setGroupFileAccess(t, "bad-number", 2)
	setGroupMode(t, "lockdown", types.Lockdown)
	setGroupFileAccess(t, "legacy", "AuditOnly")
	// a valid override in the registry, on a group the Mac is not in, so that group overrides are being resolved
	setGroupFileAccess(t, "valid-elsewhere", types.FileAccessActionDisable)

	logs := captureLog(t)
	h := newHost(t, machineA, transport)
	h.PrimaryUserGroups = []string{"bad-string", "bad-number", "lockdown"}
	res, err := h.Sync(t)
	if err != nil {
		t.Fatalf("a bad stored override made the sync fail: %v", err)
	}
	if got := sentFileAccess(t, res.Preflight); got != "NONE" {
		t.Errorf("%q, want the global NONE: undecodable group overrides are ignored", got)
	}
	if canonical := santaproto.Canonical(t, res.Preflight); strings.Contains(canonical, "BLOCK_EVERYTHING") {
		t.Errorf("the bad value was sent to Santa: %s", canonical)
	}
	if got := sentMode(t, res.Preflight); got != "LOCKDOWN" {
		t.Errorf("client mode %s, want lockdown's LOCKDOWN: a bad override must not stop the registry from being read", got)
	}
	logged := logs.String()
	if !strings.Contains(logged, "bad-string") || !strings.Contains(logged, "OverrideFileAccessAction") {
		t.Errorf("no log line naming the group bad-string and OverrideFileAccessAction:\n%s", logged)
	}

	legacy := newHost(t, machineB, transport)
	legacy.PrimaryUserGroups = []string{"legacy", "bad-string"}
	if got := sentFileAccess(t, legacy.MustSync(t).Preflight); got != "AUDIT_ONLY" {
		t.Errorf("a group storing the legacy spelling AuditOnly: %q, want AUDIT_ONLY", got)
	}
}

// groupsItemReads counts the GetItem calls on Macs' `Groups` items from now until the end of the test.
func groupsItemReads(t *testing.T) *int {
	t.Helper()
	reads := new(int)
	ddblocal.AddHook(t, ddblocal.Hook{Before: func(c *ddblocal.Call) *ddblocal.Fault {
		if c.Op == "GetItem" && strings.Contains(c.InputJSON(), `"SK":{"S":"Groups"}`) {
			*reads++
		}
		return nil
	}})
	return reads
}

// COST2: the registry is already read (and cached) for group client modes and rules, so the override costs nothing
// while no group sets one. Before this feature a steady-state sync of a Mac in a group without a client mode read its
// groups item once (rule download); with a group client mode, twice (preflight too, see
// TestGroupMode_SteadyStateSyncStaysWithinBudget).
func TestGroupFileAccess_NoExtraReadsWithoutAGroupOverride(t *testing.T) {
	transport := groupsServer(t)
	setGlobalFileAccess(t, types.FileAccessActionAuditOnly)
	registerGroups(t, map[string]bool{"eng": true, "staff": true})
	assignGroups(t, machineA, "staff")
	h := newHost(t, machineA, transport)
	h.PrimaryUserGroups = []string{"eng"}
	syncNormal(t, h)
	h.MustSync(t)

	reads := groupsItemReads(t)
	ddblocal.ResetCounts()
	res := h.MustSync(t)
	if *reads != 1 {
		t.Errorf("steady-state sync without any group override or client mode read the Mac's groups item %d times, want 1 as before (rule download only)", *reads)
	}
	if c := ddblocal.ConsumedCapacity(); c.WriteUnits > 4 || c.ReadUnits > 8 {
		t.Errorf("steady-state sync: %s; budget 4 WCU and 8 RCU", c)
	}
	if got := sentFileAccess(t, res.Preflight); got != "AUDIT_ONLY" {
		t.Errorf("%q, want the global AUDIT_ONLY", got)
	}
}

// With a group override, preflight may read the Mac's groups item once, and only once, however many of client mode
// and override its groups set.
func TestGroupFileAccess_SteadyStateSyncStaysWithinBudget(t *testing.T) {
	for _, c := range []struct {
		name       string
		clientMode bool
	}{
		{"override only", false},
		{"override and client mode", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			transport := groupsServer(t)
			registerGroups(t, map[string]bool{"pilot": true, "staff": true})
			setGroupFileAccess(t, "pilot", types.FileAccessActionAuditOnly)
			if c.clientMode {
				setGroupMode(t, "pilot", types.Lockdown)
			}
			assignGroups(t, machineA, "staff")
			h := newHost(t, machineA, transport)
			h.PrimaryUserGroups = []string{"pilot"}
			syncNormal(t, h)
			h.MustSync(t)

			reads := groupsItemReads(t)
			ddblocal.ResetCounts()
			res := h.MustSync(t)
			if *reads > 2 {
				t.Errorf("steady-state sync read the Mac's groups item %d times, want at most 2 (rule download, and preflight once for client mode and override together)", *reads)
			}
			if used := ddblocal.ConsumedCapacity(); used.WriteUnits > 4 || used.ReadUnits > 8 {
				t.Errorf("steady-state sync of a Mac in a group with an override: %s; budget 4 WCU and 8 RCU", used)
			}
			if got := sentFileAccess(t, res.Preflight); got != "AUDIT_ONLY" {
				t.Errorf("%q, want AUDIT_ONLY", got)
			}
			if c.clientMode {
				if got := sentMode(t, res.Preflight); got != "LOCKDOWN" {
					t.Errorf("client mode %s, want LOCKDOWN", got)
				}
			}
		})
	}
}
