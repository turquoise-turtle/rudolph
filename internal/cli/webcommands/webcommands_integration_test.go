//go:build integration

// Package webcommands holds the contract test for the commands rudolph-web shows for copying (plan section F1).
package webcommands

// Every line of testdata/webcontract/cli-examples.txt is a command rudolph-web renders. Run with --dry-run, each must
// parse with the real command tree, succeed without a prompt, and write nothing. --dry-run is accepted by every
// command under `rule`, `rules`, `config` and `group`: a write prints what it would write and exits 0 without writing;
// a read ignores it.

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/cli/config"
	"github.com/airbnb/rudolph/internal/cli/group"
	"github.com/airbnb/rudolph/internal/cli/rule"
	"github.com/airbnb/rudolph/internal/cli/rules"
	"github.com/airbnb/rudolph/internal/testutil/clitest"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	"github.com/airbnb/rudolph/pkg/model/groups"
	"github.com/airbnb/rudolph/pkg/model/machinerules"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/spf13/cobra"
)

func TestMain(m *testing.M) { os.Exit(ddblocal.Main(m)) }

const machine = "AAAAAAAA-1111-4111-8111-AAAAAAAAAAAA"

func examplesFile() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata", "webcontract", "cli-examples.txt")
}

// shellWords splits a command line the way a POSIX shell does for the forms rudolph-web emits: words separated by
// spaces, single-quoted words taken literally.
func shellWords(t *testing.T, line string) []string {
	t.Helper()
	var words []string
	var cur strings.Builder
	inQuote, inWord := false, false
	for _, r := range line {
		switch {
		case inQuote && r == '\'':
			inQuote = false
		case inQuote:
			cur.WriteRune(r)
		case r == '\'':
			inQuote, inWord = true, true
		case r == ' ':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if inQuote {
		t.Fatalf("unterminated quote in %q", line)
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words
}

func itemCount(t *testing.T) int {
	t.Helper()
	n := 0
	var start map[string]awstypes.AttributeValue
	for {
		out, err := ddblocal.Raw(t).Scan(context.Background(), &awsdynamodb.ScanInput{TableName: aws.String(ddblocal.TableName), ExclusiveStartKey: start, ConsistentRead: aws.Bool(true)})
		if err != nil {
			t.Fatal(err)
		}
		n += int(out.Count)
		if len(out.LastEvaluatedKey) == 0 {
			return n
		}
		start = out.LastEvaluatedKey
	}
}

// seed writes the rows the example commands refer to.
func seed(t *testing.T) {
	t.Helper()
	ddblocal.Reset(t)
	tp := clock.ConcreteTimeProvider{}
	client := ddblocal.Client(t)
	for _, r := range []struct {
		id string
		rt types.RuleType
		p  types.Policy
	}{
		{strings.Repeat("0", 63) + "1", types.RuleTypeBinary, types.RulePolicyBlocklist},
		{"EQHXZ8M8AV", types.RuleTypeTeamID, types.RulePolicyAllowlist},
	} {
		if err := globalrules.AddNewGlobalRule(tp, client, r.id, r.rt, r.p, "web commands"); err != nil {
			t.Fatal(err)
		}
	}
	if err := machinerules.AddNewMachineRule(client, machine, strings.Repeat("0", 63)+"b", types.RuleTypeBinary, types.RulePolicyAllowlist, "web commands", time.Now().Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	gw, ok := client.(groups.RegistryWriter)
	if !ok {
		t.Fatal("the DynamoDB client cannot write groups")
	}
	if err := groups.CreateGroup(tp, gw, "eng", "Engineering", true); err != nil {
		t.Fatal(err)
	}
}

func TestWebCommands_EveryExampleRunsAsADryRun(t *testing.T) {
	f, err := os.Open(examplesFile())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	commands := map[string]*cobra.Command{"rule": rule.RuleCmd, "rules": rules.RulesCmd, "config": config.ConfigCmd, "group": group.GroupCmd}
	env := clitest.Env{Table: ddblocal.TableName, Region: ddblocal.Region}

	lines := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		lines++
		t.Run(line, func(t *testing.T) {
			words := shellWords(t, line)
			if len(words) < 3 || words[0] != "ENV=prod" || words[1] != "rudolph" {
				t.Fatalf("not of the form `ENV=<env> rudolph <command> …`: %q", line)
			}
			cmd, ok := commands[words[2]]
			if !ok {
				t.Fatalf("unknown command %q", words[2])
			}
			seed(t)
			before := itemCount(t)
			out, err := clitest.Run(t, env, cmd, "", append(words[3:], "--dry-run")...)
			if err != nil {
				t.Fatalf("dry run failed: %v\n%s", err, out)
			}
			if after := itemCount(t); after != before {
				t.Errorf("the dry run changed the table: %d items before, %d after\n%s", before, after, out)
			}
		})
	}
	if lines < 10 {
		t.Errorf("only %d example commands", lines)
	}
}

// A dry run of a write says what it would do, so the owner can check a pasted command first.
func TestWebCommands_DryRunSaysWhatItWouldWrite(t *testing.T) {
	seed(t)
	env := clitest.Env{Table: ddblocal.TableName, Region: ddblocal.Region}
	id := strings.Repeat("0", 63) + "e"
	out, err := clitest.Run(t, env, rule.RuleCmd, "", "deny", "-t", "binary", "-i", id, "--global", "--dry-run")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, id) || !strings.Contains(strings.ToLower(out), "dry run") {
		t.Errorf("dry run output does not name the rule and say it is a dry run:\n%s", out)
	}
}
