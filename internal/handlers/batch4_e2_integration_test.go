//go:build integration

package handlers

// E2 (batch 4, decided) end to end: with Santa's real event upload batching, one event the sink can never store must
// not block the events after it, and rule download keeps working throughout.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/internal/testutil/fakeaws"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
	"github.com/airbnb/rudolph/pkg/types"
)

func TestE2_OnePoisonEventDoesNotBlockLaterEventsFromTheMac(t *testing.T) {
	aws := fakeaws.Start(t)
	aws.Use(t)
	t.Setenv("HANDLER", "FIREHOSE")
	t.Setenv("FIREHOSE_NAME", "events")
	t.Setenv("LAMBDA_NAME", "")
	transport := freshServer(t)

	cfg := machineconfiguration.GetUniversalDefaultConfig()
	cfg.BatchSize = 3
	if err := machineconfiguration.GetMachineConfigurationService(ddblocal.Client(t), clock.ConcreteTimeProvider{}).SetGlobalConfig(cfg); err != nil {
		t.Fatal(err)
	}
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)

	host := newHost(t, machineA, transport)
	host.SantaExactEventUpload = true
	host.MustSync(t) // first sync is CLEAN: Santa uploads no events during clean syncs

	sha := func(i int) string { return fmt.Sprintf("%064x", 0xe20000+i) }
	for i := 0; i < 8; i++ {
		path := "/Applications/Tool.app/Contents/MacOS"
		if i == 1 {
			path = "/" + strings.Repeat("x", 1_200_000) // larger than a Firehose record can be
		}
		host.PendingEvents = append(host.PendingEvents, fmt.Sprintf(
			`{"file_sha256":%q,"file_path":%q,"file_name":"tool","decision":"BLOCK_UNKNOWN"}`, sha(i), path))
	}

	newRules := []string{sha2, sha3, strings.Repeat("d4", 32)}
	for round, rule := range newRules {
		addGlobalRule(t, rule, types.RuleTypeBinary, types.RulePolicyBlocklist)
		res := host.MustSync(t)
		if res.SyncType != "NORMAL" {
			t.Fatalf("sync %d: expected an incremental sync, got %s", round+1, res.SyncType)
		}
		if host.Policy("BINARY", rule) != "BLOCKLIST" {
			t.Fatalf("sync %d: rule download must keep working while events are pending", round+1)
		}
	}

	delivered := map[string]bool{}
	for _, r := range aws.FirehoseRecords() {
		var e struct {
			FileSHA256 string `json:"file_sha256"`
		}
		if err := json.Unmarshal(r.Data, &e); err != nil {
			t.Fatal(err)
		}
		delivered[e.FileSHA256] = true
	}
	for i := 0; i < 8; i++ {
		if i != 1 && !delivered[sha(i)] {
			t.Errorf("event %d never reached the sink after 3 syncs", i)
		}
	}
	if len(host.PendingEvents) != 0 {
		t.Errorf("the Mac still holds %d events after 3 syncs: one unstorable event blocks every later event", len(host.PendingEvents))
	}
}
