//go:build integration

package handlers

// End-to-end sync tests: a simulated Santa 2026.8 host (internal/testutil/fakesanta) syncs through ApiRouter,
// with handlers booted exactly as in Lambda, against DynamoDB Local. Admin changes go through the same model
// packages the CLI uses. Test names reference IDs in docs/audit/shortcomings.md.

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/internal/testutil/fakesanta"
	"github.com/airbnb/rudolph/internal/testutil/santaproto"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
	"github.com/airbnb/rudolph/pkg/model/machinerules"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestMain(m *testing.M) { os.Exit(ddblocal.Main(m)) }

const (
	machineA = "AAAAAAAA-1111-4111-8111-AAAAAAAAAAAA"
	machineB = "BBBBBBBB-2222-4222-8222-BBBBBBBBBBBB"

	chromeTeamID    = "EQHXZ8M8AV"
	chromeSigningID = "EQHXZ8M8AV:com.google.Chrome"
	cdhash          = "a1b2c3d4e5f60718293a4b5c6d7e8f9001122334"
)

var (
	sha1 = strings.Repeat("a1", 32)
	sha2 = strings.Repeat("b2", 32)
	sha3 = strings.Repeat("c3", 32)
)

// freshServer empties the table and replaces the process-wide handlers so no state (e.g. caches) leaks
// between tests. It returns a transport for simulated hosts.
func freshServer(t *testing.T) fakesanta.Transport {
	t.Helper()
	ddblocal.Reset(t)
	handlers = newHandlers()
	return ApiRouter
}

func newHost(t *testing.T, id string, transport fakesanta.Transport) *fakesanta.Host {
	h := fakesanta.NewHost(id, transport)
	// Most tests are about sync semantics, not transport; P1 tests cover the default deflate encoding.
	h.Encoding = fakesanta.EncodingNone
	return h
}

func addGlobalRule(t *testing.T, identifier string, ruleType types.RuleType, policy types.Policy) {
	t.Helper()
	if err := globalrules.AddNewGlobalRule(clock.ConcreteTimeProvider{}, ddblocal.Client(t), identifier, ruleType, policy, "test"); err != nil {
		t.Fatalf("add global rule: %v", err)
	}
}

func addMachineRule(t *testing.T, machineID, identifier string, ruleType types.RuleType, policy types.Policy, expires time.Time) {
	t.Helper()
	if err := machinerules.AddNewMachineRule(ddblocal.Client(t), machineID, identifier, ruleType, policy, "test", expires); err != nil {
		t.Fatalf("add machine rule: %v", err)
	}
}

func getItem(t *testing.T, pk, sk string) map[string]awstypes.AttributeValue {
	t.Helper()
	out, err := ddblocal.Raw(t).GetItem(context.Background(), &awsdynamodb.GetItemInput{
		TableName: aws.String(ddblocal.TableName),
		Key: map[string]awstypes.AttributeValue{
			"PK": &awstypes.AttributeValueMemberS{Value: pk},
			"SK": &awstypes.AttributeValueMemberS{Value: sk},
		},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.Item
}

func putItem(t *testing.T, item map[string]any) {
	t.Helper()
	av, err := attributevalue.MarshalMap(item)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ddblocal.Raw(t).PutItem(context.Background(), &awsdynamodb.PutItemInput{
		TableName: aws.String(ddblocal.TableName), Item: av,
	}); err != nil {
		t.Fatal(err)
	}
}

func updateItem(t *testing.T, pk, sk string, attrs map[string]any) {
	t.Helper()
	item := getItem(t, pk, sk)
	if item == nil {
		t.Fatalf("no item %s/%s to update", pk, sk)
	}
	extra, err := attributevalue.MarshalMap(attrs)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range extra {
		item[k] = v
	}
	if _, err := ddblocal.Raw(t).PutItem(context.Background(), &awsdynamodb.PutItemInput{
		TableName: aws.String(ddblocal.TableName), Item: item,
	}); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------------------------------------
// Core behaviour we rely on (regression guards)
// ---------------------------------------------------------------------------------------------------------

func TestSync_GlobalAndMachineSpecificRules(t *testing.T) {
	transport := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	addGlobalRule(t, chromeTeamID, types.RuleTypeTeamID, types.RulePolicyAllowlist)
	addMachineRule(t, machineA, sha2, types.RuleTypeBinary, types.RulePolicyAllowlist, time.Now().Add(24*time.Hour))
	// A machine rule overriding a global rule only for machine A.
	addMachineRule(t, machineA, sha1, types.RuleTypeBinary, types.RulePolicyAllowlist, time.Now().Add(24*time.Hour))

	a, b := newHost(t, machineA, transport), newHost(t, machineB, transport)
	a.MustSync(t)
	b.MustSync(t)

	if got := a.Policy("BINARY", sha2); got != "ALLOWLIST" {
		t.Errorf("machine A should have its machine-specific rule, got %q", got)
	}
	if got := b.Policy("BINARY", sha2); got != "" {
		t.Errorf("machine B must not receive machine A's rule, got %q", got)
	}
	if got := a.Policy("BINARY", sha1); got != "ALLOWLIST" {
		t.Errorf("machine rule must override the global rule on A, got %q", got)
	}
	if got := b.Policy("BINARY", sha1); got != "BLOCKLIST" {
		t.Errorf("machine B must keep the global rule, got %q", got)
	}
	if got := b.Policy("TEAMID", chromeTeamID); got != "ALLOWLIST" {
		t.Errorf("global TeamID rule missing on B, got %q", got)
	}
}

func TestSync_IncrementalSyncDeliversNewAndRemovedGlobalRules(t *testing.T) {
	transport := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	host := newHost(t, machineA, transport)
	host.MustSync(t)

	addGlobalRule(t, sha2, types.RuleTypeBinary, types.RulePolicyBlocklist)
	removal := globalrules.ConcreteRuleRemovalService{TimeProvider: clock.ConcreteTimeProvider{}, Getter: ddblocal.Client(t), Transacter: ddblocal.Client(t)}
	if err := removal.RemoveGlobalRule("Binary#"+sha1, ""); err != nil {
		t.Fatal(err)
	}

	res := host.MustSync(t)
	if res.SyncType != "NORMAL" {
		t.Fatalf("second sync should be incremental, got %s", res.SyncType)
	}
	if host.Policy("BINARY", sha2) != "BLOCKLIST" {
		t.Error("new global rule not delivered by incremental sync")
	}
	if host.Policy("BINARY", sha1) != "" {
		t.Error("removed global rule still present after incremental sync")
	}
}

func TestSync_MachineSpecificClientMode(t *testing.T) {
	transport := freshServer(t)
	svc := machineconfiguration.GetMachineConfigurationService(ddblocal.Client(t), clock.ConcreteTimeProvider{})
	global := machineconfiguration.GetUniversalDefaultConfig()
	if err := svc.SetGlobalConfig(global); err != nil {
		t.Fatal(err)
	}
	lockdown := global
	lockdown.ClientMode = types.Lockdown
	if err := svc.SetMachineConfig(machineA, lockdown); err != nil {
		t.Fatal(err)
	}

	a, b := newHost(t, machineA, transport), newHost(t, machineB, transport)
	resA, resB := a.MustSync(t), b.MustSync(t)
	if got := santaproto.Canonical(t, resA.Preflight); !strings.Contains(got, `"client_mode":"LOCKDOWN"`) {
		t.Errorf("machine A should be told LOCKDOWN, preflight: %s", got)
	}
	if got := santaproto.Canonical(t, resB.Preflight); !strings.Contains(got, `"client_mode":"MONITOR"`) {
		t.Errorf("machine B should stay MONITOR, preflight: %s", got)
	}
}

func TestSync_MachineRuleRemovalReachesHost(t *testing.T) {
	transport := freshServer(t)
	addMachineRule(t, machineA, sha1, types.RuleTypeBinary, types.RulePolicyAllowlist, time.Now().Add(24*time.Hour))
	host := newHost(t, machineA, transport)
	host.MustSync(t)
	if host.Policy("BINARY", sha1) != "ALLOWLIST" {
		t.Fatal("precondition: host should have the machine rule")
	}

	if err := machinerules.GetMachineRulesService(ddblocal.Client(t)).Remove(machineA, sha1, types.RuleTypeBinary); err != nil {
		t.Fatal(err)
	}
	host.MustSync(t)
	if got := host.Policy("BINARY", sha1); got != "" {
		t.Fatalf("removed machine rule still on host: %q", got)
	}
}

// Santa 2026.8 only changes a path regex when the preflight response contains the field, and "" clears it.
// Removing a regex from Rudolph's config must therefore still send the field.
func TestSync_PathRegexesCanBeSetAndCleared(t *testing.T) {
	transport := freshServer(t)
	svc := machineconfiguration.GetMachineConfigurationService(ddblocal.Client(t), clock.ConcreteTimeProvider{})
	cfg := machineconfiguration.GetUniversalDefaultConfig()
	cfg.AllowedPathRegex = "^/Applications/Approved/"
	cfg.BlockedPathRegex = "^/Users/[^/]+/Downloads/"
	if err := svc.SetGlobalConfig(cfg); err != nil {
		t.Fatal(err)
	}
	host := newHost(t, machineA, transport)
	host.MustSync(t)
	if host.AllowedPathRegex != cfg.AllowedPathRegex || host.BlockedPathRegex != cfg.BlockedPathRegex {
		t.Fatalf("regexes not applied on host: allowed %q blocked %q", host.AllowedPathRegex, host.BlockedPathRegex)
	}

	cfg.AllowedPathRegex, cfg.BlockedPathRegex = "", ""
	if err := svc.SetGlobalConfig(cfg); err != nil {
		t.Fatal(err)
	}
	handlers = newHandlers() // a new Lambda instance: no cached global config
	host.MustSync(t)
	if host.AllowedPathRegex != "" || host.BlockedPathRegex != "" {
		t.Fatalf("regexes removed from config are still applied on host: allowed %q blocked %q", host.AllowedPathRegex, host.BlockedPathRegex)
	}
}

// ---------------------------------------------------------------------------------------------------------
// Protocol (P)
// ---------------------------------------------------------------------------------------------------------

func TestP1_DefaultSantaCompressionCanSync(t *testing.T) {
	for _, enc := range []fakesanta.Encoding{fakesanta.EncodingDeflate, fakesanta.EncodingGzip} {
		t.Run(string(enc), func(t *testing.T) {
			transport := freshServer(t)
			addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
			host := fakesanta.NewHost(machineA, transport)
			host.Encoding = enc
			host.PendingEvents = []string{`{"file_sha256":"` + sha3 + `","file_path":"/tmp","file_name":"x","decision":"BLOCK_UNKNOWN"}`}
			host.MustSync(t)
			if host.Policy("BINARY", sha1) != "BLOCKLIST" {
				t.Fatal("rules not delivered to a host using compressed requests")
			}
		})
	}
}

func TestP2_StandaloneModeHostCanSync(t *testing.T) {
	transport := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	host := newHost(t, machineA, transport)
	host.ClientMode = "STANDALONE"
	host.MustSync(t)
	if host.Policy("BINARY", sha1) != "BLOCKLIST" {
		t.Fatal("standalone host did not receive rules")
	}
}

func TestP3_HostsWithOnlyNewerRuleTypesDoNotCleanSyncEveryTime(t *testing.T) {
	for name, add := range map[string]func(){
		"teamid":    func() { addGlobalRule(t, chromeTeamID, types.RuleTypeTeamID, types.RulePolicyAllowlist) },
		"signingid": func() { addGlobalRule(t, chromeSigningID, types.RuleTypeSigningID, types.RulePolicyAllowlist) },
	} {
		t.Run(name, func(t *testing.T) {
			transport := freshServer(t)
			add()
			host := newHost(t, machineA, transport)
			if first := host.MustSync(t); first.SyncType != "CLEAN" {
				t.Fatalf("first sync should be CLEAN, got %s", first.SyncType)
			}
			if second := host.MustSync(t); second.SyncType != "NORMAL" {
				t.Fatalf("host holding rules must not be forced into another clean sync, got %s", second.SyncType)
			}
		})
	}
}

func TestP4_PreflightSendsConfiguredSantaSettings(t *testing.T) {
	transport := freshServer(t)
	svc := machineconfiguration.GetMachineConfigurationService(ddblocal.Client(t), clock.ConcreteTimeProvider{})
	if err := svc.SetGlobalConfig(machineconfiguration.GetUniversalDefaultConfig()); err != nil {
		t.Fatal(err)
	}
	// Storage contract for settings that exist in the Santa protocol but not yet in Rudolph's config model.
	updateItem(t, "GlobalConfig", "Config", map[string]any{
		"EnableAllEventUpload":      true,
		"DisableUnknownEventUpload": false,
		"OverrideFileAccessAction":  "AUDIT_ONLY",
		"BlockUsbMount":             true,
		"RemountUsbMode":            []string{"rdonly", "noexec"},
		"EventDetailURL":            "https://rudolph.example.com/blocked/%file_sha%",
		"EventDetailText":           "Request access",
	})

	res := newHost(t, machineA, transport).MustSync(t)
	got := santaproto.Canonical(t, res.Preflight)
	for _, want := range []string{
		`"enable_all_event_upload":true`,
		`"override_file_access_action":"AUDIT_ONLY"`,
		`"block_usb_mount":true`,
		`"remount_usb_mode":["rdonly","noexec"]`,
		`"event_detail_url":"https://rudolph.example.com/blocked/%file_sha%"`,
		`"event_detail_text":"Request access"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("preflight response missing %s\nparsed response: %s", want, got)
		}
	}
}

func TestP5_NewerRuleTypesPoliciesAndFieldsReachHosts(t *testing.T) {
	transport := freshServer(t)
	// Storage contract: RuleType 5 = CDHASH; Policy 7 = CEL, 8 = SILENT_GUI_BLOCKLIST, 9 = SILENT_TTY_BLOCKLIST.
	putItem(t, map[string]any{"PK": "GlobalRules", "SK": "CDHash#" + cdhash, "RuleType": 5, "Policy": 2, "Identifier": cdhash,
		"CustomMessage": "Blocked build", "CustomURL": "https://it.example.com/%file_sha%", "EventDetailButtonLabel": "Get help"})
	putItem(t, map[string]any{"PK": "GlobalRules", "SK": "SigningID#" + chromeSigningID, "RuleType": 3, "Policy": 7, "Identifier": chromeSigningID,
		"CELExpr": "target.signing_time >= timestamp('2025-01-01T00:00:00Z')"})
	putItem(t, map[string]any{"PK": "GlobalRules", "SK": "Binary#" + sha1, "RuleType": 1, "Policy": 8, "Identifier": sha1})
	putItem(t, map[string]any{"PK": "GlobalRules", "SK": "Binary#" + sha2, "RuleType": 1, "Policy": 9, "Identifier": sha2})

	host := newHost(t, machineA, transport)
	host.MustSync(t)

	cd := host.Rules[fakesanta.RuleKey{Type: "CDHASH", Identifier: cdhash}]
	if cd.Policy != "BLOCKLIST" || cd.CustomMsg != "Blocked build" || cd.CustomURL != "https://it.example.com/%file_sha%" {
		t.Errorf("CDHASH block rule not delivered intact: %+v", cd)
	}
	cel := host.Rules[fakesanta.RuleKey{Type: "SIGNINGID", Identifier: chromeSigningID}]
	if cel.Policy != "CEL" || cel.CELExpr == "" {
		t.Errorf("CEL rule not delivered intact: %+v", cel)
	}
	if got := host.Policy("BINARY", sha1); got != "SILENT_GUI_BLOCKLIST" {
		t.Errorf("SILENT_GUI_BLOCKLIST rule = %q", got)
	}
	if got := host.Policy("BINARY", sha2); got != "SILENT_TTY_BLOCKLIST" {
		t.Errorf("SILENT_TTY_BLOCKLIST rule = %q", got)
	}
}

func TestP5_TransitivePolicyIsNeverSentToHosts(t *testing.T) {
	transport := freshServer(t)
	// Rudolph accepts ALLOWLIST_TRANSITIVE today; Santa has no such policy and silently drops the rule.
	_ = globalrules.AddNewGlobalRule(clock.ConcreteTimeProvider{}, ddblocal.Client(t), sha1, types.RuleTypeBinary, types.RulePolicyAllowlistTransitive, "test")

	res := newHost(t, machineA, transport).MustSync(t)
	for _, d := range res.Downloaded {
		if d.Skipped {
			t.Errorf("server sent a rule Santa cannot parse (it is silently ignored on the host): %+v", d)
		}
	}
}

func TestP7_PreflightRecordsHostInventory(t *testing.T) {
	transport := freshServer(t)
	host := newHost(t, machineA, transport)
	host.MustSync(t)

	item := getItem(t, "Machine#"+machineA, "Current")
	var got map[string]any
	if err := attributevalue.UnmarshalMap(item, &got); err != nil {
		t.Fatal(err)
	}
	for attr, want := range map[string]any{
		"Hostname":        host.Hostname,
		"ModelIdentifier": host.ModelID,
		"SIPStatus":       float64(127),
		"SerialNum":       host.SerialNumber,
		"PrimaryUser":     host.PrimaryUser,
	} {
		if got[attr] != want {
			t.Errorf("sensor data %s = %v, want %v", attr, got[attr], want)
		}
	}
}

func TestP8_PostflightRecordsSyncOutcome(t *testing.T) {
	transport := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	newHost(t, machineA, transport).MustSync(t)

	var state map[string]any
	if err := attributevalue.UnmarshalMap(getItem(t, "Machine#"+machineA, "SyncState"), &state); err != nil {
		t.Fatal(err)
	}
	if state["LastSyncType"] != "CLEAN" || state["RulesProcessed"] != float64(1) {
		t.Fatalf("sync state should record the reported sync type and rules processed, got %v", state)
	}
}

// ---------------------------------------------------------------------------------------------------------
// Logic (L)
// ---------------------------------------------------------------------------------------------------------

func TestL3_UpdatingConfigForMachineWithoutOverrideWorks(t *testing.T) {
	transport := freshServer(t)
	svc := machineconfiguration.GetMachineConfigurationService(ddblocal.Client(t), clock.ConcreteTimeProvider{})
	if err := svc.SetGlobalConfig(machineconfiguration.GetUniversalDefaultConfig()); err != nil {
		t.Fatal(err)
	}
	mode := types.Lockdown
	if _, err := svc.UpdateMachineConfig(machineA, machineconfiguration.MachineConfigurationUpdateRequest{ClientMode: &mode}); err != nil {
		t.Fatalf("config update for a machine without an existing override failed: %v", err)
	}

	res := newHost(t, machineA, transport).MustSync(t)
	if got := santaproto.Canonical(t, res.Preflight); !strings.Contains(got, `"client_mode":"LOCKDOWN"`) {
		t.Errorf("machine should now be LOCKDOWN, preflight: %s", got)
	}
	if dt := getItem(t, "Machine#"+machineA, "Config")["DataType"]; dt == nil || dt.(*awstypes.AttributeValueMemberS).Value != "MachineConfig" {
		t.Errorf("machine config row must have DataType MachineConfig, got %#v", dt)
	}
}

func TestL5_UpdatingNonExistentGlobalRuleDoesNotBreakCleanSyncs(t *testing.T) {
	transport := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)

	err := globalrules.UpdateGlobalRule(clock.ConcreteTimeProvider{}, ddblocal.Client(t), sha2, types.RuleTypeBinary, types.RulePolicyAllowlist)
	if err == nil {
		t.Error("updating a rule that does not exist should return an error")
	}

	host := newHost(t, machineA, transport)
	host.MustSync(t)
	if host.Policy("BINARY", sha1) != "BLOCKLIST" {
		t.Fatal("existing global rule missing after clean sync")
	}
}

func TestL6_ExpiredMachineRulesAreNotServed(t *testing.T) {
	transport := freshServer(t)
	addMachineRule(t, machineA, sha1, types.RuleTypeBinary, types.RulePolicyAllowlist, time.Now().Add(-time.Minute))
	host := newHost(t, machineA, transport)
	host.MustSync(t)
	if got := host.Policy("BINARY", sha1); got != "" {
		t.Fatalf("expired machine rule (not yet deleted by TTL) was delivered: %q", got)
	}
}

func TestL6_MachineRuleExpiryRemovesRuleFromHost(t *testing.T) {
	transport := freshServer(t)
	addMachineRule(t, machineA, sha1, types.RuleTypeBinary, types.RulePolicyAllowlist, time.Now().Add(2*time.Second))
	host := newHost(t, machineA, transport)
	host.MustSync(t)
	if host.Policy("BINARY", sha1) != "ALLOWLIST" {
		t.Fatal("precondition: temporary allow rule should be on the host")
	}

	time.Sleep(3 * time.Second)
	// Simulate DynamoDB TTL deleting the expired row, as AWS eventually does.
	if _, err := ddblocal.Raw(t).DeleteItem(context.Background(), &awsdynamodb.DeleteItemInput{
		TableName: aws.String(ddblocal.TableName),
		Key: map[string]awstypes.AttributeValue{
			"PK": &awstypes.AttributeValueMemberS{Value: "MachineRules#" + machineA},
			"SK": &awstypes.AttributeValueMemberS{Value: "Binary#" + sha1},
		},
	}); err != nil {
		t.Fatal(err)
	}

	host.MustSync(t)
	if got := host.Policy("BINARY", sha1); got != "" {
		t.Fatalf("expired temporary rule is still enforced on the host: %q", got)
	}
}

func TestL7_MachineRuleRemovedDuringInFlightSyncStillReachesHost(t *testing.T) {
	transport := freshServer(t)
	addMachineRule(t, machineA, sha1, types.RuleTypeBinary, types.RulePolicyAllowlist, time.Now().Add(24*time.Hour))
	host := newHost(t, machineA, transport)
	host.MustSync(t)

	host.BeforePostflight = func() {
		if err := machinerules.GetMachineRulesService(ddblocal.Client(t)).Remove(machineA, sha1, types.RuleTypeBinary); err != nil {
			t.Fatal(err)
		}
	}
	host.MustSync(t)
	host.BeforePostflight = nil

	host.MustSync(t)
	if got := host.Policy("BINARY", sha1); got != "" {
		t.Fatalf("rule removed while a sync was in flight was never removed from the host: %q", got)
	}
}

// L8 turned out not to be a bug (postflight replaces the empty cursor); kept as a regression guard.
func TestL8_RequestedCleanSyncDoesNotCauseASecondCleanSync(t *testing.T) {
	transport := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	host := newHost(t, machineA, transport)
	host.MustSync(t)
	host.MustSync(t)

	host.RequestCleanSync = true
	if res := host.MustSync(t); res.SyncType != "CLEAN" {
		t.Fatalf("requested clean sync should be CLEAN, got %s", res.SyncType)
	}
	if res := host.MustSync(t); res.SyncType != "NORMAL" {
		t.Fatalf("sync after a requested clean sync should be NORMAL, got %s", res.SyncType)
	}
}

func TestL9_FeedCursorNeverSkipsRulesAddedDuringASlowSync(t *testing.T) {
	transport := freshServer(t)
	now := time.Now().UTC()
	preflightAt := now.Add(-21 * time.Minute)
	putItem(t, map[string]any{
		"PK": "Machine#" + machineA, "SK": "SyncState", "MachineID": machineA, "DataType": "SyncState",
		"BatchSize": 50, "CleanSync": false, "LastCleanSync": clock.RFC3339(now.Add(-time.Hour)),
		"FeedSyncCursor":        clock.RFC3339(now.Add(-time.Hour)),
		"PreflightAt":           clock.RFC3339(preflightAt),
		"RuledownloadStartedAt": clock.RFC3339(now.Add(-20 * time.Minute)),
	})

	body := santaproto.ClientJSON(t, "PostflightRequest", `{"machine_id":"`+machineA+`","rules_received":0,"rules_processed":0,"sync_type":"NORMAL"}`)
	resp, err := transport(events.APIGatewayProxyRequest{
		HTTPMethod: "POST", Resource: "/postflight/{machine_id}", Path: "/postflight/" + machineA,
		PathParameters: map[string]string{"machine_id": machineA},
		Headers:        map[string]string{"Content-Type": "application/json"},
		Body:           string(body),
	})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("postflight failed: %v %+v", err, resp)
	}

	cursorAttr := getItem(t, "Machine#"+machineA, "SyncState")["FeedSyncCursor"].(*awstypes.AttributeValueMemberS).Value
	cursor, err := clock.ParseRFC3339(cursorAttr)
	if err != nil {
		t.Fatal(err)
	}
	if cursor.After(preflightAt) {
		t.Fatalf("feed cursor %s is after the sync started (%s); rules added in between are never downloaded", cursor, preflightAt)
	}
}

// ---------------------------------------------------------------------------------------------------------
// Security (S) and cost (C)
// ---------------------------------------------------------------------------------------------------------

func TestS2_SyncDoesNotLogPayloads(t *testing.T) {
	transport := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	host := newHost(t, machineA, transport)
	host.PrimaryUser = "very-private-user"
	host.PendingEvents = []string{`{"file_sha256":"` + sha3 + `","file_path":"/Users/very-private-user/Downloads","file_name":"secret-tool","decision":"BLOCK_UNKNOWN","executing_user":"very-private-user"}`}

	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)
	host.MustSync(t)

	for _, secret := range []string{"very-private-user", "secret-tool", sha1, host.SerialNumber} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("logs contain payload data %q", secret)
		}
	}
}

func TestC3_SteadyStateSyncIsCheapOnDynamoDB(t *testing.T) {
	transport := freshServer(t)
	for i := 0; i < 120; i++ {
		addGlobalRule(t, fmt.Sprintf("%064x", i+1), types.RuleTypeBinary, types.RulePolicyBlocklist)
	}
	host := newHost(t, machineA, transport)
	host.MustSync(t) // clean sync

	ddblocal.ResetCounts()
	host.MustSync(t) // nothing changed since
	counts := ddblocal.Counts()

	writes := counts["PutItem"] + counts["UpdateItem"] + counts["DeleteItem"] + counts["BatchWriteItem"] + counts["TransactWriteItems"]
	reads := counts["GetItem"] + counts["Query"] + counts["BatchGetItem"]
	if counts["Scan"] > 0 {
		t.Errorf("sync must never Scan: %v", counts)
	}
	if writes > 3 {
		t.Errorf("steady-state sync made %d DynamoDB writes, budget is 3: %v", writes, counts)
	}
	if reads > 5 {
		t.Errorf("steady-state sync made %d DynamoDB reads, budget is 5: %v", reads, counts)
	}
}
