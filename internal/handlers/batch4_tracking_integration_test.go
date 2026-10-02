//go:build integration

package handlers

// Batch 3 shipped parts of machine rule delivery tracking without tests (B3L1L2H1b), and left one race (TX).

import (
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/model/machinerules"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-lambda-go/events"
)

func removeMachineRule(t *testing.T, machineID, identifier string) {
	t.Helper()
	if err := machinerules.GetMachineRulesService(ddblocal.Client(t)).Remove(machineID, identifier, types.RuleTypeBinary); err != nil {
		t.Fatal(err)
	}
}

// B3: SyncSeq identifies syncs and must keep increasing when the sync state row is lost (TTL) and recreated,
// otherwise a removal first sent by an earlier sync could look as if a later one confirmed it.
func TestB3_SyncSeqStaysMonotonicWhenTheSyncStateIsRecreated(t *testing.T) {
	transport := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	host := newHost(t, machineA, transport)
	host.MustSync(t)
	host.MustSync(t)
	before := numberAttr(t, getItem(t, "Machine#"+machineA, "SyncState"), "SyncSeq")
	if before == 0 {
		t.Fatal("precondition: the sync state has a SyncSeq")
	}
	deleteItem(t, "Machine#"+machineA, "SyncState")
	host.MustSync(t)
	if after := numberAttr(t, getItem(t, "Machine#"+machineA, "SyncState"), "SyncSeq"); after <= before {
		t.Fatalf("SyncSeq went from %d to %d when the sync state row was recreated", before, after)
	}
}

// L1: tracking items are rewritten (their expiry extended) once they were last refreshed more than 30 days ago, and
// not on every sync.
func TestL1_TrackingItemsAreRefreshedMonthlyNotEverySync(t *testing.T) {
	transport := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	for _, sha := range []string{sha2, sha3} {
		addMachineRule(t, machineA, sha, types.RuleTypeBinary, types.RulePolicyAllowlist, time.Now().Add(365*24*time.Hour))
	}
	host := newHost(t, machineA, transport)
	host.MustSync(t)
	host.MustSync(t)
	if len(trackingItems(t, machineA)) != 2 {
		t.Fatalf("precondition: 2 tracking items, have %d", len(trackingItems(t, machineA)))
	}

	// As if the items were written, and last refreshed, 31 days ago.
	old := time.Now().Add(-31 * 24 * time.Hour)
	updateItem(t, "Machine#"+machineA, "SyncState", map[string]any{"MachineRulesTrackingRefreshedAt": clock.RFC3339(old)})
	for sk := range trackingItems(t, machineA) {
		updateItem(t, "MachineRuleDelivery#"+machineA, sk, map[string]any{"ExpiresAfter": old.Add(machinerules.DeliveryExpiry).Unix()})
	}

	host.MustSync(t)
	minExpiry := time.Now().Add(machinerules.DeliveryExpiry - time.Hour).Unix()
	for sk, item := range trackingItems(t, machineA) {
		if got := numberAttr(t, item, "ExpiresAfter"); got < minExpiry {
			t.Errorf("tracking item %s was not refreshed: expires %s", sk, time.Unix(got, 0))
		}
	}

	ddblocal.ResetCounts()
	host.MustSync(t)
	if n := ddblocal.Counts()["BatchWriteItem"]; n != 0 {
		t.Errorf("tracking items were rewritten again on the next sync (%d BatchWriteItem calls)", n)
	}
}

// L2/H1b: tracking items expire 120 days after they were last written, and are only rewritten every 30 days. A Mac
// that was offline long enough for its sync state to expire (90 days) but not its items comes back with a fresh
// refresh date, so items written before the gap are not rewritten in time and expire while the Mac still holds their
// rules. A rule removed after that must still leave the Mac.
func TestL2_TrackingItemsExpiringWhileTheMacHoldsTheRuleNeverLoseARemoval(t *testing.T) {
	transport := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	addMachineRule(t, machineA, sha2, types.RuleTypeBinary, types.RulePolicyAllowlist, time.Now().Add(400*24*time.Hour))
	host := newHost(t, machineA, transport)
	host.MustSync(t)
	host.MustSync(t)

	// Offline for 100 days: the sync state (90 days) expires, the tracking item (120 days) does not.
	ageRows(t, 100*24*time.Hour)
	expireTTL(t)
	if len(trackingItems(t, machineA)) != 1 {
		t.Fatal("precondition: the tracking item outlives the sync state")
	}
	host.MustSync(t) // clean sync: the Mac gets its machine rule again
	if host.Policy("BINARY", sha2) != "ALLOWLIST" {
		t.Fatal("precondition: the Mac holds the machine rule after it came back")
	}

	// Syncing every 5 days for 25 more days, with DynamoDB's TTL deleting expired items.
	for i := 0; i < 5; i++ {
		ageRows(t, 5*24*time.Hour)
		expireTTL(t)
		host.MustSync(t)
	}
	// No periodic clean sync may hide the outcome.
	updateItem(t, "Machine#"+machineA, "SyncState", map[string]any{"LastCleanSync": clock.RFC3339(time.Now())})

	removeMachineRule(t, machineA, sha2)
	for i := 0; i < 3; i++ {
		if res := host.MustSync(t); res.SyncType != "NORMAL" {
			t.Fatalf("precondition: incremental syncs, got %s", res.SyncType)
		}
	}
	if got := host.Policy("BINARY", sha2); got != "" {
		t.Fatalf("a machine rule removed after its tracking item expired never left the Mac: %q", got)
	}
}

// L2: deleting the sync state row (TTL) never loses a removal.
func TestL2_LostSyncStateNeverLosesARemoval(t *testing.T) {
	transport := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	addMachineRule(t, machineA, sha2, types.RuleTypeBinary, types.RulePolicyAllowlist, time.Now().Add(24*time.Hour))
	host := newHost(t, machineA, transport)
	host.MustSync(t)
	host.MustSync(t)
	deleteItem(t, "Machine#"+machineA, "SyncState")
	removeMachineRule(t, machineA, sha2)
	host.MustSync(t)
	host.MustSync(t)
	if got := host.Policy("BINARY", sha2); got != "" {
		t.Fatalf("removal lost after the sync state row was deleted: %q", got)
	}
}

// H1b: Santa skips cleanup when a clean sync brings no rules (SNTSyncRuleDownload.mm). A Mac whose sync state and
// tracking expired comes back to a clean sync; if no rule applies to it (no global rules, its machine rule expired),
// the clean sync must still remove the machine rule it holds.
func TestH1b_CleanSyncWithNoRulesStillClearsTheMac(t *testing.T) {
	transport := freshServer(t)
	addMachineRule(t, machineA, sha2, types.RuleTypeBinary, types.RulePolicyAllowlist, time.Now().Add(24*time.Hour))
	host := newHost(t, machineA, transport)
	host.MustSync(t)
	host.MustSync(t)
	if host.Policy("BINARY", sha2) != "ALLOWLIST" {
		t.Fatal("precondition: the Mac holds the machine rule")
	}

	ageRows(t, 130*24*time.Hour) // offline: the rule, the sync state and the tracking all expire
	expireTTL(t)
	for i := 0; i < 3; i++ {
		host.MustSync(t)
	}
	if got := host.Policy("BINARY", sha2); got != "" {
		t.Fatalf("an expired machine rule stays on the Mac forever when the clean sync has no rules: %q", got)
	}
}

// H1b: a preflight after a sync that did not complete marks the tracking summary dirty, and the next rule download
// reads the tracking items instead of trusting the summary.
func TestH1b_PreflightAfterAnIncompleteSyncMarksTrackingDirty(t *testing.T) {
	base := freshServer(t)
	addMachineRule(t, machineA, sha2, types.RuleTypeBinary, types.RulePolicyAllowlist, time.Now().Add(24*time.Hour))
	fail := false
	dirtyAtRuledownload := ""
	transport := func(req events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, error) {
		if req.Resource == "/ruledownload/{machine_id}" {
			dirtyAtRuledownload = stringAttr(getItem(t, "Machine#"+machineA, "SyncState"), "MachineRulesTracked")
		}
		return failingStage(base, "ruledownload", 504, &fail)(req)
	}
	host := newHost(t, machineA, transport)
	host.MustSync(t)
	host.MustSync(t)

	fail = true
	if _, err := host.Sync(t); err == nil {
		t.Fatal("precondition: the sync fails at rule download")
	}
	fail = false

	var deliveryQueries int
	ddblocal.AddHook(t, ddblocal.Hook{Before: func(c *ddblocal.Call) *ddblocal.Fault {
		if c.Op == "Query" && strings.Contains(c.InputJSON(), "MachineRuleDelivery#") {
			deliveryQueries++
		}
		return nil
	}})
	host.MustSync(t)
	if dirtyAtRuledownload != "dirty" {
		t.Errorf("after an incomplete sync, preflight must write the dirty marker; rule download saw %q", dirtyAtRuledownload)
	}
	if deliveryQueries == 0 {
		t.Error("the rule download after an incomplete sync must re-read the tracking items")
	}
}

// TX: two rule downloads of the same Mac overlap (Santa gave up on a slow sync and started another), and the admin
// re-adds a rule in that window. The tracking writes of the older rule download must not overwrite what the newer
// one recorded: a later removal of the rule must still reach the Mac.
//
// Sequence, made deterministic with a pause in the DynamoDB proxy: the rule's removal has been delivered by two
// syncs, so the older rule download (sync 1) decides to forget its tracking item. Its write is held. The admin adds
// the rule again; sync 2 runs completely, sends the rule and records it as held. Then sync 1's write (deleting the
// item) goes through. Today the item is gone while the Mac holds the rule, and the summary says nothing changed, so
// the next removal is never sent. A fix may make a sync's tracking writes conditional on the sync still being the
// machine's current one (SyncSeq), all-or-nothing per sync, or anything else that keeps this guarantee.
func TestTX_OverlappingRuleDownloadsNeverLoseARemoval(t *testing.T) {
	transport := freshServer(t)
	addGlobalRule(t, sha1, types.RuleTypeBinary, types.RulePolicyBlocklist)
	addMachineRule(t, machineA, sha2, types.RuleTypeBinary, types.RulePolicyAllowlist, time.Now().Add(24*time.Hour))
	host := newHost(t, machineA, transport)
	host.MustSync(t)
	host.MustSync(t)
	removeMachineRule(t, machineA, sha2)
	host.MustSync(t) // sends the removal
	host.MustSync(t) // sends it again; two completed syncs have sent it once this one completes
	if host.Policy("BINARY", sha2) != "" {
		t.Fatal("precondition: the removal reached the Mac")
	}

	pause := ddblocal.PauseFirst(t, func(c *ddblocal.Call) bool {
		in := c.InputJSON()
		return c.Op == "BatchWriteItem" && strings.Contains(in, "MachineRuleDelivery#") && strings.Contains(in, "DeleteRequest")
	})
	ghost := host.Clone(transport) // the same Mac, in the sync Santa is about to abandon
	done := make(chan error, 1)
	go func() {
		_, err := ghost.Sync(t)
		done <- err
	}()
	select {
	case <-pause.Reached:
	case err := <-done:
		t.Fatalf("precondition: sync 1 should have reached its tracking write (it decides to forget the delivered removal); it ended with %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("precondition: sync 1 never reached its tracking write")
	}

	addMachineRule(t, machineA, sha2, types.RuleTypeBinary, types.RulePolicyAllowlist, time.Now().Add(24*time.Hour))
	host.MustSync(t) // sync 2 delivers the re-added rule
	if host.Policy("BINARY", sha2) != "ALLOWLIST" {
		t.Fatal("precondition: the re-added rule reached the Mac")
	}
	pause.Release()
	<-done // sync 1 may fail; Santa abandoned it

	host.MustSync(t)
	host.MustSync(t)
	removeMachineRule(t, machineA, sha2)
	for i := 0; i < 3; i++ {
		host.MustSync(t)
	}
	if got := host.Policy("BINARY", sha2); got != "" {
		t.Fatalf("after overlapping rule downloads, a later removal never reached the Mac: %q", got)
	}
}

