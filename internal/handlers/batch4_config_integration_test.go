//go:build integration

package handlers

// Batch 4: hosts without a serial number (B4), Content-Type handling (B9), validation of stored configuration (B5),
// machine clean sync (B6) and machine config inheritance (INH).

import (
	"context"
	"strings"
	"testing"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/internal/testutil/fakesanta"
	"github.com/airbnb/rudolph/internal/testutil/santaproto"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func configService(t *testing.T) machineconfiguration.MachineConfigurationService {
	return machineconfiguration.GetMachineConfigurationService(ddblocal.Client(t), clock.ConcreteTimeProvider{})
}

func scanAll(t *testing.T) []map[string]awstypes.AttributeValue {
	t.Helper()
	var items []map[string]awstypes.AttributeValue
	var start map[string]awstypes.AttributeValue
	for {
		out, err := ddblocal.Raw(t).Scan(context.Background(), &awsdynamodb.ScanInput{TableName: aws.String(ddblocal.TableName), ExclusiveStartKey: start})
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, out.Items...)
		if len(out.LastEvaluatedKey) == 0 {
			return items
		}
		start = out.LastEvaluatedKey
	}
}

func preflightOf(t *testing.T, res *fakesanta.SyncResult) string {
	t.Helper()
	return santaproto.Canonical(t, res.Preflight)
}

// ---------------------------------------------------------------------------------------------------------
// B4: Macs without a serial number
// ---------------------------------------------------------------------------------------------------------

// Virtual machines and Macs after some logic board repairs report no serial number. SerialNum is the key of a
// secondary index, which refuses an empty string, so writing "" makes every preflight of such a Mac fail.
func TestB4_MacWithoutSerialNumberCanSync(t *testing.T) {
	transport := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	host := newHost(t, machineA, transport)
	host.SerialNumber = ""
	for i := 0; i < 2; i++ {
		if _, err := host.Sync(t); err != nil {
			t.Fatalf("sync %d of a Mac without serial number failed: %v", i+1, err)
		}
	}
	if host.Policy("BINARY", sha1) != "BLOCKLIST" {
		t.Fatal("the Mac did not get its rules")
	}

	schema, err := ddblocal.LoadTableSchema()
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, index := range schema.Indexes {
		keys[index.HashKey], keys[index.RangeKey] = true, true
	}
	delete(keys, "")
	for _, item := range scanAll(t) {
		for name := range keys {
			if s, ok := item[name].(*awstypes.AttributeValueMemberS); ok && s.Value == "" {
				t.Errorf("item %v has an empty index key attribute %s", item["PK"], name)
			}
		}
	}
}

// ---------------------------------------------------------------------------------------------------------
// B9: Content-Type
// ---------------------------------------------------------------------------------------------------------

// Header names are case-insensitive (RFC 9110) and a media type may carry parameters; every stage must accept them.
func TestB9_ContentTypeIsMatchedCaseInsensitivelyWithParameters(t *testing.T) {
	for _, variant := range []struct{ name, value string }{
		{"content-type", "application/json"},
		{"CONTENT-TYPE", "application/json"},
		{"Content-Type", "application/json; charset=utf-8"},
		{"content-type", "Application/JSON;charset=UTF-8"},
	} {
		t.Run(variant.name+"="+variant.value, func(t *testing.T) {
			base := freshServer(t)
			addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
			transport := func(req events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, error) {
				headers := map[string]string{}
				for k, v := range req.Headers {
					if !strings.EqualFold(k, "Content-Type") {
						headers[k] = v
					}
				}
				headers[variant.name] = variant.value
				req.Headers = headers
				return base(req)
			}
			host := newHost(t, machineA, transport)
			host.PendingEvents = []string{`{"file_sha256":"` + sha3 + `","file_path":"/tmp","file_name":"x","decision":"BLOCK_UNKNOWN"}`}
			for i := 0; i < 2; i++ {
				if _, err := host.Sync(t); err != nil {
					t.Fatalf("sync %d with header %s: %s: %v", i+1, variant.name, variant.value, err)
				}
				host.PendingEvents = []string{`{"file_sha256":"` + sha3 + `","file_path":"/tmp","file_name":"x","decision":"BLOCK_UNKNOWN"}`}
			}
		})
	}
}

// ---------------------------------------------------------------------------------------------------------
// B5: stored configuration is validated before it is sent
// ---------------------------------------------------------------------------------------------------------

func manyFlags(n int) []string {
	flags := make([]string, n)
	for i := range flags {
		flags[i] = "rdonly"
	}
	return flags
}

// A value in storage that Santa cannot use (hand-edited, written by other tooling or an older Rudolph) must not make
// the preflight response unparseable for every Mac: the setting is left out (or defaulted), the others still apply.
func TestB5_BadStoredConfigValuesDegradeInsteadOfFailingEveryMac(t *testing.T) {
	for name, tc := range map[string]struct {
		attrs map[string]any
		// ok checks the parsed preflight response (canonical JSON)
		ok func(preflight string) bool
	}{
		"unknown client mode": {map[string]any{"ClientMode": 99}, func(string) bool { return true }},
		"negative batch size": {map[string]any{"BatchSize": -5}, func(p string) bool { return true }},
		"huge batch size": {map[string]any{"BatchSize": 100000}, func(p string) bool {
			return !strings.Contains(p, `"batch_size":100000`)
		}},
		"full sync interval below 60": {map[string]any{"FullSyncInterval": 10}, func(p string) bool {
			return !strings.Contains(p, `"full_sync_interval_seconds":10,`) && !strings.Contains(p, `"full_sync_interval_seconds":10}`)
		}},
		"negative full sync interval":       {map[string]any{"FullSyncInterval": -1}, func(string) bool { return true }},
		"full sync interval above uint32":   {map[string]any{"FullSyncInterval": 5000000000}, func(string) bool { return true }},
		"unknown file access action":        {map[string]any{"OverrideFileAccessAction": "BOGUS"}, func(p string) bool { return !strings.Contains(p, "BOGUS") }},
		"oversized remount flag list":       {map[string]any{"BlockUsbMount": true, "RemountUsbMode": manyFlags(10000)}, func(p string) bool { return strings.Count(p, "rdonly") < 10000 }},
		"remount flags of the wrong type":   {map[string]any{"BlockUsbMount": true, "RemountUsbMode": 7}, func(string) bool { return true }},
		"event upload flag of a wrong type": {map[string]any{"EnableAllEventUpload": "yes"}, func(string) bool { return true }},
	} {
		t.Run(name, func(t *testing.T) {
			transport := freshServer(t)
			cfg := machineconfiguration.GetUniversalDefaultConfig()
			cfg.BlockedPathRegex = "^/tmp/blocked/"
			if err := configService(t).SetGlobalConfig(cfg); err != nil {
				t.Fatal(err)
			}
			updateItem(t, "GlobalConfig", "Config", tc.attrs)
			handlers = newHandlers()

			res, err := newHost(t, machineA, transport).Sync(t)
			if err != nil {
				t.Fatalf("one bad stored setting made the sync fail for every Mac: %v", err)
			}
			got := preflightOf(t, res)
			if !tc.ok(got) {
				t.Errorf("the bad value was sent to Santa: %s", got)
			}
			if !strings.Contains(got, `"blocked_path_regex":"^/tmp/blocked/"`) {
				t.Errorf("the valid settings must still be sent: %s", got)
			}
		})
	}
}

// Writing such values through the model (and so the CLI) is refused with an error naming the setting; today some are
// stored and some silently ignored.
func TestB5_InvalidConfigValuesAreRejectedWhenWritten(t *testing.T) {
	invalid := map[string]struct {
		mutate func(*machineconfiguration.MachineConfiguration)
		update machineconfiguration.MachineConfigurationUpdateRequest
		hint   string
	}{
		"negative batch size": {func(c *machineconfiguration.MachineConfiguration) { c.BatchSize = -1 },
			machineconfiguration.MachineConfigurationUpdateRequest{BatchSize: intPtr(-1)}, "batch"},
		"huge batch size": {func(c *machineconfiguration.MachineConfiguration) { c.BatchSize = 100000 },
			machineconfiguration.MachineConfigurationUpdateRequest{BatchSize: intPtr(100000)}, "batch"},
		"full sync interval below 60": {func(c *machineconfiguration.MachineConfiguration) { c.FullSyncInterval = 10 },
			machineconfiguration.MachineConfigurationUpdateRequest{FullSyncInterval: intPtr(10)}, "interval"},
		"full sync interval above uint32": {func(c *machineconfiguration.MachineConfiguration) { c.FullSyncInterval = 5000000000 },
			machineconfiguration.MachineConfigurationUpdateRequest{FullSyncInterval: intPtr(5000000000)}, "interval"},
		"unknown client mode": {func(c *machineconfiguration.MachineConfiguration) { c.ClientMode = types.ClientMode(99) },
			machineconfiguration.MachineConfigurationUpdateRequest{ClientMode: clientModePtr(99)}, "client"},
		"unknown file access action": {func(c *machineconfiguration.MachineConfiguration) { c.OverrideFileAccessAction = "BOGUS" },
			machineconfiguration.MachineConfigurationUpdateRequest{OverrideFileAccessAction: fileAccessPtr("BOGUS")}, "file access"},
		"oversized remount flag list": {func(c *machineconfiguration.MachineConfiguration) {
			c.BlockUsbMount = boolPtr(true)
			c.RemountUsbMode = manyFlags(10000)
		}, machineconfiguration.MachineConfigurationUpdateRequest{BlockUsbMount: boolPtr(true), RemountUsbMode: strSlicePtr(manyFlags(10000))}, "remount"},
	}
	for name, tc := range invalid {
		t.Run(name, func(t *testing.T) {
			freshServer(t)
			svc := configService(t)
			valid := machineconfiguration.GetUniversalDefaultConfig()
			if err := svc.SetGlobalConfig(valid); err != nil {
				t.Fatal(err)
			}
			before := getItem(t, "GlobalConfig", "Config")

			check := func(what string, err error) {
				t.Helper()
				if err == nil {
					t.Errorf("%s accepted an invalid value", what)
					return
				}
				if !strings.Contains(strings.ToLower(err.Error()), tc.hint) {
					t.Errorf("%s: the error must name the setting (%q): %v", what, tc.hint, err)
				}
			}
			bad := valid
			tc.mutate(&bad)
			check("SetGlobalConfig", svc.SetGlobalConfig(bad, machineconfiguration.ConfirmGlobalLockdown()))
			check("SetMachineConfig", svc.SetMachineConfig(machineA, bad))
			_, err := svc.UpdateGlobalConfig(tc.update, machineconfiguration.ConfirmGlobalLockdown())
			check("UpdateGlobalConfig", err)
			_, err = svc.UpdateMachineConfig(machineB, tc.update)
			check("UpdateMachineConfig", err)

			if after := getItem(t, "GlobalConfig", "Config"); itemSize(after) != itemSize(before) {
				t.Errorf("a rejected write changed the global config")
			}
			if item := getItem(t, "Machine#"+machineA, "Config"); len(item) != 0 {
				t.Errorf("a rejected write created a machine config")
			}
		})
	}

	// Values within the limits keep working.
	freshServer(t)
	svc := configService(t)
	cfg := machineconfiguration.GetUniversalDefaultConfig()
	cfg.BatchSize, cfg.FullSyncInterval = 500, 60
	if err := svc.SetGlobalConfig(cfg); err != nil {
		t.Fatalf("batch size 500 and full sync interval 60 are valid: %v", err)
	}
	if _, err := svc.UpdateMachineConfig(machineA, machineconfiguration.MachineConfigurationUpdateRequest{BatchSize: intPtr(1), FullSyncInterval: intPtr(86400)}); err != nil {
		t.Fatalf("batch size 1 and a daily full sync are valid: %v", err)
	}
}

func intPtr(i int) *int                     { return &i }
func boolPtr(b bool) *bool                  { return &b }
func strSlicePtr(s []string) *[]string      { return &s }
func clientModePtr(m int) *types.ClientMode { c := types.ClientMode(m); return &c }
func fileAccessPtr(a string) *types.FileAccessAction {
	f := types.FileAccessAction(a)
	return &f
}

// ---------------------------------------------------------------------------------------------------------
// B6: clean sync for one machine
// ---------------------------------------------------------------------------------------------------------

// `rudolph config set --machine <id> --clean-sync` asks for that machine's next sync to be a clean sync, once. It is
// refused for the global config, where it would make the whole fleet clean sync at once.
func TestB6_MachineCleanSyncFlagMakesTheNextSyncCleanOnce(t *testing.T) {
	transport := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	host := newHost(t, machineA, transport)
	host.MustSync(t)
	host.MustSync(t)
	host.Rules[fakesanta.RuleKey{Type: "BINARY", Identifier: sha2}] = fakesanta.Rule{Policy: "ALLOWLIST"} // drifted

	cfg := machineconfiguration.GetUniversalDefaultConfig()
	cfg.CleanSync = true
	if err := configService(t).SetMachineConfig(machineA, cfg); err != nil {
		t.Fatal(err)
	}

	if res := host.MustSync(t); res.SyncType != "CLEAN" {
		t.Fatalf("the sync after setting --clean-sync must be CLEAN, got %s", res.SyncType)
	}
	if host.Policy("BINARY", sha2) != "" || host.Policy("BINARY", sha1) != "BLOCKLIST" {
		t.Fatalf("the clean sync must replace the Mac's rules: %v", host.Rules)
	}
	for i := 0; i < 2; i++ {
		if res := host.MustSync(t); res.SyncType != "NORMAL" {
			t.Fatalf("--clean-sync applies once; sync %d after it was %s", i+1, res.SyncType)
		}
	}
	if res := newHost(t, machineB, transport).MustSync(t); res.SyncType != "CLEAN" {
		t.Fatal("precondition: a new Mac clean syncs")
	}
}

func TestB6_GlobalCleanSyncFlagIsRefused(t *testing.T) {
	freshServer(t)
	svc := configService(t)
	cfg := machineconfiguration.GetUniversalDefaultConfig()
	cfg.CleanSync = true
	if err := svc.SetGlobalConfig(cfg); err == nil {
		t.Error("a global clean sync flag (a fleet-wide clean sync storm) must be refused")
	}
	if _, err := svc.UpdateGlobalConfig(machineconfiguration.MachineConfigurationUpdateRequest{CleanSync: boolPtr(true)}); err == nil {
		t.Error("a global clean sync flag must be refused by update too")
	}
}

// ---------------------------------------------------------------------------------------------------------
// INH (decided): a machine config overrides only what is set on it
// ---------------------------------------------------------------------------------------------------------

func TestINH_MachineConfigFollowsGlobalSettingsItDoesNotSet(t *testing.T) {
	transport := freshServer(t)
	svc := configService(t)
	global := machineconfiguration.GetUniversalDefaultConfig()
	global.AllowedPathRegex = "^/opt/old/"
	if err := svc.SetGlobalConfig(global); err != nil {
		t.Fatal(err)
	}

	// A machine config created to change one USB setting...
	if _, err := svc.UpdateMachineConfig(machineA, machineconfiguration.MachineConfigurationUpdateRequest{BlockUsbMount: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}
	// ...and one that explicitly sets MONITOR.
	monitor := types.Monitor
	if _, err := svc.UpdateMachineConfig(machineB, machineconfiguration.MachineConfigurationUpdateRequest{ClientMode: &monitor}); err != nil {
		t.Fatal(err)
	}

	lockdown := types.Lockdown
	if _, err := svc.UpdateGlobalConfig(machineconfiguration.MachineConfigurationUpdateRequest{
		ClientMode: &lockdown, AllowedPathRegex: strPtr("^/opt/new/"), BatchSize: intPtr(40),
	}, machineconfiguration.ConfirmGlobalLockdown()); err != nil {
		t.Fatal(err)
	}
	handlers = newHandlers()

	a := preflightOf(t, newHost(t, machineA, transport).MustSync(t))
	for _, want := range []string{`"client_mode":"LOCKDOWN"`, `"allowed_path_regex":"^/opt/new/"`, `"batch_size":40`, `"block_usb_mount":true`} {
		if !strings.Contains(a, want) {
			t.Errorf("machine A (only USB set) must follow the global config: missing %s in %s", want, a)
		}
	}
	b := preflightOf(t, newHost(t, machineB, transport).MustSync(t))
	if !strings.Contains(b, `"client_mode":"MONITOR"`) {
		t.Errorf("machine B explicitly set to MONITOR must stay MONITOR: %s", b)
	}
	if !strings.Contains(b, `"allowed_path_regex":"^/opt/new/"`) {
		t.Errorf("machine B sets only its client mode and must follow the global path regex: %s", b)
	}
}

func strPtr(s string) *string { return &s }
