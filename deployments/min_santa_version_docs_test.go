package deployments

// Editable minimum Santa version (owner decision, 2026-11-25): the docs say where the setting now lives. The fleet job
// no longer reads FLEET_MIN_SANTA_VERSION; the minimum is FleetHealth / Config's MinSantaVersion, changed with
// `rudolph fleet min-santa-version`, else 2026.9. An environment that set the variable on the function has to store its
// value once after deploying, which upgrade step 32 says. No IAM change: the fleet role already has GetItem on
// FleetHealth, and CLI and web credentials that set holidays already write that item.

import (
	"regexp"
	"strings"
	"testing"
)

func TestDocs_MinSantaVersionUpgradeStep(t *testing.T) {
	doc := readDoc(t, "docs/deploy.md")
	step := markdownSection(doc, "32. Editable minimum Santa version")
	if step == "" {
		t.Fatal("docs/deploy.md has no upgrade step \"### 32. Editable minimum Santa version\"")
	}
	for _, want := range []string{"FLEET_MIN_SANTA_VERSION", "rudolph fleet min-santa-version", "2026.9", "off"} {
		if !strings.Contains(step, want) {
			t.Errorf("upgrade step 32 does not mention %s", want)
		}
	}
	if !regexp.MustCompile(`(?i)no IAM change|IAM is unchanged|no change to IAM`).MatchString(step) {
		t.Error("upgrade step 32 does not say that no IAM change is needed")
	}
}

// Outside the upgrade steps, nothing presents FLEET_MIN_SANTA_VERSION as a setting any more.
func TestDocs_MinSantaVersionIsNoLongerAFunctionSetting(t *testing.T) {
	deploy := readDoc(t, "docs/deploy.md")
	if i := strings.Index(deploy, "## Upgrading an Existing Deployment"); i >= 0 {
		deploy = deploy[:i]
	}
	for name, doc := range map[string]string{
		"docs/deploy.md (before the upgrade steps)": deploy,
		"docs/fleet-health.md":                      readDoc(t, "docs/fleet-health.md"),
		"docs/cli.md":                               readDoc(t, "docs/cli.md"),
	} {
		for _, para := range paragraphs(doc) {
			if strings.Contains(para, "FLEET_MIN_SANTA_VERSION") && !strings.Contains(para, "ignored") {
				t.Errorf("%s still presents FLEET_MIN_SANTA_VERSION as a setting (say it is ignored, or drop it):\n%s", name, para)
			}
		}
	}

	health := readDoc(t, "docs/fleet-health.md")
	var versionRow string
	for _, line := range strings.Split(health, "\n") {
		if strings.HasPrefix(line, "| `Version` |") {
			versionRow = line
		}
	}
	if !strings.Contains(versionRow, "rudolph fleet min-santa-version") || !strings.Contains(versionRow, "2026.9") {
		t.Errorf("docs/fleet-health.md's Version row does not say the minimum is 2026.9 unless changed with rudolph fleet min-santa-version:\n%s", versionRow)
	}

	cli := readDoc(t, "docs/cli.md")
	section := markdownSection(cli, "`fleet min-santa-version`")
	if section == "" {
		t.Fatal("docs/cli.md has no section \"### `fleet min-santa-version`\"")
	}
	for _, want := range []string{"--default", "off", "fleet.min-santa-version", "2026.9"} {
		if !strings.Contains(section, want) {
			t.Errorf("docs/cli.md's fleet min-santa-version section does not mention %s", want)
		}
	}
	if !strings.Contains(cli, "fleet show") {
		t.Error("docs/cli.md does not document `rudolph fleet show`")
	}
}
