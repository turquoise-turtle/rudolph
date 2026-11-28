//go:build integration

package fleet_test

// Rules hash outliers after a global rule change. Every global rule add, update, replace and remove writes a row to
// the rules feed (the store table's `RulesFeed` partition, sort key `<RFC 3339 stamp>#<rule sort key>`). When a rule
// changes late in the day and most Macs are already shut, the Macs that synced since report a new hash while the
// sleeping majority's last hash is the old one; judged by plain majority, the up-to-date Macs would be listed as
// outliers with the stale hash as ExpectedHash.
//
// So the job reads the newest feed row (one Query of `RulesFeed`, newest first, Limit 1, at most once a run, never a
// Scan). Call its stamp T. Only Macs whose current sync completed more than syncstate.FeedSyncCursorOverlap (10
// minutes) after T take part in the check, both in working out the majority and as candidate outliers: a Mac that
// last completed a sync before that has simply not had the change yet. With no feed row at all (a fresh install, or
// every row expired after 90 days) every Mac in the 24-hour window takes part. Within the Macs that take part the
// rules are as before: groups of at least three, the expected hash is the one more than half report, a Mac already
// listed under Rules (NotApplied) is not listed twice, dirty or empty digests and Macs without a completed sync are
// left out.

import (
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/model/feedrules"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	"github.com/airbnb/rudolph/pkg/model/syncstate"
	rudolphtypes "github.com/airbnb/rudolph/pkg/types"
)

// ruleChange is when the newest global rule change in these tests was written: four hours before the run.
var ruleChange = now.Add(-4 * time.Hour)

// feedRow stores a rules feed row stamped at, as the rule writers do, for the rule on binary n.
func feedRow(t *testing.T, at time.Time, n int) {
	t.Helper()
	putStore(t, map[string]any{"PK": feedrules.Partition, "SK": clock.RFC3339(at) + "#Binary#" + sha(n),
		"DataType": string(feedrules.GetDataType()), "RuleType": 1, "Policy": 1, "Identifier": sha(n),
		"ExpiresAfter": at.AddDate(0, 0, 90).Unix()})
}

// hashMacs builds Macs of one digest that completed a sync at the given times and reported hash.
type hashMacs struct {
	t    *testing.T
	next int
	macs []mac
}

func (h *hashMacs) add(digest, hash string, postflight time.Time) mac {
	h.next++
	m := mac{id: machineID(h.next), hostname: "m" + machineID(h.next)[:8], digest: digest, rulesHash: hash,
		rulesReceived: 3, rulesProcessed: 3, preflightAt: postflight.Add(-time.Minute), postflightAt: postflight}
	h.macs = append(h.macs, m)
	return m
}

func (h *hashMacs) seed() { h.t.Helper(); seed(h.t, h.macs...) }

// outliersOfRun runs the job and returns the Rules rows with Reason Outlier, by machine ID.
func outliersOfRun(t *testing.T) (map[string]map[string]any, *recorder) {
	t.Helper()
	rec := run(t)
	out := map[string]map[string]any{}
	for _, row := range rows(t, mustPointer(t), "Health#Rules") {
		if s(row, "Reason") == "Outlier" {
			out[s(row, "MachineID")] = map[string]any{"Hash": s(row, "Hash"), "ExpectedHash": s(row, "ExpectedHash")}
		}
	}
	return out, rec
}

// feedQueries returns the run's Queries of the rules feed.
func feedQueries(rec *recorder) []*call {
	var out []*call
	for _, c := range rec.calls {
		if c.op == "Query" && strings.Join(c.keys, ",") == "RulesFeed" {
			out = append(out, c)
		}
	}
	return out
}

// The reported case: a rule is removed late in the day, four of seven Macs with the same rules last synced before
// that and still report the old hash, three synced since and report the new one. None of them is an outlier.
func TestFleet_OutliersIgnoreMacsThatHaveNotSyncedSinceTheRuleChange(t *testing.T) {
	reset(t)
	// An older change, then the newest one: removing that rule. Both write feed rows; the newest decides.
	added := clock.FrozenTimeProvider{Current: ruleChange.AddDate(0, 0, -3)}
	if err := globalrules.AddNewGlobalRule(added, ddblocal.Client(t), sha(1), rudolphtypes.RuleTypeBinary, rudolphtypes.RulePolicyAllowlist, "fleet test"); err != nil {
		t.Fatal(err)
	}
	removed := clock.FrozenTimeProvider{Current: ruleChange}
	if err := globalrules.RemoveGlobalRule(removed, ddblocal.Client(t), ddblocal.Client(t), "Binary#"+sha(1), "fleet-test-remove"); err != nil {
		t.Fatal(err)
	}

	h := &hashMacs{t: t}
	for i := 0; i < 4; i++ {
		h.add("d1", "old", ruleChange.Add(-3*time.Hour+time.Duration(i)*time.Minute))
	}
	for i := 0; i < 3; i++ {
		h.add("d1", "new", ruleChange.Add(syncstate.FeedSyncCursorOverlap+time.Minute+time.Duration(i)*20*time.Minute))
	}
	h.seed()

	ddblocal.ResetCounts()
	got, rec := outliersOfRun(t)
	if len(got) != 0 {
		t.Errorf("outliers %v after a rule change that most Macs have not synced since; want none (the Macs with the new rules are not outliers)", sortedKeys(got))
	}
	if n := ddblocal.Counts()["Scan"]; n != 0 {
		t.Errorf("%d Scan calls", n)
	}
	queries := feedQueries(rec)
	if len(queries) != 1 {
		t.Fatalf("%d Queries of the RulesFeed partition, want exactly 1 (the newest feed row, read once a run)", len(queries))
	}
	q := queries[0]
	if q.table != ddblocal.TableName || q.index != "" {
		t.Errorf("the feed was read from table %s index %q, want the store table itself", q.table, q.index)
	}
	if q.limit != 1 || !q.backward {
		t.Errorf("the feed Query has Limit %d, newest first %v; want Limit 1, newest first", q.limit, q.backward)
	}
}

// After the change, a Mac that synced since and reports a hash unlike the other Macs that synced since is the
// outlier, and the hash those report is the expected one, however many Macs still report the old hash.
func TestFleet_OutlierAmongMacsSyncedSinceTheRuleChange(t *testing.T) {
	reset(t)
	feedRow(t, ruleChange.AddDate(0, 0, -10), 1)
	feedRow(t, ruleChange, 2)
	after := ruleChange.Add(syncstate.FeedSyncCursorOverlap + time.Minute)

	h := &hashMacs{t: t}
	for i := 0; i < 3; i++ {
		h.add("d1", "old", ruleChange.Add(-2*time.Hour+time.Duration(i)*time.Minute))
	}
	for i := 0; i < 4; i++ {
		h.add("d1", "new", after.Add(time.Duration(i)*15*time.Minute))
	}
	odd := h.add("d1", "other", after.Add(5*time.Minute))
	// A Mac that applied fewer rules than it received is listed once, as NotApplied, whatever its hash.
	notApplied := h.add("d1", "partial", after.Add(30*time.Minute))
	h.macs[len(h.macs)-1].rulesProcessed = 2
	h.seed()

	got, _ := outliersOfRun(t)
	if len(got) != 1 || got[odd.id] == nil {
		t.Fatalf("outliers %v, want only %s (the one Mac synced since the change whose hash differs)", sortedKeys(got), odd.id)
	}
	if got[odd.id]["Hash"] != "other" || got[odd.id]["ExpectedHash"] != "new" {
		t.Errorf("outlier Hash %v ExpectedHash %v, want other and new (the hash the Macs synced since the change report)", got[odd.id]["Hash"], got[odd.id]["ExpectedHash"])
	}
	listed := 0
	for _, row := range rows(t, mustPointer(t), "Health#Rules") {
		if s(row, "MachineID") == notApplied.id {
			listed++
			if s(row, "Reason") != "NotApplied" {
				t.Errorf("%s listed with Reason %s, want NotApplied", notApplied.id, s(row, "Reason"))
			}
		}
	}
	if listed != 1 {
		t.Errorf("the Mac that applied 2 of 3 rules is listed %d times under Rules, want once", listed)
	}
}

// A Mac whose sync completed after the change but within the feed cursor overlap may or may not have been sent it:
// it is neither counted nor listed. Exactly the overlap after the change is still within it.
func TestFleet_OutliersSkipMacsWithinTheOverlapAfterTheRuleChange(t *testing.T) {
	reset(t)
	feedRow(t, ruleChange, 1)
	after := ruleChange.Add(syncstate.FeedSyncCursorOverlap + time.Minute)

	h := &hashMacs{t: t}
	// Digest d1: three Macs synced since agree; one within the overlap still reports the old hash: not listed.
	for i := 0; i < 3; i++ {
		h.add("d1", "new", after.Add(time.Duration(i)*time.Minute))
	}
	within := h.add("d1", "old", ruleChange.Add(5*time.Minute))
	// Digest d2: two Macs within the overlap (one exactly at it) report the old hash. Were they counted, the five
	// Macs would have no majority; they are not, so of the three that synced since, the odd one out is listed.
	h.add("d2", "new2", after)
	h.add("d2", "new2", after.Add(time.Minute))
	odd := h.add("d2", "other", after.Add(2*time.Minute))
	h.add("d2", "old2", ruleChange.Add(2*time.Minute))
	h.add("d2", "old2", ruleChange.Add(syncstate.FeedSyncCursorOverlap))
	h.seed()

	got, _ := outliersOfRun(t)
	if got[within.id] != nil {
		t.Errorf("%s, whose sync completed 5 minutes after the rule change, is listed as an outlier", within.id)
	}
	if len(got) != 1 || got[odd.id] == nil {
		t.Fatalf("outliers %v, want only %s (Macs within the overlap are not counted towards the majority)", sortedKeys(got), odd.id)
	}
	if got[odd.id]["ExpectedHash"] != "new2" {
		t.Errorf("ExpectedHash %v, want new2", got[odd.id]["ExpectedHash"])
	}
}

// With no feed row (a fresh install, or every row expired), every Mac that completed a sync in the last day takes
// part: the odd one out among five is listed, even though its sync completed before the others'.
func TestFleet_OutliersWithoutFeedRowsUseTheWholeWindow(t *testing.T) {
	reset(t)
	base := now.Add(-3 * time.Hour)
	h := &hashMacs{t: t}
	odd := h.add("d1", "h2", base.Add(-time.Hour))
	for i := 0; i < 4; i++ {
		h.add("d1", "h1", base.Add(time.Duration(i)*time.Minute))
	}
	h.seed()

	ddblocal.ResetCounts()
	got, rec := outliersOfRun(t)
	if len(got) != 1 || got[odd.id] == nil {
		t.Fatalf("outliers %v, want only %s", sortedKeys(got), odd.id)
	}
	if got[odd.id]["ExpectedHash"] != "h1" {
		t.Errorf("ExpectedHash %v, want h1", got[odd.id]["ExpectedHash"])
	}
	if n := ddblocal.Counts()["Scan"]; n != 0 {
		t.Errorf("%d Scan calls", n)
	}
	for _, q := range feedQueries(rec) {
		if q.limit != 1 || !q.backward {
			t.Errorf("the feed Query has Limit %d, newest first %v; want Limit 1, newest first", q.limit, q.backward)
		}
	}
	if n := len(feedQueries(rec)); n > 1 {
		t.Errorf("%d Queries of the RulesFeed partition, want at most 1", n)
	}
}

// Leaving out the Macs that have not synced since the change can leave fewer than three to compare: nothing is
// listed then.
func TestFleet_OutliersNeedThreeMacsSyncedSinceTheRuleChange(t *testing.T) {
	reset(t)
	feedRow(t, ruleChange, 1)
	after := ruleChange.Add(syncstate.FeedSyncCursorOverlap + time.Minute)
	h := &hashMacs{t: t}
	for i := 0; i < 4; i++ {
		h.add("d1", "old", ruleChange.Add(-2*time.Hour+time.Duration(i)*time.Minute))
	}
	h.add("d1", "new", after)
	h.add("d1", "other", after.Add(time.Hour))
	h.seed()

	if got, _ := outliersOfRun(t); len(got) != 0 {
		t.Errorf("outliers %v with only two Macs synced since the rule change; want none", sortedKeys(got))
	}
}
