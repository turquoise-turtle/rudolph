//go:build integration

package handlers

// fix/scale: rule pages are sized by response size, not batch_size. A clean sync that the previous version started
// during the deployment, paging global rules one at a time (batch_size 1), reaches this version midway.
// TestUPG_RowsFromEarlierVersionsKeepWorking covers such a cursor with one global rule left; this one leaves half of
// 60, which this version sends in one page.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/airbnb/rudolph/pkg/types"
)

func TestScale_OldBatchSizeOneCursorMidwayThroughGlobalRulesCompletesTheSync(t *testing.T) {
	const total, sentByOld = 60, 30
	oldPreflight := `{"client_mode":"MONITOR","blocked_path_regex":"","allowed_path_regex":"","batch_size":1,"enable_bundles":false,"enable_transitive_rules":false,"full_sync_interval":600,"sync_type":"clean"}`
	sha := func(i int) string { return fmt.Sprintf("%064x", i) }

	base := freshServer(t)
	for i := 1; i <= total; i++ {
		addGlobalRule(t, sha(i), types.RuleTypeBinary, types.RulePolicyBlocklist)
	}
	// What the old version had sent when it was replaced: its first 30 pages of one rule each, and the cursor of page 31
	var first []string
	for i := 1; i <= sentByOld; i++ {
		first = append(first, `{"rule_type":"BINARY","policy":"BLOCKLIST","identifier":"`+sha(i)+`"}`)
	}
	cursor, _ := json.Marshal(map[string]any{"strategy": 1, "page": sentByOld + 1, "batch_size": 1, "pk": "GlobalRules", "sk": "Binary#" + sha(sentByOld)})
	quoted, _ := json.Marshal(string(cursor))
	firstPage := `{"rules":[` + strings.Join(first, ",") + `],"cursor":` + string(quoted) + `}`

	host := newHost(t, machineA, oldInFlight(t, base, oldVersions[0], oldPreflight, firstPage, true))
	res, err := host.Sync(t)
	if err != nil {
		t.Fatalf("the sync the old version started failed after the upgrade: %v", err)
	}
	if res.SyncType != "CLEAN" {
		t.Fatalf("the sync was %s, want the old version's clean sync", res.SyncType)
	}
	if res.RulePages != 2 {
		t.Errorf("the sync took %d rule pages, want the old version's and one page of the remaining %d rules", res.RulePages, total-sentByOld)
	}
	seen := map[string]int{}
	for _, d := range res.Downloaded {
		seen[d.Key.Identifier]++
	}
	for i := 1; i <= total; i++ {
		if seen[sha(i)] != 1 {
			t.Errorf("global rule %d was downloaded %d times, want once", i, seen[sha(i)])
		}
		if got := host.Policy("BINARY", sha(i)); got != "BLOCKLIST" {
			t.Errorf("after the sync the Mac has %q for global rule %d, want BLOCKLIST", got, i)
		}
	}
	if len(res.Downloaded) != total {
		t.Errorf("the sync downloaded %d rules, want %d", len(res.Downloaded), total)
	}

	// And the next sync, entirely this version's, works
	if _, err := host.Sync(t); err != nil {
		t.Fatalf("the sync after it failed: %v", err)
	}
	if len(host.Rules) != total {
		t.Errorf("the Mac holds %d rules after the next sync, want %d", len(host.Rules), total)
	}
}
