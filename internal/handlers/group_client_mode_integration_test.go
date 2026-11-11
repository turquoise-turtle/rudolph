//go:build integration

package handlers

// Client mode per device group (owner approved, 2026-11-11). A registered group may hold a client mode: the attribute
// `ClientMode` (a number, as on configuration rows: 1 MONITOR, 2 LOCKDOWN, 3 STANDALONE) in its entry of the registry's
// Groups map (`GroupRegistry`/`Current`). Setting or clearing it bumps MembershipVersion (the CLI's job, see
// internal/cli/config/group_client_mode_cli_integration_test.go). Only the client mode, no other setting, for now.
//
// The client mode preflight sends a Mac is the first of:
//  1. its temporary client mode, while it lasts;
//  2. the ClientMode stored on its own configuration row;
//  3. the strictest mode among its groups that have one: LOCKDOWN, then STANDALONE, then MONITOR. A Mac in no group is
//     in "rudolph:ungrouped", which can hold a mode like any group;
//  4. the global configuration's (or the default MONITOR).
//
// A group's mode may loosen the global one (a MONITOR group on a LOCKDOWN fleet), as a group rule may loosen a global
// rule. A Mac's groups are worked out as for rules (groups.Effective): assigned groups, plus the groups of the list
// this preflight reports that accept reported membership, so joining a group by reporting it takes effect in the
// same sync. A stored mode that cannot be decoded (not a number, or not 1–3) is ignored, as if the group had none.
// The registry is cached for up to a minute (the tests start a fresh server with newHandlers). Without a group mode
// anywhere in the registry, preflight costs no more than before.

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// setGroupMode stores a client mode on a registered group's entry (nil removes it).
func setGroupMode(t *testing.T, group string, mode any) {
	t.Helper()
	in := &awsdynamodb.UpdateItemInput{
		TableName:                aws.String(ddblocal.TableName),
		Key:                      map[string]awstypes.AttributeValue{"PK": &awstypes.AttributeValueMemberS{Value: "GroupRegistry"}, "SK": &awstypes.AttributeValueMemberS{Value: "Current"}},
		ExpressionAttributeNames: map[string]string{"#g": "Groups", "#n": group, "#c": "ClientMode"},
	}
	one := &awstypes.AttributeValueMemberN{Value: "1"}
	switch v := mode.(type) {
	case nil:
		in.UpdateExpression = aws.String("REMOVE #g.#n.#c ADD MembershipVersion :one")
		in.ExpressionAttributeValues = map[string]awstypes.AttributeValue{":one": one}
	case types.ClientMode:
		in.UpdateExpression = aws.String("SET #g.#n.#c = :m ADD MembershipVersion :one")
		in.ExpressionAttributeValues = map[string]awstypes.AttributeValue{":one": one, ":m": &awstypes.AttributeValueMemberN{Value: strconv.Itoa(int(v))}}
	case int:
		in.UpdateExpression = aws.String("SET #g.#n.#c = :m ADD MembershipVersion :one")
		in.ExpressionAttributeValues = map[string]awstypes.AttributeValue{":one": one, ":m": &awstypes.AttributeValueMemberN{Value: strconv.Itoa(v)}}
	case string:
		in.UpdateExpression = aws.String("SET #g.#n.#c = :m ADD MembershipVersion :one")
		in.ExpressionAttributeValues = map[string]awstypes.AttributeValue{":one": one, ":m": &awstypes.AttributeValueMemberS{Value: v}}
	default:
		t.Fatalf("setGroupMode: unsupported %T", mode)
	}
	if _, err := ddblocal.Raw(t).UpdateItem(context.Background(), in); err != nil {
		t.Fatalf("set the mode of %s: %v", group, err)
	}
	handlers = newHandlers()
}

func TestGroupMode_MembersGetTheGroupsMode(t *testing.T) {
	transport := groupsServer(t)
	setGlobalClientMode(t, types.Monitor)
	registerGroups(t, map[string]bool{"pilot": true, "staff": true})
	setGroupMode(t, "pilot", types.Lockdown)

	reporter := newHost(t, machineA, transport)
	reporter.PrimaryUserGroups = []string{"Pilot"} // normalised
	if got := sentMode(t, reporter.MustSync(t).Preflight); got != "LOCKDOWN" {
		t.Errorf("a Mac reporting pilot: %s, want the group's LOCKDOWN in the same sync", got)
	}

	assignGroups(t, machineB, "pilot")
	assigned := newHost(t, machineB, transport)
	if got := sentMode(t, assigned.MustSync(t).Preflight); got != "LOCKDOWN" {
		t.Errorf("a Mac assigned pilot: %s, want LOCKDOWN", got)
	}

	other := newHost(t, "CCCCCCCC-3333-4333-8333-CCCCCCCCCCCC", transport)
	other.PrimaryUserGroups = []string{"staff"} // a group without a mode
	if got := sentMode(t, other.MustSync(t).Preflight); got != "MONITOR" {
		t.Errorf("a Mac in a group without a mode: %s, want the global MONITOR", got)
	}

	// Leaving the group: the Mac stops reporting it
	reporter.PrimaryUserGroups = nil
	if got := sentMode(t, reporter.MustSync(t).Preflight); got != "MONITOR" {
		t.Errorf("after leaving pilot: %s, want the global MONITOR", got)
	}

	// Clearing the group's mode
	setGroupMode(t, "pilot", nil)
	if got := sentMode(t, assigned.MustSync(t).Preflight); got != "MONITOR" {
		t.Errorf("after the group's mode was cleared: %s, want the global MONITOR", got)
	}
}

func TestGroupMode_AGroupCanLoosenTheGlobalMode(t *testing.T) {
	transport := groupsServer(t)
	setGlobalClientMode(t, types.Lockdown)
	registerGroups(t, map[string]bool{"lab": true})
	setGroupMode(t, "lab", types.Monitor)
	h := newHost(t, machineA, transport)
	h.PrimaryUserGroups = []string{"lab"}
	if got := sentMode(t, h.MustSync(t).Preflight); got != "MONITOR" {
		t.Errorf("a MONITOR group on a LOCKDOWN fleet: %s, want MONITOR", got)
	}
}

func TestGroupMode_TheStrictestGroupWins(t *testing.T) {
	for _, c := range []struct {
		name  string
		modes map[string]types.ClientMode
		want  string
	}{
		{"lockdown beats monitor", map[string]types.ClientMode{"a-monitor": types.Monitor, "z-lockdown": types.Lockdown}, "LOCKDOWN"},
		{"lockdown beats monitor, whatever the names", map[string]types.ClientMode{"a-lockdown": types.Lockdown, "z-monitor": types.Monitor}, "LOCKDOWN"},
		{"lockdown beats standalone", map[string]types.ClientMode{"a-standalone": types.Standalone, "z-lockdown": types.Lockdown}, "LOCKDOWN"},
		{"standalone beats monitor", map[string]types.ClientMode{"a-monitor": types.Monitor, "z-standalone": types.Standalone}, "STANDALONE"},
	} {
		t.Run(c.name, func(t *testing.T) {
			transport := groupsServer(t)
			setGlobalClientMode(t, types.Monitor)
			accept := map[string]bool{"no-mode": true}
			var names []string
			for name := range c.modes {
				accept[name] = true
				names = append(names, name)
			}
			registerGroups(t, accept)
			for name, mode := range c.modes {
				setGroupMode(t, name, mode)
			}
			h := newHost(t, machineA, transport)
			h.PrimaryUserGroups = append(names, "no-mode")
			if got := sentMode(t, h.MustSync(t).Preflight); got != c.want {
				t.Errorf("%s, want %s", got, c.want)
			}
		})
	}
}

// The Mac's own mode, and a temporary mode, beat any group: how a single Mac is exempted from its group's lockdown.
func TestGroupMode_TheMachinesOwnAndTemporaryModesWin(t *testing.T) {
	transport := groupsServer(t)
	setGlobalClientMode(t, types.Monitor)
	registerGroups(t, map[string]bool{"pilot": true})
	setGroupMode(t, "pilot", types.Lockdown)

	own := machineconfiguration.GetUniversalDefaultConfig()
	own.ClientMode = types.Monitor
	if err := configService(t).SetMachineConfig(machineA, own); err != nil {
		t.Fatal(err)
	}
	a := newHost(t, machineA, transport)
	a.PrimaryUserGroups = []string{"pilot"}
	if got := sentMode(t, a.MustSync(t).Preflight); got != "MONITOR" {
		t.Errorf("the Mac's own MONITOR in a LOCKDOWN group: %s, want MONITOR", got)
	}

	// A partial machine row without ClientMode does not stop the group's mode
	putItem(t, map[string]any{"PK": "Machine#" + machineB, "SK": "Config", "DataType": "MachineConfiguration", "BatchSize": 25})
	handlers = newHandlers()
	b := newHost(t, machineB, transport)
	b.PrimaryUserGroups = []string{"pilot"}
	if got := sentMode(t, b.MustSync(t).Preflight); got != "LOCKDOWN" {
		t.Errorf("a Mac whose own row sets only BatchSize: %s, want the group's LOCKDOWN", got)
	}

	setTemporaryMode(t, machineB, types.Monitor, time.Now().Add(time.Hour))
	if got := sentMode(t, b.MustSync(t).Preflight); got != "MONITOR" {
		t.Errorf("a temporary MONITOR in a LOCKDOWN group: %s, want MONITOR", got)
	}
	setTemporaryMode(t, machineB, types.Monitor, time.Now().Add(-time.Minute))
	handlers = newHandlers()
	if got := sentMode(t, b.MustSync(t).Preflight); got != "LOCKDOWN" {
		t.Errorf("after the temporary mode: %s, want the group's LOCKDOWN back", got)
	}
}

func TestGroupMode_UngroupedMacs(t *testing.T) {
	transport := groupsServer(t)
	setGlobalClientMode(t, types.Lockdown)
	registerGroups(t, map[string]bool{"eng": true, ungrouped: false})
	setGroupMode(t, ungrouped, types.Monitor)

	h := newHost(t, machineA, transport)
	h.PrimaryUserGroups = []string{"some-directory-group"} // not registered: still ungrouped
	if got := sentMode(t, h.MustSync(t).Preflight); got != "MONITOR" {
		t.Errorf("an ungrouped Mac: %s, want the ungrouped MONITOR", got)
	}
	h.PrimaryUserGroups = []string{"eng"}
	if got := sentMode(t, h.MustSync(t).Preflight); got != "LOCKDOWN" {
		t.Errorf("after joining eng (no mode): %s, want the global LOCKDOWN, not the ungrouped mode", got)
	}
}

func TestGroupMode_GroupsThatDoNotCountAndUndecodableModes(t *testing.T) {
	transport := groupsServer(t)
	setGlobalClientMode(t, types.Monitor)
	registerGroups(t, map[string]bool{"assigned-only": false, "bad-string": true, "bad-number": true})
	setGroupMode(t, "assigned-only", types.Lockdown)
	setGroupMode(t, "bad-string", "LOCKDOWN")
	setGroupMode(t, "bad-number", 99)

	h := newHost(t, machineA, transport)
	h.PrimaryUserGroups = []string{"assigned-only", "unregistered", "bad-string", "bad-number"}
	res, err := h.Sync(t)
	if err != nil {
		t.Fatalf("sync failed: %v", err)
	}
	if got := sentMode(t, res.Preflight); got != "MONITOR" {
		t.Errorf("%s, want the global MONITOR: a reported group that accepts only assigned members does not count, and undecodable modes are ignored", got)
	}
}

// A group with a mode costs a steady-state sync no more than the existing budget (TestGroups_SteadyStateSyncStaysWithinBudget).
func TestGroupMode_SteadyStateSyncStaysWithinBudget(t *testing.T) {
	transport := groupsServer(t)
	registerGroups(t, map[string]bool{"pilot": true, "staff": true})
	setGroupMode(t, "pilot", types.Lockdown)
	assignGroups(t, machineA, "staff")
	h := newHost(t, machineA, transport)
	h.PrimaryUserGroups = []string{"pilot"}
	syncNormal(t, h)
	h.MustSync(t)
	ddblocal.ResetCounts()
	res := h.MustSync(t)
	if c := ddblocal.ConsumedCapacity(); c.WriteUnits > 4 || c.ReadUnits > 8 {
		t.Errorf("steady-state sync of a Mac in a group with a mode: %s; budget 4 WCU and 8 RCU", c)
	}
	if got := sentMode(t, res.Preflight); got != "LOCKDOWN" {
		t.Errorf("%s, want LOCKDOWN", got)
	}
}
