//go:build integration

package group

// The device groups CLI (docs/groups-design.md). Rows as described in internal/handlers/groups_integration_test.go:
// registry `GroupRegistry`/`Current` (Groups map: Description, AcceptReported, RulesVersion, CreatedAt;
// MembershipVersion), group rules `GroupRules#<group>`/<rule sort key>, assigned membership `Machine#<id>`/`Groups`
// (Assigned string set).
//
//	rudolph group create <name> [--description <text>] [--no-reported]
//	rudolph group update <name> [--description <text>] [--no-reported | --accept-reported]
//	rudolph group delete <name> [--force]          (refused while the group has rules, unless --force)
//	rudolph group list
//	rudolph group add|remove --machine <id> <name>
//	rudolph group show --machine <id>              (assigned, reported, effective groups; "ungrouped" when none)
//	rudolph rule allow|deny|… … --group <name>     (a third target besides --global and --machine; also --ungrouped)
//
// Names are trimmed and lower-cased; empty names, control characters, more than 128 bytes and the "rudolph:" prefix
// are refused. At most 64 groups. A rule for a group that is not registered is refused, and --expires/--permanent are
// refused for group rules.

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/airbnb/rudolph/internal/cli/rule"
	"github.com/airbnb/rudolph/internal/testutil/clitest"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestMain(m *testing.M) { os.Exit(ddblocal.Main(m)) }

const machine = "AAAAAAAA-1111-4111-8111-AAAAAAAAAAAA"

var (
	env = clitest.Env{Table: ddblocal.TableName, Region: ddblocal.Region}
	sha = strings.Repeat("ab", 32)
)

func group(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return clitest.Run(t, env, GroupCmd, "yes\n", args...)
}

func mustGroup(t *testing.T, args ...string) string {
	t.Helper()
	out, err := group(t, args...)
	if err != nil {
		t.Fatalf("group %v: %v\n%s", args, err, out)
	}
	return out
}

func ruleCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return clitest.Run(t, env, rule.RuleCmd, "yes\n", args...)
}

func item(t *testing.T, pk, sk string) map[string]awstypes.AttributeValue {
	t.Helper()
	out, err := ddblocal.Raw(t).GetItem(context.Background(), &awsdynamodb.GetItemInput{
		TableName:      aws.String(ddblocal.TableName),
		Key:            map[string]awstypes.AttributeValue{"PK": &awstypes.AttributeValueMemberS{Value: pk}, "SK": &awstypes.AttributeValueMemberS{Value: sk}},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.Item
}

// registryEntry returns the group's entry in the registry, or nil.
func registryEntry(t *testing.T, name string) map[string]awstypes.AttributeValue {
	t.Helper()
	groups, ok := item(t, "GroupRegistry", "Current")["Groups"].(*awstypes.AttributeValueMemberM)
	if !ok {
		return nil
	}
	e, ok := groups.Value[name].(*awstypes.AttributeValueMemberM)
	if !ok {
		return nil
	}
	return e.Value
}

func rulesVersion(t *testing.T, name string) int {
	t.Helper()
	n, _ := registryEntry(t, name)["RulesVersion"].(*awstypes.AttributeValueMemberN)
	if n == nil {
		return -1
	}
	v, _ := strconv.Atoi(n.Value)
	return v
}

func TestGroupCLI_CreateNormalisesAndRecords(t *testing.T) {
	ddblocal.Reset(t)
	mustGroup(t, "create", "  Eng ", "--description", "Engineering")
	e := registryEntry(t, "eng")
	if e == nil {
		t.Fatal("group eng not in the registry")
	}
	if b, _ := e["AcceptReported"].(*awstypes.AttributeValueMemberBOOL); b == nil || !b.Value {
		t.Errorf("AcceptReported %v, want true by default", e["AcceptReported"])
	}
	if d, _ := e["Description"].(*awstypes.AttributeValueMemberS); d == nil || d.Value != "Engineering" {
		t.Errorf("Description %v", e["Description"])
	}
	mustGroup(t, "create", "admins", "--no-reported")
	if b, _ := registryEntry(t, "admins")["AcceptReported"].(*awstypes.AttributeValueMemberBOOL); b == nil || b.Value {
		t.Errorf("admins AcceptReported %v, want false", registryEntry(t, "admins")["AcceptReported"])
	}
	out := mustGroup(t, "list")
	if !strings.Contains(out, "eng") || !strings.Contains(out, "admins") {
		t.Errorf("group list:\n%s", out)
	}
	if _, err := group(t, "create", "ENG"); err == nil {
		t.Error("creating a group that exists (in another case) succeeded")
	}
}

func TestGroupCLI_RefusedNames(t *testing.T) {
	for name, n := range map[string]string{
		"empty":           "   ",
		"reserved prefix": "rudolph:ungrouped",
		"control char":    "eng\x07",
		"over 128 bytes":  strings.Repeat("g", 129),
	} {
		t.Run(name, func(t *testing.T) {
			ddblocal.Reset(t)
			if out, err := group(t, "create", n); err == nil {
				t.Errorf("accepted:\n%s", out)
			}
		})
	}
}

func TestGroupCLI_AssignAndShowMembership(t *testing.T) {
	ddblocal.Reset(t)
	mustGroup(t, "create", "eng")
	mustGroup(t, "create", "design")
	mustGroup(t, "add", "--machine", machine, "eng")
	mustGroup(t, "add", "--machine", strings.ToLower(machine), "design")
	ss, _ := item(t, "Machine#"+machine, "Groups")["Assigned"].(*awstypes.AttributeValueMemberSS)
	if ss == nil || len(ss.Value) != 2 {
		t.Fatalf("Assigned %v, want eng and design", item(t, "Machine#"+machine, "Groups"))
	}
	out := mustGroup(t, "show", "--machine", machine)
	if !strings.Contains(out, "eng") || !strings.Contains(out, "design") {
		t.Errorf("group show:\n%s", out)
	}
	mustGroup(t, "remove", "--machine", machine, "eng")
	mustGroup(t, "remove", "--machine", machine, "design")
	out = mustGroup(t, "show", "--machine", machine)
	if !strings.Contains(strings.ToLower(out), "ungrouped") {
		t.Errorf("a Mac in no group is not shown as ungrouped:\n%s", out)
	}
	if _, err := group(t, "add", "--machine", machine, "nonexistent"); err == nil {
		t.Error("assigning an unregistered group succeeded")
	}
}

func TestGroupCLI_GroupRules(t *testing.T) {
	ddblocal.Reset(t)
	mustGroup(t, "create", "eng")
	before := rulesVersion(t, "eng")
	if out, err := ruleCmd(t, "deny", "-t", "binary", "-i", sha, "--group", "eng", "--force"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	row := item(t, "GroupRules#eng", "Binary#"+sha)
	if len(row) == 0 {
		t.Fatal("no group rule row")
	}
	if g, _ := row["Group"].(*awstypes.AttributeValueMemberS); g == nil || g.Value != "eng" {
		t.Errorf("Group %v", row["Group"])
	}
	if rulesVersion(t, "eng") <= before {
		t.Errorf("RulesVersion %d did not increase from %d", rulesVersion(t, "eng"), before)
	}
	if len(item(t, "GlobalRules", "Binary#"+sha)) != 0 {
		t.Error("a group rule was also written as a global rule")
	}

	if out, err := ruleCmd(t, "deny", "-t", "binary", "-i", sha, "--ungrouped", "--force"); err != nil {
		t.Fatalf("--ungrouped: %v\n%s", err, out)
	}
	if len(item(t, "GroupRules#rudolph:ungrouped", "Binary#"+sha)) == 0 {
		t.Error("no ungrouped rule row")
	}

	for name, args := range map[string][]string{
		"unregistered group":  {"deny", "-t", "binary", "-i", sha, "--group", "nope", "--force"},
		"group and global":    {"deny", "-t", "binary", "-i", sha, "--group", "eng", "--global", "--force"},
		"group and machine":   {"deny", "-t", "binary", "-i", sha, "--group", "eng", "--machine", machine, "--force"},
		"group with expiry":   {"deny", "-t", "binary", "-i", sha, "--group", "eng", "--expires", "24h", "--force"},
		"group and ungrouped": {"deny", "-t", "binary", "-i", sha, "--group", "eng", "--ungrouped", "--force"},
	} {
		if out, err := ruleCmd(t, args...); err == nil {
			t.Errorf("%s: accepted:\n%s", name, out)
		}
	}

	if out, err := ruleCmd(t, "remove", "-t", "binary", "-i", sha, "--group", "eng", "--force"); err != nil {
		t.Fatalf("remove: %v\n%s", err, out)
	}
	if len(item(t, "GroupRules#eng", "Binary#"+sha)) != 0 {
		t.Error("the group rule was not removed")
	}
}

func TestGroupCLI_DeleteNeedsForceWhileTheGroupHasRules(t *testing.T) {
	ddblocal.Reset(t)
	mustGroup(t, "create", "eng")
	if out, err := ruleCmd(t, "deny", "-t", "binary", "-i", sha, "--group", "eng", "--force"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := group(t, "delete", "eng"); err == nil {
		t.Fatal("deleting a group with rules succeeded without --force")
	}
	mustGroup(t, "delete", "eng", "--force")
	if registryEntry(t, "eng") != nil || len(item(t, "GroupRules#eng", "Binary#"+sha)) != 0 {
		t.Error("delete --force left the group or its rules")
	}
}

// From the groups review: rule partitions are cached by group name and RulesVersion, so a group that is deleted and
// created again must never reuse a RulesVersion its earlier life had, or a server could keep serving the deleted
// group's rules.
func TestGroupCLI_RecreatedGroupNeverReusesARulesVersion(t *testing.T) {
	ddblocal.Reset(t)
	mustGroup(t, "create", "x")
	if out, err := ruleCmd(t, "deny", "-t", "binary", "-i", sha, "--group", "x", "--force"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err := ruleCmd(t, "deny", "-t", "binary", "-i", strings.Repeat("cd", 32), "--group", "x", "--force"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	highest := rulesVersion(t, "x")
	mustGroup(t, "delete", "x", "--force")
	mustGroup(t, "create", "x")
	if v := rulesVersion(t, "x"); v <= highest {
		t.Errorf("recreated group starts at RulesVersion %d; its earlier life reached %d", v, highest)
	}
}
