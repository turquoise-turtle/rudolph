package deployments

// Rules hash outliers after a global rule change: the fleet job reads the newest row of the rules feed (`RulesFeed`)
// and only compares Macs whose sync completed more than 10 minutes after it, so the Macs that picked up a change are
// not listed against a majority that has not synced since. The fleet role gains Query on `RulesFeed`, which an
// existing deployment gets from the Terraform apply of upgrade step 33.

import (
	"regexp"
	"strings"
	"testing"
)

func TestDocs_OutlierAfterRuleChangeUpgradeStep(t *testing.T) {
	doc := readDoc(t, "docs/deploy.md")
	heading := regexp.MustCompile(`\n### (33\. [^\n]+)\n`).FindStringSubmatch(doc)
	if heading == nil {
		t.Fatal("docs/deploy.md has no upgrade step \"### 33. ...\" for the fleet role's new RulesFeed permission")
	}
	step := markdownSection(doc, heading[1])
	for _, want := range []string{"RulesFeed", "make deploy"} {
		if !strings.Contains(step, want) {
			t.Errorf("upgrade step 33 does not mention %s", want)
		}
	}
	if !regexp.MustCompile(`(?i)fleet[^\n]*role|aws_iam_role_policy\.fleet`).MatchString(step) {
		t.Error("upgrade step 33 does not say that the fleet job's role policy changes")
	}
	if !regexp.MustCompile(`(?i)outlier`).MatchString(step) {
		t.Error("upgrade step 33 does not say what changes for rules hash outliers")
	}
}

func TestDocs_OutliersOnlyCompareMacsSyncedSinceTheLastRuleChange(t *testing.T) {
	health := readDoc(t, "docs/fleet-health.md")
	section := markdownSection(health, "Rules hash outliers")
	if section == "" {
		t.Fatal("docs/fleet-health.md has no section \"### Rules hash outliers\"")
	}
	if strings.Contains(section, "earliest postflight") {
		t.Error("docs/fleet-health.md still describes the old rule (more than 10 minutes after the earliest postflight that produced the majority hash)")
	}
	for _, want := range []string{"RulesFeed", "10 minutes"} {
		if !strings.Contains(section, want) {
			t.Errorf("docs/fleet-health.md's Rules hash outliers section does not mention %s (only Macs whose sync completed more than 10 minutes after the newest feed row are compared)", want)
		}
	}
	if !regexp.MustCompile(`(?i)no (rules )?(feed|RulesFeed) row|no row in the (rules )?feed|feed is empty|empty feed|feed has no rows?|without a feed row`).MatchString(section) {
		t.Error("docs/fleet-health.md's Rules hash outliers section does not say what happens when the rules feed has no row")
	}

	var row string
	for _, line := range strings.Split(health, "\n") {
		if strings.HasPrefix(line, "| store table | `Query` |") {
			row = line
		}
	}
	for _, want := range []string{"`Machine#*`", "`GlobalRules`", "`RulesFeed`"} {
		if !strings.Contains(row, want) {
			t.Errorf("docs/fleet-health.md's Permissions row for store table Query does not list %s:\n%s", want, row)
		}
	}
}
