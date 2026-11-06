//go:build integration

package handlers

// R7b of rudolph-web's docs/data-layer-design.md ("Collecting allowed events per rule"). Santa uploads executions
// allowed by a rule only with EnableAllEventUpload, and has no per-rule switch, so Rudolph's event writer decides: an
// ALLOW_BINARY, ALLOW_CERTIFICATE, ALLOW_TEAMID, ALLOW_SIGNINGID or ALLOW_CDHASH event is stored only if the rule that
// allowed it has collection switched on. The rule is found from the decision and the event's own fields (file_sha256,
// the leaf certificate's sha256, team_id, signing_id, cdhash), among the global rules and the Mac's own machine rules.
//
//	rudolph rule update -t <type> -i <identifier> (--global | --machine <id>) --collect-allowed-events=true|false
//
// sets or clears CollectAllowedEvents on the rule row. Santa never sees it: no feed row, no change to what a sync
// downloads. The writer may cache which rules are flagged for up to 60 s. Stored allowed events count as Allowed on the
// day counter. When collection is switched on while EnableAllEventUpload is off everywhere, the CLI warns that Santa
// will not upload such events.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/cli/rule"
	"github.com/airbnb/rudolph/internal/testutil/clitest"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/pkg/types"
)

const otherTeam = "ABCDE12345"

func ruleCLI(t *testing.T, args ...string) string {
	t.Helper()
	out, err := clitest.Run(t, clitest.Env{Table: ddblocal.TableName, Region: ddblocal.Region}, rule.RuleCmd, "yes\n", args...)
	if err != nil {
		t.Fatalf("rudolph rule %v: %v\n%s", args, err, out)
	}
	// A rule change through the CLI is picked up by a fresh writer at once; a warm Lambda may take up to 60 s.
	handlers = newHandlers()
	return out
}

func storedSHAs(t *testing.T, machineID string) map[string]bool {
	t.Helper()
	got := map[string]bool{}
	for _, it := range eventsOf(t, machineID) {
		sha, _ := sensorString(t, it, "FileSHA256")
		got[sha] = true
	}
	return got
}

func TestR7b_FlaggedTeamIDCollectsItsAllowedExecutions(t *testing.T) {
	transport := eventsServer(t)
	addGlobalRule(t, teamGoogle, types.RuleTypeTeamID, types.RulePolicyAllowlist)
	addGlobalRule(t, otherTeam, types.RuleTypeTeamID, types.RulePolicyAllowlist)
	host := syncedHost(t, machineA, transport)

	// Santa is not told anything: the sync after switching collection on downloads what the sync before it did (an
	// incremental sync re-sends the last 10 minutes of the feed either way).
	before := host.MustSync(t)
	ruleCLI(t, "update", "-t", "teamid", "-i", teamGoogle, "--global", "--collect-allowed-events=true")
	if after := host.MustSync(t); after.RulesReceived != before.RulesReceived || after.SyncType != "NORMAL" {
		t.Errorf("the sync after switching collection on: %s with %d rules; the one before: %s with %d", after.SyncType, after.RulesReceived, before.SyncType, before.RulesReceived)
	}

	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	gemini := strings.Repeat("9e", 32)
	host.PendingEvents = []string{
		execEvent(gemini, "ALLOW_TEAMID", 1, at, fmt.Sprintf(`"team_id":%q,"signing_id":%q`, teamGoogle, teamGoogle+":com.google.GeminiMacOS")),
		execEvent(sha2, "ALLOW_TEAMID", 2, at, fmt.Sprintf(`"team_id":%q`, otherTeam)),
		execEvent(sha3, "ALLOW_SIGNINGID", 3, at, fmt.Sprintf(`"team_id":%q,"signing_id":%q`, teamGoogle, chromeSigningID)),
	}
	host.MustSync(t)

	got := storedSHAs(t, machineA)
	if !got[gemini] {
		t.Error("the execution allowed by the flagged Google team ID rule was not stored")
	}
	if got[sha2] {
		t.Error("an execution allowed by an unflagged team ID rule was stored")
	}
	if got[sha3] {
		t.Error("an ALLOW_SIGNINGID execution was stored although no signing ID rule is flagged")
	}
	stored := eventsItem(t, "Exec#"+machineA, tsOf(at)+"#"+gemini+"#1")
	if sid, _ := sensorString(t, stored, "SigningID"); sid != teamGoogle+":com.google.GeminiMacOS" {
		t.Errorf("stored SigningID %q: the signing ID is what the owner is looking for", sid)
	}
	if n, _ := numAttr(t, eventsItem(t, "BinaryDay#"+at.UTC().Format("2006-01-02"), gemini), "Allowed"); n != 1 {
		t.Errorf("Allowed = %v, want 1", n)
	}
}

func TestR7b_SwitchingCollectionOffStopsIt(t *testing.T) {
	transport := eventsServer(t)
	addGlobalRule(t, teamGoogle, types.RuleTypeTeamID, types.RulePolicyAllowlist)
	host := syncedHost(t, machineA, transport)
	ruleCLI(t, "update", "-t", "teamid", "-i", teamGoogle, "--global", "--collect-allowed-events=true")
	ruleCLI(t, "update", "-t", "teamid", "-i", teamGoogle, "--global", "--collect-allowed-events=false")

	host.PendingEvents = []string{execEvent(sha1, "ALLOW_TEAMID", 1, time.Now().Add(-time.Minute), fmt.Sprintf(`"team_id":%q`, teamGoogle))}
	host.MustSync(t)
	if got := storedSHAs(t, machineA); got[sha1] {
		t.Error("an allowed execution was stored after collection was switched off")
	}
}

// A machine rule can be flagged to watch one Mac only.
func TestR7b_FlaggedMachineRuleCollectsOnThatMacOnly(t *testing.T) {
	transport := eventsServer(t)
	addMachineRule(t, machineA, chromeSigningID, types.RuleTypeSigningID, types.RulePolicyAllowlist, time.Now().Add(24*time.Hour))
	addGlobalRule(t, chromeSigningID, types.RuleTypeSigningID, types.RulePolicyAllowlist)
	a := syncedHost(t, machineA, transport)
	b := syncedHost(t, machineB, transport)
	ruleCLI(t, "update", "-t", "signingid", "-i", chromeSigningID, "--machine", machineA, "--collect-allowed-events=true")

	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	event := execEvent(sha1, "ALLOW_SIGNINGID", 1, at, fmt.Sprintf(`"team_id":%q,"signing_id":%q`, chromeTeamID, chromeSigningID))
	a.PendingEvents = []string{event}
	a.MustSync(t)
	b.PendingEvents = []string{event}
	b.MustSync(t)
	if !storedSHAs(t, machineA)[sha1] {
		t.Error("the Mac whose machine rule is flagged did not have its allowed execution stored")
	}
	if storedSHAs(t, machineB)[sha1] {
		t.Error("another Mac, allowed by the unflagged global rule, had its execution stored")
	}
}

func TestR7b_EveryRuleTypeCanBeFlagged(t *testing.T) {
	transport := eventsServer(t)
	cert := strings.Repeat("ce", 32)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyAllowlist)
	addGlobalRule(t, cdhash, types.RuleTypeCDHash, types.RulePolicyAllowlist)
	addGlobalRule(t, cert, types.RuleTypeCertificate, types.RulePolicyAllowlist)
	host := syncedHost(t, machineA, transport)
	ruleCLI(t, "update", "-t", "binary", "-i", sha1, "--global", "--collect-allowed-events=true")
	ruleCLI(t, "update", "-t", "cdhash", "-i", cdhash, "--global", "--collect-allowed-events=true")
	ruleCLI(t, "update", "-t", "certificate", "-i", cert, "--global", "--collect-allowed-events=true")

	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	host.PendingEvents = []string{
		execEvent(sha1, "ALLOW_BINARY", 1, at, ""),
		execEvent(sha2, "ALLOW_CDHASH", 2, at, fmt.Sprintf(`"cdhash":%q`, cdhash)),
		execEvent(sha3, "ALLOW_CERTIFICATE", 3, at, fmt.Sprintf(`"signing_chain":[{"sha256":%q,"cn":"Developer ID Application: Example"}]`, cert)),
	}
	host.MustSync(t)
	got := storedSHAs(t, machineA)
	for name, sha := range map[string]string{"binary": sha1, "cdhash": sha2, "certificate": sha3} {
		if !got[sha] {
			t.Errorf("the execution allowed by the flagged %s rule was not stored", name)
		}
	}
}

func TestR7b_CLIWarnsWhenSantaDoesNotUploadAllowedEvents(t *testing.T) {
	eventsServer(t)
	addGlobalRule(t, teamGoogle, types.RuleTypeTeamID, types.RulePolicyAllowlist)
	out := ruleCLI(t, "update", "-t", "teamid", "-i", teamGoogle, "--global", "--collect-allowed-events=true")
	if !strings.Contains(strings.ToLower(out), "enable-all-event-upload") && !strings.Contains(out, "EnableAllEventUpload") {
		t.Errorf("no warning that EnableAllEventUpload is off everywhere:\n%s", out)
	}
}
