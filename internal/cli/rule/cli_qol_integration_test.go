//go:build integration

package rule

// Quality-of-life gaps of the rule commands from the plan review of 2026-11-06 (plan C.1, C.2, C.3, C.5). They fail
// until the CLI batch lands:
//
//   - A write that is not confirmed is an error, whether the answer is "no" or stdin is empty, so a script can tell
//     "declined" from "applied"; --yes writes without a prompt.
//   - Creating a rule needs exactly one of --filepath and --identifier, checked before anything is printed.
//   - `rule update -p remove` is refused and points at `rule remove`: a REMOVE policy on a rule row never deletes it.
//   - `rule remove` names the rule it removed, and a rule it cannot find, with its identifier (and machine).

import (
	"strings"
	"testing"

	"github.com/airbnb/rudolph/internal/testutil/clitest"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	"github.com/airbnb/rudolph/pkg/types"
)

var qolSHA = strings.Repeat("cd", 32)

func qolRun(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	return clitest.Run(t, env, RuleCmd, stdin, args...)
}

func qolGlobalPolicy(t *testing.T) (types.Policy, bool) {
	t.Helper()
	row, err := globalrules.GetGlobalRuleByIdentifier(ddblocal.Client(t), qolSHA, types.RuleTypeBinary)
	if err != nil {
		t.Fatal(err)
	}
	if row == nil {
		return 0, false
	}
	return row.Policy, true
}

func TestQOL_UnconfirmedWritesAreErrors(t *testing.T) {
	for name, stdin := range map[string]string{"answered no": "no\n", "empty stdin": ""} {
		t.Run(name, func(t *testing.T) {
			ddblocal.Reset(t)
			out, err := qolRun(t, stdin, "allow", "-t", "binary", "-i", qolSHA, "--global")
			if err == nil {
				t.Errorf("allow without a confirmation returned no error:\n%s", out)
			}
			if _, present := qolGlobalPolicy(t); present {
				t.Error("the rule was written without a confirmation")
			}
		})
	}
	t.Run("remove answered no", func(t *testing.T) {
		ddblocal.Reset(t)
		if out, err := qolRun(t, "yes\n", "allow", "-t", "binary", "-i", qolSHA, "--global"); err != nil {
			t.Fatalf("allow: %v\n%s", err, out)
		}
		out, err := qolRun(t, "no\n", "remove", "-t", "binary", "-i", qolSHA, "--global")
		if err == nil {
			t.Errorf("remove without a confirmation returned no error:\n%s", out)
		}
		if _, present := qolGlobalPolicy(t); !present {
			t.Error("the rule was removed without a confirmation")
		}
	})
}

func TestQOL_YesFlagWritesWithoutAPrompt(t *testing.T) {
	ddblocal.Reset(t)
	out, err := qolRun(t, "", "allow", "-t", "binary", "-i", qolSHA, "--global", "--yes")
	if err != nil {
		t.Fatalf("allow --yes with no stdin: %v\n%s", err, out)
	}
	if strings.Contains(out, "Apply changes?") {
		t.Errorf("--yes still asked for a confirmation:\n%s", out)
	}
	if _, present := qolGlobalPolicy(t); !present {
		t.Error("allow --yes wrote nothing")
	}
}

func TestQOL_CreateNeedsExactlyOneOfFilepathAndIdentifier(t *testing.T) {
	for name, args := range map[string][]string{
		"allow without either":    {"allow", "-t", "binary", "--global"},
		"deny without either":     {"deny", "-t", "teamid", "--global"},
		"silent without either":   {"silent", "-t", "signingid", "--global"},
		"compiler without either": {"compiler", "-t", "binary", "--global"},
		"both given":              {"allow", "-t", "binary", "-i", qolSHA, "-f", "/usr/bin/true", "--global"},
	} {
		t.Run(name, func(t *testing.T) {
			ddblocal.Reset(t)
			out, err := qolRun(t, "yes\n", args...)
			if err == nil {
				t.Fatalf("no error:\n%s", out)
			}
			if msg := err.Error(); !strings.Contains(msg, "--identifier") && !strings.Contains(msg, "--filepath") {
				t.Errorf("error %q does not say which of --filepath and --identifier is missing or duplicated", msg)
			}
			if strings.Contains(out, "Apply changes?") {
				t.Errorf("the confirmation prompt was shown before the check:\n%s", out)
			}
			if n := countItems(t); n != 0 {
				t.Errorf("%d items written", n)
			}
		})
	}
}

func TestQOL_UpdateToRemoveIsRefused(t *testing.T) {
	ddblocal.Reset(t)
	if out, err := qolRun(t, "yes\n", "allow", "-t", "binary", "-i", qolSHA, "--global"); err != nil {
		t.Fatalf("allow: %v\n%s", err, out)
	}
	out, err := qolRun(t, "yes\n", "update", "-t", "binary", "-i", qolSHA, "--global", "-p", "remove")
	if err == nil || !strings.Contains(err.Error(), "rule remove") {
		t.Errorf("update -p remove: err %v, want a refusal pointing at rule remove\n%s", err, out)
	}
	if policy, present := qolGlobalPolicy(t); !present || policy != types.RulePolicyAllowlist {
		t.Errorf("rule policy is now %v (present %v), want the ALLOWLIST rule untouched", policy, present)
	}
}

func TestQOL_RemoveNamesTheRuleItRemovedOrCouldNotFind(t *testing.T) {
	ddblocal.Reset(t)
	out, err := qolRun(t, "yes\n", "remove", "-t", "binary", "-i", qolSHA, "--global")
	if err == nil || !strings.Contains(err.Error(), qolSHA) {
		t.Errorf("removing a missing global rule: err %v, want an error naming %s\n%s", err, qolSHA, out)
	}
	out, err = qolRun(t, "yes\n", "remove", "-t", "binary", "-i", qolSHA, "-m", machine)
	if err == nil || !strings.Contains(err.Error(), qolSHA) || !strings.Contains(err.Error(), machine) {
		t.Errorf("removing a missing machine rule: err %v, want an error naming the rule and the machine\n%s", err, out)
	}

	if out, err := qolRun(t, "yes\n", "allow", "-t", "binary", "-i", qolSHA, "-m", machine); err != nil {
		t.Fatalf("allow for a machine: %v\n%s", err, out)
	}
	out, err = qolRun(t, "yes\n", "remove", "-t", "binary", "-i", qolSHA, "-m", machine)
	if err != nil {
		t.Fatalf("remove a machine rule: %v\n%s", err, out)
	}
	if !strings.Contains(out, qolSHA) {
		t.Errorf("removing a machine rule printed nothing that names the rule:\n%s", out)
	}
}
