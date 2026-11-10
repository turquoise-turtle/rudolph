//go:build integration

// Package machine holds `rudolph machine show` and `rudolph machines` (plan C.13): what a Mac last reported and how
// Rudolph is set up for it, from the command line.
package machine

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/cli/config"
	"github.com/airbnb/rudolph/internal/cli/group"
	"github.com/airbnb/rudolph/internal/testutil/clitest"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	rdynamodb "github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/sensordata"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

func TestMain(m *testing.M) { os.Exit(ddblocal.Main(m)) }

const (
	macA = "AAAAAAAA-1111-4111-8111-AAAAAAAAAAAA"
	macB = "BBBBBBBB-2222-4222-8222-BBBBBBBBBBBB"
	macC = "CCCCCCCC-3333-4333-8333-CCCCCCCCCCCC"
)

func env() clitest.Env { return clitest.Env{Table: ddblocal.TableName, Region: ddblocal.Region} }

func seedMac(t *testing.T, id, hostname, serial string, seen time.Time) {
	t.Helper()
	pk, sk := sensordata.MachineIDSensorDataPKSK(id)
	sd := sensordata.SensorData{
		PrimaryKey:      rdynamodb.PrimaryKey{PartitionKey: pk, SortKey: sk},
		MachineID:       id,
		SerialNum:       serial,
		SerialKey:       sensordata.SerialKey(serial),
		Hostname:        hostname,
		HostnameKey:     sensordata.HostnameKey(hostname),
		ModelIdentifier: "Mac15,3",
		OSVersion:       "15.1",
		OSBuild:         "24B83",
		SantaVersion:    "2026.9.0",
		ClientMode:      types.Monitor,
		PrimaryUser:     "jsmith",
		RuleCount:       12,
		BinaryRuleCount: 7,
		TeamIDRuleCount: 5,
		Time:            seen.UTC().Format(time.RFC3339),
		LastSeenDay:     sensordata.LastSeenDay(seen),
		DataType:        sensordata.GetDataType(),
	}
	item, err := attributevalue.MarshalMap(sd)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ddblocal.Raw(t).PutItem(context.Background(), &awsdynamodb.PutItemInput{TableName: aws.String(ddblocal.TableName), Item: item}); err != nil {
		t.Fatal(err)
	}
}

func mustContain(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("output does not contain %q:\n%s", w, out)
		}
	}
}

func TestMachineShow_WhatTheMacReported(t *testing.T) {
	ddblocal.Reset(t)
	seen := time.Now().Add(-25 * time.Minute)
	seedMac(t, macA, "lab-mac-01", "C02ABC123", seen)
	out, err := clitest.Run(t, env(), MachineCmd, "", "show", "-m", macA)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	mustContain(t, out, macA, "lab-mac-01", "C02ABC123", "jsmith", "2026.9.0", "15.1", "MONITOR", "12", "("+seen.UTC().Format("2006-01-02 15:04:05")+" UTC)")
}

func TestMachineShow_AcceptsAnyCase(t *testing.T) {
	ddblocal.Reset(t)
	seedMac(t, macA, "lab-mac-01", "C02ABC123", time.Now())
	out, err := clitest.Run(t, env(), MachineCmd, "", "show", "-m", strings.ToLower(macA))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	mustContain(t, out, "lab-mac-01")
}

func TestMachineShow_TemporaryModeAndGroups(t *testing.T) {
	ddblocal.Reset(t)
	seedMac(t, macA, "lab-mac-01", "C02ABC123", time.Now())
	if out, err := clitest.Run(t, env(), config.ConfigCmd, "", "update", "--machine", macA, "-c", "lockdown", "--for", "2h", "--yes"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err := clitest.Run(t, env(), group.GroupCmd, "", "create", "eng", "--description", "Engineering"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err := clitest.Run(t, env(), group.GroupCmd, "", "add", "-m", macA, "eng"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	out, err := clitest.Run(t, env(), MachineCmd, "", "show", "-m", macA)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	lower := strings.ToLower(out)
	mustContain(t, lower, "lockdown", "temporary", "eng")
}

func TestMachineShow_UnknownMachine(t *testing.T) {
	ddblocal.Reset(t)
	out, err := clitest.Run(t, env(), MachineCmd, "", "show", "-m", macB)
	if err == nil {
		t.Fatalf("no error for a machine that never synced:\n%s", out)
	}
	if !strings.Contains(strings.ToLower(err.Error()+out), "no machine") {
		t.Errorf("error does not say there is no such machine: %v\n%s", err, out)
	}
}

func TestMachineShow_RejectsAnInvalidID(t *testing.T) {
	ddblocal.Reset(t)
	if out, err := clitest.Run(t, env(), MachineCmd, "", "show", "-m", "not-a-uuid"); err == nil {
		t.Fatalf("accepted an invalid machine ID:\n%s", out)
	}
}

func TestMachines_ListsWithLimit(t *testing.T) {
	ddblocal.Reset(t)
	now := time.Now()
	seedMac(t, macA, "lab-mac-01", "C02AAA111", now)
	seedMac(t, macB, "lab-mac-02", "C02BBB222", now.Add(-time.Hour))
	seedMac(t, macC, "office-mac", "C02CCC333", now.Add(-48*time.Hour))

	out, err := clitest.Run(t, env(), MachinesCmd, "")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	mustContain(t, out, macA, macB, macC, "lab-mac-01", "office-mac", "C02BBB222")

	out, err = clitest.Run(t, env(), MachinesCmd, "", "--limit", "2")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	n := strings.Count(out, "-4111-") + strings.Count(out, "-4222-") + strings.Count(out, "-4333-")
	if n != 2 {
		t.Errorf("--limit 2 listed %d machines:\n%s", n, out)
	}
	mustContain(t, out, "--limit")

	if out, err := clitest.Run(t, env(), MachinesCmd, "", "--limit", "0"); err == nil {
		t.Errorf("--limit 0 accepted:\n%s", out)
	}
}

func TestMachines_None(t *testing.T) {
	ddblocal.Reset(t)
	out, err := clitest.Run(t, env(), MachinesCmd, "")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	mustContain(t, strings.ToLower(out), "no machine")
}
