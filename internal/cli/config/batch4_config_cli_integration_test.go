//go:build integration

package config

// B5 and B6 (batch 4) through the `rudolph config` commands, against DynamoDB Local.

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/airbnb/rudolph/internal/testutil/clitest"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestMain(m *testing.M) { os.Exit(ddblocal.Main(m)) }

const machine = "AAAAAAAA-1111-4111-8111-AAAAAAAAAAAA"

var env = clitest.Env{Table: ddblocal.TableName, Region: ddblocal.Region}

func configRow(t *testing.T, pk string) map[string]awstypes.AttributeValue {
	t.Helper()
	out, err := ddblocal.Raw(t).GetItem(context.Background(), &awsdynamodb.GetItemInput{
		TableName: aws.String(ddblocal.TableName),
		Key: map[string]awstypes.AttributeValue{
			"PK": &awstypes.AttributeValueMemberS{Value: pk},
			"SK": &awstypes.AttributeValueMemberS{Value: "Config"},
		},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.Item
}

// Guard: the command layer writes a valid configuration.
func TestConfigSet_WritesAValidGlobalConfig(t *testing.T) {
	ddblocal.Reset(t)
	if out, err := clitest.Run(t, env, ConfigCmd, "yes\n", "set", "--global", "-c", "monitor", "--batch-size", "100", "--full-sync-interval", "120"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(configRow(t, "GlobalConfig")) == 0 {
		t.Fatal("no global config written")
	}
}

func TestB5_ConfigSetRefusesInvalidValues(t *testing.T) {
	for name, args := range map[string][]string{
		"full sync interval below 60": {"set", "--global", "-c", "monitor", "--full-sync-interval", "10"},
		"negative batch size":         {"set", "--machine", machine, "-c", "monitor", "--batch-size", "-1"},
		"huge batch size":             {"set", "--global", "-c", "monitor", "--batch-size", "100000"},
	} {
		t.Run(name, func(t *testing.T) {
			ddblocal.Reset(t)
			out, err := clitest.Run(t, env, ConfigCmd, "yes\n", args...)
			if err == nil {
				t.Fatalf("an invalid value was accepted:\n%s", out)
			}
			if len(configRow(t, "GlobalConfig")) != 0 || len(configRow(t, "Machine#"+machine)) != 0 {
				t.Fatal("a refused config was written")
			}
		})
	}
}

// B6 (decided): --clean-sync works for a machine and is refused globally (a fleet-wide clean sync storm).
func TestB6_ConfigSetCleanSyncIsRefusedGlobally(t *testing.T) {
	ddblocal.Reset(t)
	out, err := clitest.Run(t, env, ConfigCmd, "yes\n", "set", "--global", "-c", "monitor", "--clean-sync")
	if err == nil {
		t.Fatalf("--global --clean-sync must be refused:\n%s", out)
	}
	if !strings.Contains(err.Error(), "clean") {
		t.Errorf("the error must explain that --clean-sync is per machine: %v", err)
	}
	if len(configRow(t, "GlobalConfig")) != 0 {
		t.Fatal("the refused global config was written")
	}

	if out, err := clitest.Run(t, env, ConfigCmd, "yes\n", "set", "--machine", machine, "-c", "monitor", "--clean-sync"); err != nil {
		t.Fatalf("--machine --clean-sync must be accepted: %v\n%s", err, out)
	}
}
