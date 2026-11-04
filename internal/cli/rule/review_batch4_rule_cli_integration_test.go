//go:build integration

package rule

// Regression tests for the independent review of batch 4's CLI fixes (B7).

import (
	"strings"
	"testing"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func policyOf(item map[string]awstypes.AttributeValue) string {
	if n, ok := item["Policy"].(*awstypes.AttributeValueMemberN); ok {
		return n.Value
	}
	return ""
}

// Review findings 1 and 2: `rule update --machine` must work (it used to panic), and updating a machine rule's policy
// must keep its expiry: a permanent rule stays permanent, a rule expiring in 48 hours keeps that expiry.
func TestReview_RuleUpdateOfAMachineRuleKeepsItsExpiry(t *testing.T) {
	for _, tc := range []struct {
		name   string
		create []string
	}{
		{"permanent", []string{"--permanent"}},
		{"expiring", []string{"--expires", "48h"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ddblocal.Reset(t)
			if out, err := run(t, append([]string{"allow", "-t", "binary", "-i", sha, "--machine", machine, "--force"}, tc.create...)...); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			before := machineRule(t)
			beforeExpiry, beforeHas := expiresAfter(t, before)

			if out, err := run(t, "update", "-t", "binary", "-i", sha, "-p", "blocklist", "--machine", machine, "--force"); err != nil {
				t.Fatalf("rule update --machine failed: %v\n%s", err, out)
			}
			after := machineRule(t)
			if policyOf(after) == policyOf(before) {
				t.Fatalf("the policy was not updated: %v", after)
			}
			afterExpiry, afterHas := expiresAfter(t, after)
			if afterHas != beforeHas || afterExpiry != beforeExpiry {
				t.Fatalf("updating the policy changed the expiry: before %d (set %v), after %d (set %v)", beforeExpiry, beforeHas, afterExpiry, afterHas)
			}
		})
	}
}

// Review finding 6: machine IDs are compared exactly (the web view contract treats another case as another machine),
// so a machine ID the CLI is given must be a UUID and must address the same machine however it is typed.
func TestReview_RuleCommandsNormaliseOrRefuseMachineIDs(t *testing.T) {
	ddblocal.Reset(t)
	if _, err := run(t, "allow", "-t", "binary", "-i", sha, "--machine", "not-a-uuid", "--force"); err == nil {
		t.Error("a machine ID that is not a UUID must be refused")
	}
	out, err := run(t, "allow", "-t", "binary", "-i", sha, "--machine", strings.ToLower(machine), "--force")
	if err == nil && len(machineRule(t)) == 0 {
		t.Errorf("a lower-case machine ID was accepted but stored under another key than the Mac's own ID:\n%s", out)
	}
}
