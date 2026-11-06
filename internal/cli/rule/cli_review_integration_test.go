//go:build integration

package rule

// From the review of the CLI batch: a machine rule that is being removed (Rudolph tells the Mac to drop it at its next
// sync) is not a live rule. `rule update` refuses it instead of silently bringing it back.

import (
	"testing"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
)

func TestReviewQOL_UpdateRefusesAMachineRuleBeingRemoved(t *testing.T) {
	setupRemovingMachineRule(t)
	before := machineRule(t)
	if out, err := run(t, "update", "-t", "binary", "-i", sha, "--machine", machine, "--expires", "48h", "--yes"); err == nil {
		t.Errorf("updating a machine rule being removed succeeded:\n%s", out)
	}
	if after := machineRule(t); policyOf(after) != policyOf(before) {
		t.Errorf("the refused update changed the row's policy from %s to %s", policyOf(before), policyOf(after))
	}
}

func setupRemovingMachineRule(t *testing.T) {
	t.Helper()
	ddblocal.Reset(t)
	if out, err := run(t, "allow", "-t", "binary", "-i", sha, "--machine", machine, "--permanent", "--yes"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err := run(t, "remove", "-t", "binary", "-i", sha, "--machine", machine, "--yes"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(machineRule(t)) == 0 {
		t.Skip("the removal deleted the row at once; nothing is being removed")
	}
}

// From the --dry-run review: a dry run fails where the real run would, so a script checking a pasted command learns
// that the rule it wants to remove is not there.
func TestReviewQOL_DryRunOfRemovingAMissingRuleFails(t *testing.T) {
	ddblocal.Reset(t)
	for name, args := range map[string][]string{
		"global":  {"remove", "-t", "binary", "-i", sha, "--global", "--dry-run"},
		"machine": {"remove", "-t", "binary", "-i", sha, "--machine", machine, "--dry-run"},
	} {
		if out, err := run(t, args...); err == nil {
			t.Errorf("%s: a dry run of removing a missing rule succeeded:\n%s", name, out)
		}
	}
}
