//go:build integration

package handlers

// R7b for group rules (owner approved 2026-11-23). A group rule can be flagged like a global or machine rule:
//
//	rudolph rule update -t <type> -i <identifier> (--group <name> | --ungrouped) --collect-allowed-events=true|false
//
// and an allowed-by-rule execution whose rule is a flagged group rule is stored when the group is one of the uploading
// Mac's effective groups (assigned, reported where accepted, or rudolph:ungrouped), no other of its groups blocks the
// key, and no machine rule of that Mac decides the key. Santa is not told anything: no change to what a sync downloads.
// The details (precedence, reads, failures) are pinned in internal/handlers/eventupload/group_collection_test.go; the
// CLI in internal/cli/group/group_collect_cli_integration_test.go.

import (
	"fmt"
	"testing"
	"time"

	"github.com/airbnb/rudolph/pkg/types"
)

const machineC = "CCCCCCCC-3333-4333-8333-CCCCCCCCCCCC"

func otherTeamEvent(pid int) string {
	return execEvent(fmt.Sprintf("%064x", 0x7b0000+pid), "ALLOW_TEAMID", pid, time.Now().Add(-time.Hour).Truncate(time.Millisecond), fmt.Sprintf(`"team_id":%q`, otherTeam))
}

func otherTeamSHA(pid int) string { return fmt.Sprintf("%064x", 0x7b0000+pid) }

func TestR7bGroup_FlaggedGroupRuleCollectsOnItsMembersOnly(t *testing.T) {
	transport := eventsServer(t)
	addGlobalRule(t, otherTeam, types.RuleTypeTeamID, types.RulePolicyAllowlist)
	registerGroups(t, map[string]bool{"eng": false})
	putGroupRule(t, "eng", otherTeam, types.RuleTypeTeamID, types.RulePolicyAllowlist, "")
	assignGroups(t, machineA, "eng")
	a := syncedHost(t, machineA, transport)
	b := syncedHost(t, machineB, transport)

	before := a.MustSync(t)
	ruleCLI(t, "update", "-t", "teamid", "-i", otherTeam, "--group", "eng", "--collect-allowed-events=true")
	if after := a.MustSync(t); after.RulesReceived != before.RulesReceived || after.SyncType != "NORMAL" {
		t.Errorf("the sync after flagging the group rule: %s with %d rules; the one before: %s with %d", after.SyncType, after.RulesReceived, before.SyncType, before.RulesReceived)
	}

	a.PendingEvents = []string{otherTeamEvent(1)}
	a.MustSync(t)
	b.PendingEvents = []string{otherTeamEvent(2)}
	b.MustSync(t)
	if !storedSHAs(t, machineA)[otherTeamSHA(1)] {
		t.Error("the execution allowed by the Mac's group's flagged rule was not stored")
	}
	if storedSHAs(t, machineB)[otherTeamSHA(2)] {
		t.Error("a Mac in no group, allowed by the unflagged global rule, had its execution stored")
	}

	ruleCLI(t, "update", "-t", "teamid", "-i", otherTeam, "--group", "eng", "--collect-allowed-events=false")
	a.PendingEvents = []string{otherTeamEvent(3)}
	a.MustSync(t)
	if storedSHAs(t, machineA)[otherTeamSHA(3)] {
		t.Error("an execution was stored after collection was switched off for the group rule")
	}
}

func TestR7bGroup_UngroupedRuleCollectsOnMacsInNoGroup(t *testing.T) {
	transport := eventsServer(t)
	registerGroups(t, map[string]bool{"eng": false, ungrouped: false})
	putGroupRule(t, ungrouped, otherTeam, types.RuleTypeTeamID, types.RulePolicyAllowlist, "")
	putGroupRule(t, "eng", otherTeam, types.RuleTypeTeamID, types.RulePolicyAllowlist, "")
	assignGroups(t, machineA, "eng")
	a := syncedHost(t, machineA, transport)
	b := syncedHost(t, machineB, transport)
	ruleCLI(t, "update", "-t", "teamid", "-i", otherTeam, "--ungrouped", "--collect-allowed-events=true")

	a.PendingEvents = []string{otherTeamEvent(1)}
	a.MustSync(t)
	b.PendingEvents = []string{otherTeamEvent(2)}
	b.MustSync(t)
	if storedSHAs(t, machineA)[otherTeamSHA(1)] {
		t.Error("a Mac in a group had its execution stored through the ungrouped rule's flag")
	}
	if !storedSHAs(t, machineB)[otherTeamSHA(2)] {
		t.Error("a Mac in no group did not have its execution stored through the ungrouped rule's flag")
	}
}

// A block in another of the Mac's groups beats the flagged group allow; a machine rule of the Mac beats any group rule.
func TestR7bGroup_HigherPrecedenceRulesDecide(t *testing.T) {
	transport := eventsServer(t)
	registerGroups(t, map[string]bool{"eng": false, "lab": false})
	putGroupRule(t, "eng", otherTeam, types.RuleTypeTeamID, types.RulePolicyAllowlist, "")
	putGroupRule(t, "lab", otherTeam, types.RuleTypeTeamID, types.RulePolicyBlocklist, "")
	assignGroups(t, machineA, "eng", "lab")
	assignGroups(t, machineB, "eng")
	assignGroups(t, machineC, "eng")
	addMachineRule(t, machineB, otherTeam, types.RuleTypeTeamID, types.RulePolicyAllowlist, time.Now().Add(24*time.Hour))
	a := syncedHost(t, machineA, transport)
	b := syncedHost(t, machineB, transport)
	c := syncedHost(t, machineC, transport)
	ruleCLI(t, "update", "-t", "teamid", "-i", otherTeam, "--group", "eng", "--collect-allowed-events=true")

	a.PendingEvents = []string{otherTeamEvent(1)}
	a.MustSync(t)
	b.PendingEvents = []string{otherTeamEvent(2)}
	b.MustSync(t)
	c.PendingEvents = []string{otherTeamEvent(3)}
	c.MustSync(t)
	if storedSHAs(t, machineA)[otherTeamSHA(1)] {
		t.Error("stored through eng's flag although lab, another of the Mac's groups, blocks the team ID")
	}
	if storedSHAs(t, machineB)[otherTeamSHA(2)] {
		t.Error("stored through eng's flag although the Mac's own (unflagged) machine rule decides the team ID")
	}
	if !storedSHAs(t, machineC)[otherTeamSHA(3)] {
		t.Error("not stored for a Mac whose only group allows the team ID with a flagged rule")
	}
}

func TestR7bGroup_RemovingTheGroupRuleEndsCollection(t *testing.T) {
	transport := eventsServer(t)
	registerGroups(t, map[string]bool{"eng": false})
	putGroupRule(t, "eng", otherTeam, types.RuleTypeTeamID, types.RulePolicyAllowlist, "")
	assignGroups(t, machineA, "eng")
	a := syncedHost(t, machineA, transport)
	ruleCLI(t, "update", "-t", "teamid", "-i", otherTeam, "--group", "eng", "--collect-allowed-events=true")
	ruleCLI(t, "remove", "-t", "teamid", "-i", otherTeam, "--group", "eng")
	ruleCLI(t, "allow", "-t", "teamid", "-i", otherTeam, "--group", "eng")

	a.PendingEvents = []string{otherTeamEvent(1)}
	a.MustSync(t)
	if storedSHAs(t, machineA)[otherTeamSHA(1)] {
		t.Error("collection resumed for a group rule that was removed and added again without the flag")
	}
	if set := getItem(t, "EventCollection", "Rules")["Group#eng"]; set != nil {
		t.Errorf("the removed group rule is still in the index: %v", set)
	}
}
