//go:build integration

package handlers

// R3 and R4 of rudolph-web's docs/data-layer-design.md: the web view lists Macs from two GSIs on the sensor data
// rows, DataType_SerialKey (ordered by serial, serial prefix search) and DataType_HostnameKey (hostname prefix
// search). SerialKey is the trimmed upper-case serial, so a search matches whatever case a serial was reported in
// (Apple serials are upper case, some virtual machines report mixed case); SerialNum keeps the serial as reported.
// Both use an INCLUDE projection of attributes that rarely change, so a steady-state preflight, which
// rewrites the row with a new Time and ExpiresAfter, writes nothing to either index. LastSeenDay (the UTC date of
// the last preflight) changes at most once a day and gives the list a last-seen column.

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	serialIndex   = "DataType_SerialKey"
	hostnameIndex = "DataType_HostnameKey"
)

// The attributes the machine list shows, plus the table and index keys DynamoDB always projects.
var machineListAttributes = []string{
	"PK", "SK", "DataType", "SerialKey", "HostnameKey", "SerialNum",
	"MachineID", "Hostname", "ModelIdentifier", "OSVersion", "SantaVersion", "ClientMode", "PrimaryUser", "LastSeenDay",
}

func queryIndex(t *testing.T, index, rangeKey, prefix string) []map[string]awstypes.AttributeValue {
	t.Helper()
	cond := "DataType = :dt"
	values := map[string]awstypes.AttributeValue{":dt": &awstypes.AttributeValueMemberS{Value: "SensorData"}}
	if prefix != "" {
		cond += " AND begins_with(" + rangeKey + ", :p)"
		values[":p"] = &awstypes.AttributeValueMemberS{Value: prefix}
	}
	out, err := ddblocal.Raw(t).Query(context.Background(), &awsdynamodb.QueryInput{
		TableName:                 aws.String(ddblocal.TableName),
		IndexName:                 aws.String(index),
		KeyConditionExpression:    aws.String(cond),
		ExpressionAttributeValues: values,
	})
	if err != nil {
		t.Fatalf("query %s: %v", index, err)
	}
	return out.Items
}

func machineIDs(t *testing.T, items []map[string]awstypes.AttributeValue) []string {
	t.Helper()
	var ids []string
	for _, it := range items {
		id, _ := sensorString(t, it, "MachineID")
		ids = append(ids, id)
	}
	return ids
}

func TestR4_SerialIndexListsMacsBySerial(t *testing.T) {
	transport := freshServer(t)
	macs := map[string]string{ // machine ID → serial
		machineA:                               "C02ZZZ0001",
		machineB:                               "C02AAA0002",
		"CCCCCCCC-3333-4333-8333-CCCCCCCCCCCC": "FVFAAA0003",
	}
	for id, serial := range macs {
		h := newHost(t, id, transport)
		h.SerialNumber = serial
		h.MustSync(t)
	}

	got := machineIDs(t, queryIndex(t, serialIndex, "SerialKey", ""))
	want := []string{machineB, machineA, "CCCCCCCC-3333-4333-8333-CCCCCCCCCCCC"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("serial index order %v, want %v", got, want)
	}
	if got := machineIDs(t, queryIndex(t, serialIndex, "SerialKey", "C02")); len(got) != 2 {
		t.Errorf("serial prefix C02 found %v, want the two C02 Macs", got)
	}
}

func TestR4_HostnameIndexFindsMacsCaseInsensitively(t *testing.T) {
	transport := freshServer(t)
	a := newHost(t, machineA, transport)
	a.Hostname = "Build-01.Corp.Example"
	a.MustSync(t)
	b := newHost(t, machineB, transport)
	b.Hostname = "laptop-02.corp.example"
	b.MustSync(t)

	if got := machineIDs(t, queryIndex(t, hostnameIndex, "HostnameKey", "build-")); len(got) != 1 || got[0] != machineA {
		t.Errorf("hostname prefix build- found %v, want only %s", got, machineA)
	}
	items := queryIndex(t, hostnameIndex, "HostnameKey", "")
	if got := machineIDs(t, items); strings.Join(got, ",") != machineA+","+machineB {
		t.Errorf("hostname index order %v, want %s then %s", got, machineA, machineB)
	}
	if h, _ := sensorString(t, items[0], "Hostname"); h != "Build-01.Corp.Example" {
		t.Errorf("hostname index projects Hostname %q, want it as reported", h)
	}
}

func TestR4_IndexesProjectOnlyTheMachineListAttributes(t *testing.T) {
	transport := freshServer(t)
	h := newHost(t, machineA, transport)
	h.PrimaryUserGroups = []string{"staff"}
	h.RulesHash = "0123456789abcdef"
	h.MustSync(t)

	allowed := map[string]bool{}
	for _, a := range machineListAttributes {
		allowed[a] = true
	}
	for _, index := range []struct{ name, rangeKey string }{{serialIndex, "SerialKey"}, {hostnameIndex, "HostnameKey"}} {
		items := queryIndex(t, index.name, index.rangeKey, "")
		if len(items) != 1 {
			t.Fatalf("%s: %d items, want 1", index.name, len(items))
		}
		var extra, missing []string
		for attr := range items[0] {
			if !allowed[attr] {
				extra = append(extra, attr)
			}
		}
		for _, attr := range machineListAttributes {
			if _, ok := items[0][attr]; !ok {
				missing = append(missing, attr)
			}
		}
		sort.Strings(extra)
		sort.Strings(missing)
		// Time, ExpiresAfter, RulesHash or the rule counts in an index would cost index writes on every sync.
		if len(extra) > 0 {
			t.Errorf("%s projects %v, which change on most syncs or are not shown in the list", index.name, extra)
		}
		if len(missing) > 0 {
			t.Errorf("%s lacks %v", index.name, missing)
		}
	}
}

func TestR3_LastSeenDayIsTheUTCDateOfTheLastPreflight(t *testing.T) {
	transport := freshServer(t)
	before := time.Now().UTC().Format("2006-01-02")
	newHost(t, machineA, transport).MustSync(t)
	after := time.Now().UTC().Format("2006-01-02")

	got, ok := sensorString(t, getItem(t, "Machine#"+machineA, "Current"), "LastSeenDay")
	if !ok || (got != before && got != after) {
		t.Errorf("LastSeenDay = %q (present %v), want %q", got, ok, after)
	}
}

// A Mac that reports no serial or no hostname is left out of that index only; its preflight still succeeds.
func TestR4_MacWithoutSerialOrHostnameStillSyncs(t *testing.T) {
	transport := freshServer(t)
	a := newHost(t, machineA, transport)
	a.SerialNumber = ""
	a.MustSync(t)
	b := newHost(t, machineB, transport)
	b.Hostname = ""
	b.MustSync(t)

	if got := machineIDs(t, queryIndex(t, serialIndex, "SerialKey", "")); len(got) != 1 || got[0] != machineB {
		t.Errorf("serial index has %v, want only %s", got, machineB)
	}
	if got := machineIDs(t, queryIndex(t, hostnameIndex, "HostnameKey", "")); len(got) != 1 || got[0] != machineA {
		t.Errorf("hostname index has %v, want only %s", got, machineA)
	}
}

func TestR4_SerialKeyIsTheTrimmedUpperCaseSerial(t *testing.T) {
	transport := freshServer(t)
	h := newHost(t, machineA, transport)
	h.SerialNumber = "VMk3xQ9pLr2a"
	h.MustSync(t)

	item := getItem(t, "Machine#"+machineA, "Current")
	if got, _ := sensorString(t, item, "SerialKey"); got != "VMK3XQ9PLR2A" {
		t.Errorf("SerialKey = %q, want %q", got, "VMK3XQ9PLR2A")
	}
	if got, _ := sensorString(t, item, "SerialNum"); got != "VMk3xQ9pLr2a" {
		t.Errorf("SerialNum = %q, want it stored as reported", got)
	}
	items := queryIndex(t, serialIndex, "SerialKey", "VMK3X")
	if got := machineIDs(t, items); len(got) != 1 || got[0] != machineA {
		t.Fatalf("serial prefix VMK3X found %v, want %s", got, machineA)
	}
	if got, _ := sensorString(t, items[0], "SerialNum"); got != "VMk3xQ9pLr2a" {
		t.Errorf("the serial index projects SerialNum %q, want it as reported", got)
	}

	h.SerialNumber = "   "
	h.MustSync(t)
	if v, ok := getItem(t, "Machine#"+machineA, "Current")["SerialKey"]; ok {
		t.Errorf("SerialKey is stored for a blank serial: %#v", v)
	}
}
