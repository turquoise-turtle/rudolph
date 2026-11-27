package deployments

// Automatic clean syncs for rules hash outliers: Santa's rules hash depends on the order the Mac wrote its rules, so
// Macs holding the same rules can report different hashes. The fleet job requests a clean sync, once per rule change,
// for a Mac its outlier check finds, and lists it only when its hash still differs after a clean sync
// (AfterCleanSync). The fleet role gains UpdateItem on `Machine#*`, limited to the request's attributes, and PutItem on
// `Audit#*`, which an existing deployment gets from `make deploy` in upgrade step 34.

import (
	"regexp"
	"strings"
	"testing"
)

func TestDocs_AutoCleanSyncUpgradeStep(t *testing.T) {
	doc := readDoc(t, "docs/deploy.md")
	heading := regexp.MustCompile(`\n### (34\. [^\n]+)\n`).FindStringSubmatch(doc)
	if heading == nil {
		t.Fatal("docs/deploy.md has no upgrade step \"### 34. ...\" for automatic clean syncs for rules hash outliers")
	}
	if !regexp.MustCompile(`(?i)automatic clean syncs? for rules hash outliers`).MatchString(heading[1]) {
		t.Errorf("upgrade step 34 is %q, want \"34. Automatic clean syncs for rules hash outliers\"", heading[1])
	}
	step := markdownSection(doc, heading[1])
	for _, want := range []string{"make deploy", "100", "UpdateItem"} {
		if !strings.Contains(step, want) {
			t.Errorf("upgrade step 34 does not mention %s", want)
		}
	}
	if !regexp.MustCompile(`(?i)fleet[^\n]*role|aws_iam_role_policy\.fleet`).MatchString(step) {
		t.Error("upgrade step 34 does not say that the fleet job's role policy changes")
	}
	if !regexp.MustCompile(`(?i)first run`).MatchString(step) {
		t.Error("upgrade step 34 does not say that the first run after deploying may request clean syncs for the Macs listed today")
	}
	if !strings.Contains(step, "rudolph fleet auto-clean-sync off") {
		t.Error("upgrade step 34 does not say how to turn automatic clean syncs off (rudolph fleet auto-clean-sync off)")
	}
}

func TestDocs_FleetHealthExplainsAutoCleanSync(t *testing.T) {
	health := readDoc(t, "docs/fleet-health.md")
	section := markdownSection(health, "Rules hash outliers")
	if section == "" {
		t.Fatal("docs/fleet-health.md has no section \"### Rules hash outliers\"")
	}
	// The section and whatever follows it about automatic clean syncs: the explanation may be its own section.
	text := section
	for _, at := range regexp.MustCompile(`\n#{3,4} [^\n]*(?i:clean sync)[^\n]*\n`).FindAllStringIndex(health, -1) {
		rest := health[at[0]+1:]
		if end := regexp.MustCompile(`\n#{1,4} `).FindStringIndex(rest[1:]); end != nil {
			rest = rest[:end[0]+1]
		}
		text += rest
	}

	if !regexp.MustCompile(`(?i)\border\b[^.]*\b(wr[io]te|written|insert\w*|stored)\b|\b(insertion|write) order\b`).MatchString(text) {
		t.Error("docs/fleet-health.md does not explain that Santa's rules hash depends on the order the Mac wrote its rules")
	}
	if !regexp.MustCompile(`(?i)same rules[^.]*different hash|different hash[^.]*same rules`).MatchString(text) {
		t.Error("docs/fleet-health.md does not say that Macs holding the same rules can report different hashes")
	}
	if !strings.Contains(text, "AfterCleanSync") {
		t.Error("docs/fleet-health.md does not explain AfterCleanSync (listed after a clean sync and still different: a real problem)")
	}
	if !regexp.MustCompile(`(?i)once (per|for each|a) rule change|one (automatic )?clean sync (per|for each) rule change`).MatchString(text) {
		t.Error("docs/fleet-health.md does not say a Mac gets at most one automatic clean sync per rule change")
	}
	if !strings.Contains(text, "100") {
		t.Error("docs/fleet-health.md does not mention the cap of 100 clean sync requests a run")
	}
	if !strings.Contains(text, "rudolph fleet auto-clean-sync off") {
		t.Error("docs/fleet-health.md does not mention the off switch, rudolph fleet auto-clean-sync off")
	}

	rows := map[string]string{}
	for _, line := range strings.Split(health, "\n") {
		for _, prefix := range []string{"| store table | `UpdateItem` |", "| store table | `PutItem` |"} {
			if strings.HasPrefix(line, prefix) {
				rows[prefix] = line
			}
		}
	}
	if row := rows["| store table | `UpdateItem` |"]; !strings.Contains(row, "`Machine#*`") {
		t.Errorf("docs/fleet-health.md's Permissions table has no store table UpdateItem row on `Machine#*`: %q", row)
	}
	if row := rows["| store table | `PutItem` |"]; !strings.Contains(row, "`Audit#*`") {
		t.Errorf("docs/fleet-health.md's Permissions table has no store table PutItem row on `Audit#*`: %q", row)
	}
}

func TestDocs_CLIDescribesAutoCleanSync(t *testing.T) {
	cli := readDoc(t, "docs/cli.md")
	for _, want := range []string{"rudolph fleet auto-clean-sync", "requested by the fleet job"} {
		if !strings.Contains(cli, want) {
			t.Errorf("docs/cli.md does not mention %q", want)
		}
	}
	if !strings.Contains(readDoc(t, "docs/sync.md"), "fleet job") {
		t.Error("docs/sync.md does not say that the fleet job also requests clean syncs")
	}
}
