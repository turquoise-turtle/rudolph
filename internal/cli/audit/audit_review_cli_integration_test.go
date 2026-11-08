//go:build integration

package audit

// From the review of feat/audit-trail: a write that changes something and then fails part-way is still recorded (the
// change happened), and a command that writes nothing records nothing.

import (
	"strings"
	"testing"

	"github.com/airbnb/rudolph/internal/cli/config"
	"github.com/airbnb/rudolph/internal/cli/group"
	"github.com/airbnb/rudolph/internal/cli/machine"
	"github.com/airbnb/rudolph/internal/cli/repair"
	"github.com/airbnb/rudolph/internal/cli/rule"
	"github.com/airbnb/rudolph/internal/cli/rules"
	"github.com/airbnb/rudolph/internal/testutil/clitest"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
)

func TestAudit_ForgetThatFailsPartWayIsRecorded(t *testing.T) {
	ddblocal.Reset(t)
	run(t, group.GroupCmd, "", "create", "eng")
	run(t, config.ConfigCmd, "", "update", "--machine", mac, "-c", "lockdown", "--for", "1h", "--yes")
	run(t, rule.RuleCmd, "", "allow", "-t", "binary", "-i", sha, "--machine", mac, "--expires", "2h", "--yes")
	run(t, group.GroupCmd, "", "add", "-m", mac, "eng")
	before := len(entries(t))

	ddblocal.FailNext(t, "BatchWriteItem", 50, ddblocal.FaultAccessDenied)
	if out, err := clitest.Run(t, env(), machine.MachineCmd, "", "forget", "-m", mac, "--yes"); err == nil {
		t.Fatalf("forget succeeded although every batch delete was refused:\n%s", out)
	}
	got := entries(t)
	if len(got) != before+1 || got[0].Action != "machine.forget" || got[0].Target != "machine:"+mac {
		t.Fatalf("after a forget that removed the group assignment and then failed: %d entries, newest %+v; want a machine.forget entry", len(got), got[0])
	}
	if !strings.Contains(strings.ToLower(got[0].Summary), "fail") && !strings.Contains(strings.ToLower(got[0].Summary), "partial") {
		t.Errorf("summary %q does not say the forget failed part-way", got[0].Summary)
	}
}

func TestAudit_CommandsThatWriteNothingRecordNothing(t *testing.T) {
	ddblocal.Reset(t)
	if out, err := clitest.Run(t, env(), repair.RepairCmd, "", "--delete", "--yes"); err != nil {
		t.Fatalf("repair: %v\n%s", err, out)
	}
	if out, err := clitest.Run(t, env(), rules.RulesCmd, "", "backfill-policy-index"); err != nil {
		t.Fatalf("backfill: %v\n%s", err, out)
	}
	if got := entries(t); len(got) != 0 {
		t.Errorf("entries for commands that changed nothing: %+v", got)
	}
}

func TestAuditCommand_RefusesASinceBeyondTheLookback(t *testing.T) {
	ddblocal.Reset(t)
	out, err := clitest.Run(t, env(), AuditCmd, "", "--since", "3650d")
	if err == nil {
		t.Fatalf("--since 3650d accepted:\n%s", out)
	}
	if !strings.Contains(err.Error()+out, "days") {
		t.Errorf("error does not say how far back the audit can be listed: %v\n%s", err, out)
	}
}
