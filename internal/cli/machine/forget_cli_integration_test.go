//go:build integration

package machine

// `rudolph machine forget -m <id>` removes everything Rudolph keeps for one retired Mac (plan C.13, recorded as wanted):
// its sensor data, sync state, configuration, machine rules, rule delivery tracking and group assignments. Events stay
// until they expire (they record what happened). It asks first, and --dry-run deletes nothing.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/cli/config"
	"github.com/airbnb/rudolph/internal/cli/group"
	"github.com/airbnb/rudolph/internal/cli/rule"
	"github.com/airbnb/rudolph/internal/testutil/clitest"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// itemsMentioning returns the PK/SK of every item whose key contains the machine ID (any case).
func itemsMentioning(t *testing.T, id string) []string {
	t.Helper()
	var keys []string
	var start map[string]awstypes.AttributeValue
	for {
		out, err := ddblocal.Raw(t).Scan(context.Background(), &awsdynamodb.ScanInput{TableName: aws.String(ddblocal.TableName), ExclusiveStartKey: start, ConsistentRead: aws.Bool(true)})
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range out.Items {
			pk := it["PK"].(*awstypes.AttributeValueMemberS).Value
			sk := it["SK"].(*awstypes.AttributeValueMemberS).Value
			if strings.Contains(strings.ToUpper(pk+" "+sk), strings.ToUpper(id)) {
				keys = append(keys, pk+" / "+sk)
			}
		}
		if len(out.LastEvaluatedKey) == 0 {
			return keys
		}
		start = out.LastEvaluatedKey
	}
}

func seedEverything(t *testing.T, id string) {
	t.Helper()
	seedMac(t, id, "retired-"+id[:4], "SER"+id[:4], time.Now())
	run := func(args ...string) {
		t.Helper()
		var err error
		var out string
		switch args[0] {
		case "config":
			out, err = clitest.Run(t, env(), config.ConfigCmd, "", args[1:]...)
		case "rule":
			out, err = clitest.Run(t, env(), rule.RuleCmd, "", args[1:]...)
		case "group":
			out, err = clitest.Run(t, env(), group.GroupCmd, "", args[1:]...)
		}
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}
	run("config", "update", "--machine", id, "-c", "lockdown", "--for", "2h", "--yes")
	run("rule", "allow", "-t", "binary", "-i", strings.Repeat("ab", 32), "--machine", id, "--expires", "2h", "--yes")
	run("group", "add", "-m", id, "eng")
}

func TestMachineForget_RemovesEverythingForThatMacOnly(t *testing.T) {
	ddblocal.Reset(t)
	if out, err := clitest.Run(t, env(), group.GroupCmd, "", "create", "eng"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	seedEverything(t, macA)
	seedEverything(t, macB)
	if len(itemsMentioning(t, macA)) < 3 {
		t.Fatalf("precondition: only %v stored for %s", itemsMentioning(t, macA), macA)
	}
	keepB := itemsMentioning(t, macB)

	// Dry run: says what it would delete, deletes nothing.
	before := itemsMentioning(t, macA)
	out, err := clitest.Run(t, env(), MachineCmd, "", "forget", "-m", macA, "--dry-run")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got := itemsMentioning(t, macA); len(got) != len(before) {
		t.Errorf("--dry-run deleted items: before %v, after %v", before, got)
	}

	// Declining the prompt deletes nothing.
	if out, err := clitest.Run(t, env(), MachineCmd, "no\n", "forget", "-m", macA); err == nil && len(itemsMentioning(t, macA)) != len(before) {
		t.Errorf("answering no still deleted items:\n%s", out)
	}
	if got := itemsMentioning(t, macA); len(got) != len(before) {
		t.Errorf("answering no deleted items: %v", got)
	}

	out, err = clitest.Run(t, env(), MachineCmd, "yes\n", "forget", "-m", strings.ToLower(macA))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if left := itemsMentioning(t, macA); len(left) != 0 {
		t.Errorf("items left for the forgotten Mac: %v", left)
	}
	if got := itemsMentioning(t, macB); len(got) != len(keepB) {
		t.Errorf("the other Mac's items changed: before %v, after %v", keepB, got)
	}
	if out, err := clitest.Run(t, env(), MachineCmd, "", "show", "-m", macA); err == nil {
		t.Errorf("machine show still finds the forgotten Mac:\n%s", out)
	}
	// The group itself stays; only the assignment went.
	out, err = clitest.Run(t, env(), group.GroupCmd, "", "get", "eng")
	if err != nil {
		t.Fatalf("group eng is gone: %v\n%s", err, out)
	}
}

func TestMachineForget_UnknownMachine(t *testing.T) {
	ddblocal.Reset(t)
	if out, err := clitest.Run(t, env(), MachineCmd, "", "forget", "-m", macC, "--yes"); err == nil {
		t.Errorf("forgetting a Mac Rudolph has nothing for succeeded:\n%s", out)
	}
}
