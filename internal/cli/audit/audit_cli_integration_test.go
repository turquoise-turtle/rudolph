//go:build integration

// Package audit holds `rudolph audit`, the reader of Rudolph's audit trail (plan F2, v2 groundwork, 2026-11-07).
//
// Every write the CLI makes to rules, configuration, groups or machines also records who made it: an audit entry with
// the time, the actor (for the CLI "cli:<OS user>@<host name>"; rudolph-web will use "web:<Entra user>"), the action,
// the target and what changed. Entries live in the store table under Audit# partitions, read through
// pkg/model/audit.List (newest first), and never expire. A dry run or a declined prompt records nothing.
package audit

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/cli/config"
	"github.com/airbnb/rudolph/internal/cli/group"
	"github.com/airbnb/rudolph/internal/cli/rule"
	"github.com/airbnb/rudolph/internal/testutil/clitest"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	modelaudit "github.com/airbnb/rudolph/pkg/model/audit"
	"github.com/spf13/cobra"
)

func TestMain(m *testing.M) { os.Exit(ddblocal.Main(m)) }

const mac = "AAAAAAAA-1111-4111-8111-AAAAAAAAAAAA"

var sha = strings.Repeat("ab", 32)

func env() clitest.Env { return clitest.Env{Table: ddblocal.TableName, Region: ddblocal.Region} }

func run(t *testing.T, cmd *cobra.Command, stdin string, args ...string) string {
	t.Helper()
	out, err := clitest.Run(t, env(), cmd, stdin, args...)
	if err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return out
}

func entries(t *testing.T) []modelaudit.Entry {
	t.Helper()
	got, err := modelaudit.List(ddblocal.Client(t), time.Now().Add(-time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestAudit_CLIWritesAreRecordedWithTheActor(t *testing.T) {
	ddblocal.Reset(t)
	start := time.Now().Add(-time.Second)
	run(t, rule.RuleCmd, "", "deny", "-t", "binary", "-i", sha, "--global", "--yes")
	run(t, rule.RuleCmd, "", "allow", "-t", "binary", "-i", sha, "--machine", mac, "--expires", "2h", "--yes")
	run(t, config.ConfigCmd, "", "update", "--machine", mac, "-c", "lockdown", "--for", "1h", "--yes")
	run(t, group.GroupCmd, "", "create", "eng")
	run(t, rule.RuleCmd, "", "remove", "-t", "binary", "-i", sha, "--global", "--yes")

	got := entries(t)
	if len(got) != 5 {
		t.Fatalf("%d audit entries, want 5: %+v", len(got), got)
	}
	// Newest first.
	for i := 1; i < len(got); i++ {
		if got[i].Time.After(got[i-1].Time) {
			t.Errorf("entries not newest first: %v before %v", got[i-1].Time, got[i].Time)
		}
	}
	host, _ := os.Hostname()
	for _, e := range got {
		if !strings.HasPrefix(e.Actor, "cli:") || !strings.Contains(e.Actor, "@") {
			t.Errorf("actor %q, want cli:<user>@<host>", e.Actor)
		}
		if host != "" && !strings.Contains(e.Actor, host) {
			t.Errorf("actor %q does not name the host %q", e.Actor, host)
		}
		if e.Time.Before(start) || e.Time.After(time.Now().Add(time.Second)) {
			t.Errorf("entry time %v outside the test", e.Time)
		}
	}
	want := []struct{ action, target, summary string }{
		{"rule.remove", "global", sha},
		{"group.create", "group:eng", "eng"},
		{"config.temporary-mode", "machine:" + mac, "LOCKDOWN"},
		{"rule.add", "machine:" + mac, sha},
		{"rule.add", "global", sha},
	}
	for i, w := range want {
		e := got[i]
		if e.Action != w.action || e.Target != w.target || !strings.Contains(strings.ToUpper(e.Summary), strings.ToUpper(w.summary)) {
			t.Errorf("entry %d = {%s %s %q}, want {%s %s ...%s...}", i, e.Action, e.Target, e.Summary, w.action, w.target, w.summary)
		}
	}
	if !strings.Contains(got[4].Summary, "BLOCKLIST") {
		t.Errorf("rule.add summary %q does not name the policy", got[4].Summary)
	}
}

func TestAudit_DryRunAndDeclinedPromptRecordNothing(t *testing.T) {
	ddblocal.Reset(t)
	run(t, rule.RuleCmd, "", "deny", "-t", "binary", "-i", sha, "--global", "--dry-run")
	if _, err := clitest.Run(t, env(), rule.RuleCmd, "no\n", "deny", "-t", "binary", "-i", sha, "--global"); err != nil {
		t.Logf("declined prompt returned %v", err)
	}
	if got := entries(t); len(got) != 0 {
		t.Errorf("audit entries after a dry run and a declined prompt: %+v", got)
	}
}

func TestAudit_EntriesNeverExpire(t *testing.T) {
	ddblocal.Reset(t)
	run(t, group.GroupCmd, "", "create", "eng")
	got := entries(t)
	if len(got) != 1 {
		t.Fatalf("%d entries", len(got))
	}
	if got[0].ExpiresAfter != 0 {
		t.Errorf("audit entry expires (ExpiresAfter %d); audit entries are kept", got[0].ExpiresAfter)
	}
}

func TestAuditCommand_ListsNewestFirst(t *testing.T) {
	ddblocal.Reset(t)
	run(t, group.GroupCmd, "", "create", "eng")
	run(t, group.GroupCmd, "", "create", "ops")
	out := run(t, AuditCmd, "", "--since", "1h")
	if i, j := strings.Index(out, "group:ops"), strings.Index(out, "group:eng"); i < 0 || j < 0 || i > j {
		t.Errorf("rudolph audit output not newest first or missing entries:\n%s", out)
	}
	if !strings.Contains(out, "cli:") {
		t.Errorf("rudolph audit output does not show the actor:\n%s", out)
	}
	out = run(t, AuditCmd, "", "--since", "1h", "--limit", "1")
	if strings.Contains(out, "group:eng") || !strings.Contains(out, "group:ops") {
		t.Errorf("--limit 1 output:\n%s", out)
	}
	if _, err := clitest.Run(t, env(), AuditCmd, "", "--since", "soon"); err == nil {
		t.Error("--since soon accepted")
	}
}
