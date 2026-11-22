//go:build integration

package fleet_test

// WEBCONTRACT for the fleet job's rows: testdata/webcontract/fleet-rows.json holds the pointer and one row of each kind
// of list (a health list, readiness for All, for a group and for rudolph:ungrouped) that rudolph-web reads, from a
// fixed scenario run at a fixed time. The snapshot ID, which differs on every run, is written as `<snapshot>`. As for
// rows.json (internal/handlers/webcontract_integration_test.go), the test fails when an attribute of the fixture
// disappears, changes type or changes value; new attributes and rows are allowed. Regenerate with
// WEBCONTRACT_UPDATE=1 after an intended change, and tell the rudolph-web maintainers.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func fleetContractFile() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", "webcontract", "fleet-rows.json")
}

func contractJSON(v types.AttributeValue, snapshot string) any {
	switch v := v.(type) {
	case *types.AttributeValueMemberS:
		return map[string]any{"S": strings.ReplaceAll(v.Value, snapshot, "<snapshot>")}
	case *types.AttributeValueMemberN:
		return map[string]any{"N": v.Value}
	case *types.AttributeValueMemberBOOL:
		return map[string]any{"BOOL": v.Value}
	case *types.AttributeValueMemberSS:
		return map[string]any{"SS": v.Value}
	case *types.AttributeValueMemberL:
		var out []any
		for _, e := range v.Value {
			out = append(out, contractJSON(e, snapshot))
		}
		return map[string]any{"L": out}
	case *types.AttributeValueMemberM:
		out := map[string]any{}
		for k, e := range v.Value {
			out[k] = contractJSON(e, snapshot)
		}
		return map[string]any{"M": out}
	}
	return map[string]any{"?": fmt.Sprintf("%T", v)}
}

func currentFleetContractRows(t *testing.T) []map[string]any {
	t.Helper()
	reset(t)
	registry(t, group{name: "eng"})
	ready := mac{id: machineID(1), hostname: "contract-few", serial: "C02FEW", assigned: []string{"eng"}}
	stale := mac{id: machineID(2), hostname: "contract-stale", serial: "C02STALE", preflightAt: now.AddDate(0, 0, -20)}
	seed(t, ready, stale)
	markers(t, ready.id, marker{sha: sha(1), signingID: "ABCDE12345:com.example.tool", teamID: "ABCDE12345"})
	run(t)
	p := mustPointer(t)

	items := []map[string]types.AttributeValue{p.raw}
	for _, list := range []string{"Health#Stale", "Ready#All", "Ready#Group#eng", "Ready#Group#rudolph:ungrouped"} {
		items = append(items, rows(t, p, list)...)
	}
	var out []map[string]any
	for _, item := range items {
		row := map[string]any{}
		for k, v := range item {
			row[k] = contractJSON(v, p.Snapshot)
		}
		out = append(out, row)
	}
	raw, _ := json.Marshal(out)
	out = nil
	_ = json.Unmarshal(raw, &out)
	sort.Slice(out, func(i, j int) bool { return contractKey(out[i]) < contractKey(out[j]) })
	return out
}

func contractKey(row map[string]any) string {
	get := func(k string) string {
		m, _ := row[k].(map[string]any)
		v, _ := m["S"].(string)
		return v
	}
	return get("PK") + " | " + get("SK")
}

// containsAll reports the differences where got lacks or changes something want has (extra keys in got are allowed;
// lists must match exactly).
func containsAll(want, got any, path string) []string {
	w, ok := want.(map[string]any)
	if !ok {
		if !reflect.DeepEqual(want, got) {
			return []string{fmt.Sprintf("%s: was %v, now %v", path, want, got)}
		}
		return nil
	}
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
		diffs = append(diffs, containsAll(wv, gv, path+"."+k)...)
	}
	return diffs
}

func TestWEBCONTRACT_FleetRows(t *testing.T) {
	rows := currentFleetContractRows(t)
	if os.Getenv("WEBCONTRACT_UPDATE") != "" {
		var out bytes.Buffer
		encoder := json.NewEncoder(&out)
		encoder.SetEscapeHTML(false)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(rows); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fleetContractFile(), out.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %d rows to %s", len(rows), fleetContractFile())
		return
	}
	raw, err := os.ReadFile(fleetContractFile())
	if err != nil {
		t.Fatalf("read the fixture (regenerate with WEBCONTRACT_UPDATE=1): %v", err)
	}
	var fixture []map[string]any
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	byKey := map[string]map[string]any{}
	for _, row := range rows {
		byKey[contractKey(row)] = row
	}
	for _, want := range fixture {
		got, ok := byKey[contractKey(want)]
		if !ok {
			t.Errorf("row %s is no longer written", contractKey(want))
			continue
		}
		if diffs := containsAll(want, got, ""); len(diffs) > 0 {
			t.Errorf("row %s changed in a way rudolph-web may not decode:\n  %s", contractKey(want), strings.Join(diffs, "\n  "))
		}
	}
}
