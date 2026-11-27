//go:build integration

package machineconfiguration_test

// RequestFleetCleanSync is how the fleet job requests a clean sync for a rules hash outlier:
//
//	RequestFleetCleanSync(ctx, client, table, machineID, changeKey, now) (request string, err error)
//
// with client the UpdateItem of the AWS SDK's DynamoDB client (FleetCleanSyncWriter) and table the store table. It is
// one UpdateItem of the Mac's `Machine#<id>` / `Config` item, created if absent, that sets CleanSync true, a new
// CleanSyncRequest (as `rudolph config update --clean-sync` writes), AutoCleanSyncFor = changeKey, AutoCleanSyncAt = now
// as RFC 3339 UTC and AutoCleanSyncRequest = the CleanSyncRequest written, and DataType MachineConfig; no other
// attribute. It returns the CleanSyncRequest written. Preflight performs and clears the request like any other
// (ClearMachineCleanSync), which leaves the AutoCleanSync attributes as the record of it.

import (
	"context"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
	rudolphtypes "github.com/airbnb/rudolph/pkg/types"
)

func TestMain(m *testing.M) { os.Exit(ddblocal.Main(m)) }

const fleetMac = "AAAAAAAA-1111-4111-8111-AAAAAAAAAAAA"

var requestedAt = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

var _ machineconfiguration.FleetCleanSyncWriter = (*awsdynamodb.Client)(nil)

func configKey(pk string) map[string]awstypes.AttributeValue {
	return map[string]awstypes.AttributeValue{"PK": sv(pk), "SK": sv("Config")}
}

func storedItem(t *testing.T, pk string) map[string]awstypes.AttributeValue {
	t.Helper()
	out, err := ddblocal.Raw(t).GetItem(context.Background(), &awsdynamodb.GetItemInput{
		TableName: aws.String(ddblocal.TableName), Key: configKey(pk), ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.Item
}

func putItem(t *testing.T, item map[string]awstypes.AttributeValue) {
	t.Helper()
	if _, err := ddblocal.Raw(t).PutItem(context.Background(), &awsdynamodb.PutItemInput{TableName: aws.String(ddblocal.TableName), Item: item}); err != nil {
		t.Fatal(err)
	}
}

func request(t *testing.T, changeKey string, at time.Time) string {
	t.Helper()
	got, err := machineconfiguration.RequestFleetCleanSync(context.Background(), ddblocal.Raw(t), ddblocal.TableName, fleetMac, changeKey, at)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func stringAttr(item map[string]awstypes.AttributeValue, name string) string {
	v, _ := item[name].(*awstypes.AttributeValueMemberS)
	if v == nil {
		return ""
	}
	return v.Value
}

func checkRequest(t *testing.T, item map[string]awstypes.AttributeValue, request, changeKey string, at time.Time) {
	t.Helper()
	if v, ok := item["CleanSync"].(*awstypes.AttributeValueMemberBOOL); !ok || !v.Value {
		t.Errorf("CleanSync %#v, want the boolean true", item["CleanSync"])
	}
	if len(request) != 16 || strings.Trim(request, "0123456789abcdef") != "" {
		t.Errorf("returned request %q, want 16 hex digits", request)
	}
	for name, want := range map[string]string{
		"CleanSyncRequest": request, "AutoCleanSyncRequest": request, "AutoCleanSyncFor": changeKey,
		"AutoCleanSyncAt": at.UTC().Format(time.RFC3339), "DataType": "MachineConfig",
	} {
		if got := stringAttr(item, name); got != want {
			t.Errorf("%s %q, want %q", name, got, want)
		}
	}
}

func TestRequestFleetCleanSync_CreatesTheItem(t *testing.T) {
	ddblocal.Reset(t)
	got := request(t, "2026-11-21T16:00:00Z", requestedAt)
	item := storedItem(t, "Machine#"+fleetMac)
	checkRequest(t, item, got, "2026-11-21T16:00:00Z", requestedAt)
	var names []string
	for name := range item {
		names = append(names, name)
	}
	sort.Strings(names)
	want := []string{"AutoCleanSyncAt", "AutoCleanSyncFor", "AutoCleanSyncRequest", "CleanSync", "CleanSyncRequest", "DataType", "PK", "SK"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("the created item holds %v, want exactly %v: the Mac keeps inheriting every setting", names, want)
	}
}

// A time in another zone is stored as UTC.
func TestRequestFleetCleanSync_StoresUTC(t *testing.T) {
	ddblocal.Reset(t)
	zone, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Skip(err)
	}
	request(t, "none", requestedAt.In(zone))
	if got := stringAttr(storedItem(t, "Machine#"+fleetMac), "AutoCleanSyncAt"); got != "2026-11-22T12:00:00Z" {
		t.Errorf("AutoCleanSyncAt %q, want 2026-11-22T12:00:00Z", got)
	}
}

func TestRequestFleetCleanSync_KeepsTheOtherSettings(t *testing.T) {
	ddblocal.Reset(t)
	before := map[string]awstypes.AttributeValue{
		"PK": sv("Machine#" + fleetMac), "SK": sv("Config"), "DataType": sv("MachineConfig"),
		"ClientMode":                   &awstypes.AttributeValueMemberN{Value: "2"},
		"BatchSize":                    &awstypes.AttributeValueMemberN{Value: "100"},
		"DisableUnknownEventUpload":    &awstypes.AttributeValueMemberBOOL{Value: true},
		"TemporaryClientMode":          &awstypes.AttributeValueMemberN{Value: "1"},
		"TemporaryClientModeExpiresAt": &awstypes.AttributeValueMemberN{Value: "1795000000"},
		"EventDetailText":              sv("Ask the service desk"),
	}
	putItem(t, before)
	got := request(t, "none", requestedAt)
	item := storedItem(t, "Machine#"+fleetMac)
	checkRequest(t, item, got, "none", requestedAt)
	for name, value := range before {
		if !reflect.DeepEqual(item[name], value) {
			t.Errorf("%s changed from %v to %v", name, value, item[name])
		}
	}
	if len(item) != len(before)+5 {
		t.Errorf("the item holds %d attributes, want the %d it had and the five of the request", len(item), len(before))
	}
}

// A new request replaces an earlier one, the fleet job's or a manual one, as `rudolph config` does.
func TestRequestFleetCleanSync_ReplacesAnEarlierRequest(t *testing.T) {
	ddblocal.Reset(t)
	first := request(t, "2026-11-20T08:00:00Z", requestedAt.Add(-48*time.Hour))
	second := request(t, "2026-11-21T16:00:00Z", requestedAt)
	if second == first {
		t.Error("the second request has the first request's identifier")
	}
	checkRequest(t, storedItem(t, "Machine#"+fleetMac), second, "2026-11-21T16:00:00Z", requestedAt)
}

// Preflight clears the flag once the clean sync is confirmed; the AutoCleanSync attributes stay as the record of it,
// and decode with the rest of the config.
func TestRequestFleetCleanSync_PerformedAndThenAManualRequest(t *testing.T) {
	ddblocal.Reset(t)
	client := ddblocal.Client(t)
	got := request(t, "none", requestedAt)

	config, err := machineconfiguration.GetMachineSpecificConfig(client, fleetMac)
	if err != nil || config == nil {
		t.Fatalf("GetMachineSpecificConfig: %+v, %v", config, err)
	}
	if config.AutoCleanSyncFor != "none" || config.AutoCleanSyncAt != "2026-11-22T12:00:00Z" || config.AutoCleanSyncRequest != got || !config.CleanSyncRequestedByFleet() {
		t.Errorf("decoded %+v; want the request's attributes and CleanSyncRequestedByFleet", config)
	}

	if err := machineconfiguration.ClearMachineCleanSync(client, fleetMac, got); err != nil {
		t.Fatal(err)
	}
	item := storedItem(t, "Machine#"+fleetMac)
	if v, _ := item["CleanSync"].(*awstypes.AttributeValueMemberBOOL); v != nil && v.Value {
		t.Error("ClearMachineCleanSync left CleanSync set")
	}
	for _, name := range []string{"AutoCleanSyncFor", "AutoCleanSyncAt", "AutoCleanSyncRequest"} {
		if _, ok := item[name]; !ok {
			t.Errorf("clearing the performed clean sync removed %s", name)
		}
	}

	// A manual request afterwards is not the fleet job's.
	svc := machineconfiguration.GetUncachedMachineConfigurationService(client, clock.ConcreteTimeProvider{})
	yes := true
	if _, err := svc.UpdateMachineConfig(fleetMac, machineconfiguration.MachineConfigurationUpdateRequest{CleanSync: &yes}); err != nil {
		t.Fatal(err)
	}
	config, err = machineconfiguration.GetMachineSpecificConfig(client, fleetMac)
	if err != nil {
		t.Fatal(err)
	}
	if !config.CleanSync || config.CleanSyncRequest == got || config.CleanSyncRequestedByFleet() {
		t.Errorf("after a manual request: CleanSync %v CleanSyncRequest %q (fleet's was %q) CleanSyncRequestedByFleet %v; want a manual request",
			config.CleanSync, config.CleanSyncRequest, got, config.CleanSyncRequestedByFleet())
	}
}

// The global config never carries the attributes: they are not written with it, and a row edited by hand that has
// them is not inherited.
func TestAutoCleanSyncAttributesAndTheGlobalConfig(t *testing.T) {
	ddblocal.Reset(t)
	client := ddblocal.Client(t)
	svc := machineconfiguration.GetUncachedMachineConfigurationService(client, clock.ConcreteTimeProvider{})
	cfg := machineconfiguration.GetUniversalDefaultConfig()
	cfg.ClientMode = rudolphtypes.Monitor
	cfg.AutoCleanSyncFor, cfg.AutoCleanSyncAt, cfg.AutoCleanSyncRequest = "none", "2026-11-22T12:00:00Z", "0123456789abcdef"
	if err := svc.SetGlobalConfig(cfg); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"AutoCleanSyncFor", "AutoCleanSyncAt", "AutoCleanSyncRequest"} {
		if _, ok := storedItem(t, "GlobalConfig")[name]; ok {
			t.Errorf("SetGlobalConfig wrote %s to the global config", name)
		}
	}

	global := storedItem(t, "GlobalConfig")
	global["AutoCleanSyncFor"], global["AutoCleanSyncAt"], global["AutoCleanSyncRequest"] = sv("none"), sv("2026-11-22T12:00:00Z"), sv("0123456789abcdef")
	putItem(t, global)
	for name, own := range map[string]bool{"without a machine config": false, "with one": true} {
		t.Run(name, func(t *testing.T) {
			if own {
				putItem(t, map[string]awstypes.AttributeValue{"PK": sv("Machine#" + fleetMac), "SK": sv("Config"), "DataType": sv("MachineConfig"),
					"BatchSize": &awstypes.AttributeValueMemberN{Value: "100"}})
			}
			intended, err := machineconfiguration.GetIntendedConfig(client, fleetMac)
			if err != nil {
				t.Fatal(err)
			}
			if intended.AutoCleanSyncFor != "" || intended.AutoCleanSyncAt != "" || intended.AutoCleanSyncRequest != "" {
				t.Errorf("inherited from the global config: %q %q %q", intended.AutoCleanSyncFor, intended.AutoCleanSyncAt, intended.AutoCleanSyncRequest)
			}
		})
	}
}
