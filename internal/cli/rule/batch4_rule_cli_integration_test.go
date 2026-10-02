//go:build integration

package rule

// B7 (batch 4): machine rules from the CLI. Run through the cobra commands against DynamoDB Local.
//
// Flags specified here: --expires <duration|RFC 3339 timestamp> sets when a machine rule expires (a Go duration such
// as "48h" counts from now); --permanent creates a machine rule that never expires; neither is allowed with --global,
// they cannot be combined, and an expiry in the past is refused. Every command that writes a rule requires either
// --global or --machine: no silent default to "this machine".

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/clitest"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestMain(m *testing.M) { os.Exit(ddblocal.Main(m)) }

const machine = "AAAAAAAA-1111-4111-8111-AAAAAAAAAAAA"

var sha = strings.Repeat("ab", 32)

var env = clitest.Env{Table: ddblocal.TableName, Region: ddblocal.Region}

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return clitest.Run(t, env, RuleCmd, "yes\n", args...)
}

func machineRule(t *testing.T) map[string]awstypes.AttributeValue {
	t.Helper()
	out, err := ddblocal.Raw(t).GetItem(context.Background(), &awsdynamodb.GetItemInput{
		TableName: aws.String(ddblocal.TableName),
		Key: map[string]awstypes.AttributeValue{
			"PK": &awstypes.AttributeValueMemberS{Value: "MachineRules#" + machine},
			"SK": &awstypes.AttributeValueMemberS{Value: "Binary#" + sha},
		},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.Item
}

func countItems(t *testing.T) int32 {
	t.Helper()
	out, err := ddblocal.Raw(t).Scan(context.Background(), &awsdynamodb.ScanInput{TableName: aws.String(ddblocal.TableName)})
	if err != nil {
		t.Fatal(err)
	}
	return out.Count
}

func expiresAfter(t *testing.T, item map[string]awstypes.AttributeValue) (int64, bool) {
	t.Helper()
	n, ok := item["ExpiresAfter"].(*awstypes.AttributeValueMemberN)
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseInt(n.Value, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return v, v != 0
}

func TestB7_RuleCommandsRequireGlobalOrMachine(t *testing.T) {
	for _, args := range [][]string{
		{"allow", "-t", "binary", "-i", sha},
		{"deny", "-t", "binary", "-i", sha},
		{"silent", "-t", "binary", "-i", sha},
		{"silent-gui", "-t", "binary", "-i", sha},
		{"silent-tty", "-t", "binary", "-i", sha},
		{"compiler", "-t", "binary", "-i", sha},
		{"cel", "-t", "binary", "-i", sha, "-e", "true"},
		{"remove", "Binary#" + sha},
	} {
		t.Run(args[0], func(t *testing.T) {
			ddblocal.Reset(t)
			out, err := run(t, append(args, "--force")...)
			if err == nil {
				t.Fatalf("rule %s without --global or --machine must fail; output:\n%s", args[0], out)
			}
			if !strings.Contains(err.Error(), "--global") || !strings.Contains(err.Error(), "--machine") {
				t.Errorf("the error must say to pass --global or --machine: %v", err)
			}
			if n := countItems(t); n != 0 {
				t.Errorf("nothing may be written, found %d items", n)
			}
		})
	}
}

func TestB7_MachineRuleExpiryFromTheCLI(t *testing.T) {
	ddblocal.Reset(t)
	start := time.Now()
	if out, err := run(t, "allow", "-t", "binary", "-i", sha, "--machine", machine, "--expires", "48h", "--force"); err != nil {
		t.Fatalf("--expires 48h: %v\n%s", err, out)
	}
	got, ok := expiresAfter(t, machineRule(t))
	want := start.Add(48 * time.Hour).Unix()
	if !ok || got < want-120 || got > want+120 {
		t.Fatalf("--expires 48h stored ExpiresAfter %d, want about %d", got, want)
	}

	ddblocal.Reset(t)
	stamp := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second)
	if out, err := run(t, "deny", "-t", "binary", "-i", sha, "--machine", machine, "--expires", stamp.Format(time.RFC3339), "--force"); err != nil {
		t.Fatalf("--expires <timestamp>: %v\n%s", err, out)
	}
	if got, _ := expiresAfter(t, machineRule(t)); got != stamp.Unix() {
		t.Fatalf("--expires %s stored ExpiresAfter %d, want %d", stamp.Format(time.RFC3339), got, stamp.Unix())
	}

	ddblocal.Reset(t)
	if out, err := run(t, "allow", "-t", "binary", "-i", sha, "--machine", machine, "--permanent", "--force"); err != nil {
		t.Fatalf("--permanent: %v\n%s", err, out)
	}
	item := machineRule(t)
	if len(item) == 0 {
		t.Fatal("--permanent wrote no machine rule")
	}
	if got, ok := expiresAfter(t, item); ok {
		t.Fatalf("a permanent machine rule must not expire (no ExpiresAfter, the table's TTL attribute); got %d", got)
	}
}

// Guard: without an expiry flag a machine rule is still created (with whatever default expiry), through the command
// layer with --machine and the typed confirmation.
func TestB7_MachineRuleWithoutExpiryFlagIsCreated(t *testing.T) {
	ddblocal.Reset(t)
	if out, err := run(t, "allow", "-t", "binary", "-i", sha, "--machine", machine, "--force"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(machineRule(t)) == 0 {
		t.Fatal("no machine rule written")
	}
}

func TestB7_InvalidExpiryCombinationsAreRefused(t *testing.T) {
	for name, args := range map[string][]string{
		"permanent and expires": {"allow", "-t", "binary", "-i", sha, "--machine", machine, "--permanent", "--expires", "1h"},
		"expires on a global":   {"allow", "-t", "binary", "-i", sha, "--global", "--expires", "1h"},
		"permanent on a global": {"allow", "-t", "binary", "-i", sha, "--global", "--permanent"},
		"expiry in the past":    {"allow", "-t", "binary", "-i", sha, "--machine", machine, "--expires", "2001-01-01T00:00:00Z"},
		"unparseable expiry":    {"allow", "-t", "binary", "-i", sha, "--machine", machine, "--expires", "next tuesday"},
	} {
		t.Run(name, func(t *testing.T) {
			ddblocal.Reset(t)
			if _, err := run(t, append(args, "--force")...); err == nil {
				t.Fatal("must be refused")
			}
			if n := countItems(t); n != 0 {
				t.Errorf("nothing may be written, found %d items", n)
			}
		})
	}
}
