//go:build integration

package handlers

// WEBCONTRACT (batch 4): testdata/webcontract/rows.json holds the DynamoDB rows Rudolph writes, generated through its
// real code paths, for rudolph-web to decode (see testdata/webcontract/README.md). This test regenerates the rows and
// fails when an attribute in the fixture disappears, changes type, or changes its (normalized) value. New attributes
// and new rows are allowed. Regenerate the fixture with WEBCONTRACT_UPDATE=1 when a change is intended, and tell the
// rudolph-web maintainers.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
	"github.com/airbnb/rudolph/pkg/model/machinerules"
	"github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/types"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func webContractFile() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", "webcontract", "rows.json")
}

// generateWebContractRows writes every kind of row through the code paths that write it in production.
func generateWebContractRows(t *testing.T) {
	transport := freshServer(t)
	svc := configService(t)
	tp := clock.ConcreteTimeProvider{}
	client := ddblocal.Client(t)

	// Configs
	global := machineconfiguration.GetUniversalDefaultConfig()
	global.BlockedPathRegex = "^/Users/[^/]+/Downloads/"
	if err := svc.SetGlobalConfig(global); err != nil {
		t.Fatal(err)
	}
	action := types.FileAccessActionAuditOnly
	if _, err := svc.UpdateGlobalConfig(machineconfiguration.MachineConfigurationUpdateRequest{
		EnableAllEventUpload: boolPtr(true), OverrideFileAccessAction: &action,
		EventDetailURL: strPtr("https://rudolph.example.com/%file_sha%"), EventDetailText: strPtr("Request access"),
	}); err != nil {
		t.Fatal(err)
	}
	// `config set --machine`: every setting given explicitly (a machine config created by `config update` only holds
	// what was set on it once machine configs inherit the rest, see INH, so it is not part of the contract).
	machine := machineconfiguration.GetUniversalDefaultConfig()
	machine.ClientMode = types.Lockdown
	machine.BlockUsbMount = boolPtr(true)
	machine.RemountUsbMode = []string{"rdonly", "noexec"}
	if err := svc.SetMachineConfig(machineB, machine); err != nil {
		t.Fatal(err)
	}

	// Global rules of every type and policy
	add := func(id string, rt types.RuleType, p types.Policy, options ...rules.RuleOption) {
		if err := globalrules.AddNewGlobalRule(tp, client, id, rt, p, "web contract", options...); err != nil {
			t.Fatalf("add global %s: %v", id, err)
		}
	}
	add(shaN(1), types.RuleTypeBinary, types.RulePolicyBlocklist, rules.WithCustomMessage("Blocked"), rules.WithCustomURL("https://it.example.com/%file_sha%"), rules.WithEventDetailButtonLabel("Get help"))
	add(shaN(2), types.RuleTypeBinary, types.RulePolicyAllowlist, rules.WithNotificationAppName("Tool"))
	add(shaN(3), types.RuleTypeBinary, types.RulePolicySilentBlocklist)
	add(shaN(4), types.RuleTypeBinary, types.RulePolicyAllowlistCompiler)
	add(shaN(5), types.RuleTypeBinary, types.RulePolicySilentGUIBlocklist)
	add(shaN(6), types.RuleTypeBinary, types.RulePolicySilentTTYBlocklist)
	add(shaN(7), types.RuleTypeCertificate, types.RulePolicyAllowlist)
	add(chromeTeamID, types.RuleTypeTeamID, types.RulePolicyAllowlist)
	add(chromeSigningID, types.RuleTypeSigningID, types.RulePolicyCEL, rules.WithCELExpr("target.signing_time >= timestamp('2025-01-01T00:00:00Z')"))
	add(cdhash, types.RuleTypeCDHash, types.RulePolicyBlocklist)

	// Machine rules, a Mac that synced, then a removal and an expired rule
	mr := machinerules.GetMachineRulesService(client)
	addMachine := func(id string, p types.Policy, expires time.Time) {
		if err := machinerules.AddNewMachineRule(client, machineA, id, types.RuleTypeBinary, p, "web contract", expires); err != nil {
			t.Fatalf("add machine %s: %v", id, err)
		}
	}
	addMachine(shaN(11), types.RulePolicyAllowlist, time.Now().Add(24*time.Hour))
	addMachine(shaN(1), types.RulePolicyAllowlist, time.Now().Add(24*time.Hour)) // overrides a global rule
	addMachine(shaN(12), types.RulePolicyBlocklist, time.Now().Add(24*time.Hour))
	host := newHost(t, machineA, transport)
	host.RulesHash = "0123456789abcdef"
	host.MustSync(t)
	host.MustSync(t)
	if err := mr.Remove(machineA, shaN(1), types.RuleTypeBinary); err != nil { // falls back to the global rule
		t.Fatal(err)
	}
	if err := mr.Remove(machineA, shaN(12), types.RuleTypeBinary); err != nil { // no global rule: REMOVE
		t.Fatal(err)
	}
	addMachine(shaN(13), types.RulePolicyAllowlist, time.Now().Add(-time.Hour)) // expired, not yet deleted by TTL
}

var (
	rfc3339Prefix = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z`)
	utcDay        = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	volatileN     = map[string]bool{"ExpiresAfter": true, "SyncSeq": true, "Seq": true, "RemovalFirstSentInSync": true}
	volatileS     = map[string]bool{"MachineRulesTracked": true}
)

// toDynamoJSON converts an attribute value to DynamoDB JSON, normalizing values that change between runs.
func toDynamoJSON(name string, v awstypes.AttributeValue) any {
	switch v := v.(type) {
	case *awstypes.AttributeValueMemberS:
		s := v.Value
		if volatileS[name] && s != "" && s != "dirty" {
			s = "<digest>"
		}
		s = rfc3339Prefix.ReplaceAllString(s, "<timestamp>")
		if name == "LastSeenDay" && utcDay.MatchString(s) {
			s = "<day>"
		}
		return map[string]any{"S": s}
	case *awstypes.AttributeValueMemberN:
		if volatileN[name] {
			return map[string]any{"N": "<number>"}
		}
		return map[string]any{"N": v.Value}
	case *awstypes.AttributeValueMemberBOOL:
		return map[string]any{"BOOL": v.Value}
	case *awstypes.AttributeValueMemberNULL:
		return map[string]any{"NULL": true}
	case *awstypes.AttributeValueMemberB:
		return map[string]any{"B": base64.StdEncoding.EncodeToString(v.Value)}
	case *awstypes.AttributeValueMemberSS:
		return map[string]any{"SS": v.Value}
	case *awstypes.AttributeValueMemberNS:
		return map[string]any{"NS": v.Value}
	case *awstypes.AttributeValueMemberL:
		var out []any
		for _, e := range v.Value {
			out = append(out, toDynamoJSON(name, e))
		}
		return map[string]any{"L": out}
	case *awstypes.AttributeValueMemberM:
		out := map[string]any{}
		for k, e := range v.Value {
			out[k] = toDynamoJSON(k, e)
		}
		return map[string]any{"M": out}
	}
	return map[string]any{"?": fmt.Sprintf("%T", v)}
}

func currentWebContractRows(t *testing.T) []map[string]any {
	t.Helper()
	generateWebContractRows(t)
	var rows []map[string]any
	for _, item := range scanAll(t) {
		row := map[string]any{}
		for k, v := range item {
			row[k] = toDynamoJSON(k, v)
		}
		rows = append(rows, row)
	}
	// Round-trip through JSON so fixture and generated rows have the same Go types.
	raw, _ := json.Marshal(rows)
	rows = nil
	_ = json.Unmarshal(raw, &rows)
	sort.Slice(rows, func(i, j int) bool { return rowKey(rows[i]) < rowKey(rows[j]) })
	return rows
}

func rowKey(row map[string]any) string {
	s := func(k string) string {
		m, _ := row[k].(map[string]any)
		v, _ := m["S"].(string)
		return v
	}
	return s("PK") + " | " + s("SK")
}

// contains reports whether got has everything want has: same keys, types and values, recursively for maps (extra
// keys in got are allowed); lists must match exactly.
func contains(want, got any, path string) []string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return []string{fmt.Sprintf("%s: was %v, now %v", path, want, got)}
		}
		var diffs []string
		for k, wv := range w {
			gv, ok := g[k]
			if !ok {
				diffs = append(diffs, fmt.Sprintf("%s.%s: missing (was %v)", path, k, wv))
				continue
			}
			diffs = append(diffs, contains(wv, gv, path+"."+k)...)
		}
		return diffs
	default:
		if !reflect.DeepEqual(want, got) {
			return []string{fmt.Sprintf("%s: was %v, now %v", path, want, got)}
		}
		return nil
	}
}

func TestWEBCONTRACT_StoredRowsMatchTheFixture(t *testing.T) {
	rows := currentWebContractRows(t)
	if os.Getenv("WEBCONTRACT_UPDATE") != "" {
		var out bytes.Buffer
		encoder := json.NewEncoder(&out)
		encoder.SetEscapeHTML(false)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(rows); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(webContractFile(), out.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %d rows to %s", len(rows), webContractFile())
		return
	}

	raw, err := os.ReadFile(webContractFile())
	if err != nil {
		t.Fatalf("read the fixture (regenerate with WEBCONTRACT_UPDATE=1): %v", err)
	}
	var fixture []map[string]any
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	byKey := map[string][]map[string]any{}
	for _, row := range rows {
		byKey[rowKey(row)] = append(byKey[rowKey(row)], row)
	}
	for _, want := range fixture {
		candidates := byKey[rowKey(want)]
		if len(candidates) == 0 {
			t.Errorf("row %s is no longer written", rowKey(want))
			continue
		}
		var best []string
		for i, got := range candidates {
			diffs := contains(want, got, "")
			if i == 0 || len(diffs) < len(best) {
				best = diffs
			}
		}
		if len(best) > 0 {
			t.Errorf("row %s changed in a way rudolph-web may not decode:\n  %s", rowKey(want), strings.Join(best, "\n  "))
		}
	}
	if len(rows) > len(fixture) {
		t.Logf("%d rows are written that the fixture does not have yet (allowed; regenerate to document them)", len(rows)-len(fixture))
	}
}
