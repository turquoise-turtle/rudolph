//go:build integration

package fleet_test

// Automatic clean syncs for rules hash outliers. Santa's execution rules hash (2026.8 to 2026.10) depends on the order
// the Mac wrote its rules, so Macs holding the same rules can report different hashes; a clean sync rewrites the rules
// in Rudolph's order and brings such a Mac back to the majority hash. So for each outlier settleOutliers finds, the job
// decides:
//
//   - Request: no automatic clean sync yet for its set's current rule change. The job requests one and does not list
//     the Mac under Rules.
//   - Pending: it has had one for this rule change, or a clean sync is requested by anyone (CleanSync true), and no
//     clean sync completed since. Neither listed nor requested again.
//   - Still differs: it has had one for this rule change, and a clean sync completed after the request (the sync
//     state's LastCompletedCleanSync is after AutoCleanSyncAt), yet it is still an outlier. Listed under Rules as before,
//     with AfterCleanSync true on the row.
//
// The rule change is the set's change time (ruleChanges.of) as RFC 3339 UTC, or "none" when no change is on record. A
// request is one UpdateItem of the Mac's own `Machine#<id>` / `Config` item, created if absent, setting CleanSync true,
// a new CleanSyncRequest, AutoCleanSyncFor (the change), AutoCleanSyncAt (the run's time, RFC 3339 UTC) and
// AutoCleanSyncRequest (equal to the CleanSyncRequest written), and DataType on an item it creates; nothing else.
// At most fleet.MaxCleanSyncRequestsPerRun (100) requests a run: the outliers beyond it are listed as before, without
// AfterCleanSync. The pointer records CleanSyncsRequested and CleanSyncsPending. A run that requested any records one
// audit entry: Action fleet.clean_sync, Target fleet, Actor job:fleet, the machine IDs in the Summary.
//
// FleetHealth / Config's AutoCleanSync "off" turns it off: every outlier is listed and no machine config is written.

import (
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/airbnb/rudolph/internal/fleet"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/pkg/model/audit"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
	"github.com/airbnb/rudolph/pkg/model/syncstate"
	rudolphtypes "github.com/airbnb/rudolph/pkg/types"
)

// fleetWritable are the attributes a clean sync request may write on a machine config item; the fleet role's
// UpdateItem is limited to them (deployments/fleet_test.go).
var fleetWritable = []string{"PK", "SK", "DataType", "CleanSync", "CleanSyncRequest", "AutoCleanSyncFor", "AutoCleanSyncAt", "AutoCleanSyncRequest"}

// autoCleanSyncOff stores AutoCleanSync "off" on FleetHealth / Config, keeping the item's other attributes.
func autoCleanSyncOff(t *testing.T) {
	t.Helper()
	_, err := ddblocal.Raw(t).UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(ddblocal.TableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: "FleetHealth"}, "SK": &types.AttributeValueMemberS{Value: "Config"},
		},
		UpdateExpression:          aws.String("SET AutoCleanSync = :off"),
		ExpressionAttributeValues: map[string]types.AttributeValue{":off": &types.AttributeValueMemberS{Value: "off"}},
	})
	if err != nil {
		t.Fatal(err)
	}
}

// configOf reads a Mac's machine config item as stored; nil when there is none.
func configOf(t *testing.T, id string) map[string]types.AttributeValue {
	t.Helper()
	out, err := ddblocal.Raw(t).GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(ddblocal.TableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: "Machine#" + id}, "SK": &types.AttributeValueMemberS{Value: "Config"},
		},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.Item
}

// rulesRows runs nothing: it reads the current snapshot's Rules rows by machine ID.
func rulesRows(t *testing.T) map[string]map[string]types.AttributeValue {
	t.Helper()
	return byMachine(rows(t, mustPointer(t), "Health#Rules"))
}

func pointerCount(t *testing.T, name string) (int, bool) {
	t.Helper()
	return n(mustPointer(t).raw, name)
}

func wantPointerCounts(t *testing.T, requested, pending int) {
	t.Helper()
	for name, want := range map[string]int{"CleanSyncsRequested": requested, "CleanSyncsPending": pending} {
		got, ok := pointerCount(t, name)
		if !ok {
			t.Errorf("the pointer has no number %s", name)
		} else if got != want {
			t.Errorf("pointer %s %d, want %d", name, got, want)
		}
	}
}

// cleanSyncEntries lists the fleet job's clean sync audit entries.
func cleanSyncEntries(t *testing.T) []audit.Entry {
	t.Helper()
	all, err := audit.ListContext(ctx, ddblocal.Raw(t), ddblocal.TableName, now.Add(-72*time.Hour), 1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []audit.Entry
	for _, e := range all {
		if e.Action == "fleet.clean_sync" {
			out = append(out, e)
		}
	}
	return out
}

func updateCalls(rec *recorder) []*call {
	var out []*call
	for _, c := range rec.calls {
		if c.op == "UpdateItem" {
			out = append(out, c)
		}
	}
	return out
}

// wantRequested checks that the Mac's config holds an automatic clean sync request for change made at at, and returns
// its CleanSyncRequest.
func wantRequested(t *testing.T, id, change string, at time.Time) string {
	t.Helper()
	item := configOf(t, id)
	if item == nil {
		t.Fatalf("%s has no machine config item: no clean sync was requested", id)
	}
	if v, ok := b(item, "CleanSync"); !ok || !v {
		t.Errorf("%s CleanSync %v (present %v), want true", id, v, ok)
	}
	request := s(item, "CleanSyncRequest")
	if len(request) != 16 || strings.Trim(request, "0123456789abcdef") != "" {
		t.Errorf("%s CleanSyncRequest %q, want a new request identifier (16 hex digits) as `rudolph config` writes", id, request)
	}
	if got := s(item, "AutoCleanSyncRequest"); got != request {
		t.Errorf("%s AutoCleanSyncRequest %q, want the CleanSyncRequest written with it, %q", id, got, request)
	}
	if got := s(item, "AutoCleanSyncFor"); got != change {
		t.Errorf("%s AutoCleanSyncFor %q, want %q", id, got, change)
	}
	if got := s(item, "AutoCleanSyncAt"); got != rfc(at) {
		t.Errorf("%s AutoCleanSyncAt %q, want the run's time %q", id, got, rfc(at))
	}
	return request
}

// fleetWithOddMac seeds a rules feed row at change and a set of Macs that synced at synced: four agree on h1, odd
// reports h2.
func fleetWithOddMac(t *testing.T, change, synced time.Time) (*hashMacs, mac) {
	t.Helper()
	reset(t)
	feedRow(t, change, 1)
	h := &hashMacs{t: t}
	for i := 0; i < 4; i++ {
		h.add("d1", "h1", synced.Add(time.Duration(i)*time.Minute))
	}
	odd := h.add("d1", "h2", synced.Add(5*time.Minute))
	return h, odd
}

// The whole cycle for one outlier, as it happens in production: requested and not listed; not listed and not
// requested again while the clean sync has not happened; listed with AfterCleanSync once a clean sync completed after
// the request and the hash still differs.
func TestFleet_AutoCleanSyncCycle(t *testing.T) {
	change := now.Add(-20 * time.Hour)
	h, odd := fleetWithOddMac(t, change, now.Add(-12*time.Hour))
	h.seed()

	// Run 1: the outlier gets a clean sync requested and is not listed.
	first := now.Add(-10 * time.Hour)
	rec := runAt(t, first)
	if got := rulesRows(t); got[odd.id] != nil {
		t.Errorf("run 1 lists %s under Rules; a Mac whose clean sync was just requested is not listed", odd.id)
	}
	request := wantRequested(t, odd.id, rfc(change), first)
	if p := mustPointer(t); p.Health["Rules"] != 0 || p.Attention != 0 {
		t.Errorf("run 1 pointer Health Rules %d Attention %d, want 0 and 0", p.Health["Rules"], p.Attention)
	}
	wantPointerCounts(t, 1, 0)
	if calls := updateCalls(rec); len(calls) != 1 || calls[0].table != ddblocal.TableName || calls[0].keys[0] != "Machine#"+odd.id || calls[0].sk != "Config" {
		t.Errorf("run 1 UpdateItem calls %+v, want one, on the store table's Machine#%s / Config", calls, odd.id)
	}
	for _, other := range h.macs[:4] {
		if item := configOf(t, other.id); item != nil {
			t.Errorf("a clean sync was requested for %s, which reports the majority hash: %v", other.id, item)
		}
	}

	// Run 2: no clean sync has happened since; nothing is listed or requested.
	second := now.Add(-5 * time.Hour)
	rec = runAt(t, second)
	if got := rulesRows(t); got[odd.id] != nil {
		t.Errorf("run 2 lists %s while its clean sync is pending", odd.id)
	}
	if got := s(configOf(t, odd.id), "CleanSyncRequest"); got != request {
		t.Errorf("run 2 changed CleanSyncRequest from %q to %q: a pending request is not made again", request, got)
	}
	if got := s(configOf(t, odd.id), "AutoCleanSyncAt"); got != rfc(first) {
		t.Errorf("run 2 changed AutoCleanSyncAt to %q, want %q kept", got, rfc(first))
	}
	if calls := updateCalls(rec); len(calls) != 0 {
		t.Errorf("run 2 made %d UpdateItem calls, want none", len(calls))
	}
	wantPointerCounts(t, 0, 1)

	// The Mac performs the clean sync two hours ago (preflight clears the flag) and still reports h2.
	cleanAt := now.Add(-2 * time.Hour)
	odd.preflightAt, odd.postflightAt, odd.lastCleanSync = cleanAt, cleanAt.Add(time.Minute), cleanAt
	odd.seed(t)
	if err := machineconfiguration.ClearMachineCleanSync(ddblocal.Client(t), odd.id, request); err != nil {
		t.Fatal(err)
	}

	// Run 3: listed, marked AfterCleanSync, and no new request.
	rec = runAt(t, now)
	row := rulesRows(t)[odd.id]
	if row == nil {
		t.Fatalf("run 3 does not list %s, whose hash still differs after a clean sync", odd.id)
	}
	if v, ok := b(row, "AfterCleanSync"); !ok || !v {
		t.Errorf("run 3 row AfterCleanSync %v (present %v), want true", v, ok)
	}
	if s(row, "Reason") != "Outlier" || s(row, "Hash") != "h2" || s(row, "ExpectedHash") != "h1" {
		t.Errorf("run 3 row Reason %q Hash %q ExpectedHash %q, want Outlier, h2, h1", s(row, "Reason"), s(row, "Hash"), s(row, "ExpectedHash"))
	}
	if v, _ := b(configOf(t, odd.id), "CleanSync"); v {
		t.Error("run 3 requested another clean sync for the same rule change")
	}
	if calls := updateCalls(rec); len(calls) != 0 {
		t.Errorf("run 3 made %d UpdateItem calls, want none", len(calls))
	}
	if p := mustPointer(t); p.Health["Rules"] != 1 {
		t.Errorf("run 3 pointer Health Rules %d, want 1", p.Health["Rules"])
	}
	wantPointerCounts(t, 0, 0)
}

// A clean sync that completed before the request does not count: the Mac is still pending.
func TestFleet_AutoCleanSyncPendingIgnoresAnEarlierCleanSync(t *testing.T) {
	change := now.Add(-20 * time.Hour)
	h, odd := fleetWithOddMac(t, change, now.Add(-3*time.Hour))
	h.macs[len(h.macs)-1].lastCleanSync = now.Add(-3*time.Hour - 30*time.Minute)
	h.macs[len(h.macs)-1].config = map[string]any{
		"AutoCleanSyncFor": rfc(change), "AutoCleanSyncAt": rfc(now.Add(-time.Hour)), "AutoCleanSyncRequest": "0123456789abcdef",
	}
	h.seed()
	rec := run(t)
	if rulesRows(t)[odd.id] != nil {
		t.Errorf("%s is listed, but no clean sync completed after its request", odd.id)
	}
	if calls := updateCalls(rec); len(calls) != 0 {
		t.Errorf("%d UpdateItem calls, want none: the Mac already had its clean sync for this rule change", len(calls))
	}
	wantPointerCounts(t, 0, 1)
}

// A later rule change is a new key: a Mac that had its clean sync for the earlier one gets another.
func TestFleet_AutoCleanSyncAgainAfterANewRuleChange(t *testing.T) {
	earlier := now.Add(-30 * time.Hour)
	change := now.Add(-20 * time.Hour)
	h, odd := fleetWithOddMac(t, change, now.Add(-12*time.Hour))
	h.macs[len(h.macs)-1].lastCleanSync = now.Add(-12*time.Hour - time.Minute)
	h.macs[len(h.macs)-1].config = map[string]any{
		"CleanSync": false, "AutoCleanSyncFor": rfc(earlier), "AutoCleanSyncAt": rfc(earlier.Add(time.Hour)), "AutoCleanSyncRequest": "0123456789abcdef",
	}
	h.seed()

	run(t)
	if rulesRows(t)[odd.id] != nil {
		t.Errorf("%s is listed; its automatic clean sync was for an earlier rule change, so a new one is requested", odd.id)
	}
	if request := wantRequested(t, odd.id, rfc(change), now); request == "0123456789abcdef" {
		t.Error("the new request kept the earlier CleanSyncRequest")
	}
	wantPointerCounts(t, 1, 0)
}

// The same Mac, its automatic clean sync for the current rule change done, is listed instead.
func TestFleet_AutoCleanSyncStillDiffersForTheSameRuleChange(t *testing.T) {
	change := now.Add(-20 * time.Hour)
	h, odd := fleetWithOddMac(t, change, now.Add(-12*time.Hour))
	h.macs[len(h.macs)-1].lastCleanSync = now.Add(-12*time.Hour - time.Minute)
	h.macs[len(h.macs)-1].config = map[string]any{
		"CleanSync": false, "AutoCleanSyncFor": rfc(change), "AutoCleanSyncAt": rfc(change.Add(time.Hour)), "AutoCleanSyncRequest": "0123456789abcdef",
	}
	h.seed()

	rec := run(t)
	row := rulesRows(t)[odd.id]
	if row == nil {
		t.Fatalf("%s is not listed, though a clean sync completed after its request for this rule change", odd.id)
	}
	if v, _ := b(row, "AfterCleanSync"); !v {
		t.Error("its row has no AfterCleanSync true")
	}
	if calls := updateCalls(rec); len(calls) != 0 {
		t.Errorf("%d UpdateItem calls, want none", len(calls))
	}
}

// The key is the set's change time: "none" without a change on record, a group's RulesChangedAt when it is the
// newest change for the set.
func TestFleet_AutoCleanSyncKeyIsTheSetsRuleChange(t *testing.T) {
	t.Run("no change on record", func(t *testing.T) {
		reset(t)
		h := &hashMacs{t: t}
		for i := 0; i < 3; i++ {
			h.add("d1", "h1", now.Add(-2*time.Hour+time.Duration(i)*time.Minute))
		}
		odd := h.add("d1", "h2", now.Add(-time.Hour))
		h.seed()
		run(t)
		wantRequested(t, odd.id, "none", now)
	})
	t.Run("a group rule change newer than the feed", func(t *testing.T) {
		reset(t)
		groupChanged := now.Add(-6 * time.Hour)
		stampedRegistry(t, stampedGroup{name: "G", changed: groupChanged})
		feedRow(t, now.Add(-30*time.Hour), 1)
		h := &hashMacs{t: t}
		after := groupChanged.Add(syncstate.FeedSyncCursorOverlap + time.Minute)
		for i := 0; i < 3; i++ {
			h.inGroup("G", "d1", "h1", after.Add(time.Duration(i)*time.Minute))
		}
		odd := h.inGroup("G", "d1", "h2", after.Add(10*time.Minute))
		h.seed()
		run(t)
		wantRequested(t, odd.id, rfc(groupChanged), now)
	})
}

// A clean sync already requested by someone else (`rudolph config update --clean-sync`) is left alone: the Mac is not
// requested again, not listed, and counts as pending.
func TestFleet_AutoCleanSyncLeavesAManualRequestAlone(t *testing.T) {
	h, odd := fleetWithOddMac(t, now.Add(-20*time.Hour), now.Add(-12*time.Hour))
	h.macs[len(h.macs)-1].config = map[string]any{"CleanSync": true, "CleanSyncRequest": "feedfacefeedface"}
	h.seed()
	before := configOf(t, odd.id)

	rec := run(t)
	if rulesRows(t)[odd.id] != nil {
		t.Errorf("%s is listed while a clean sync is requested for it", odd.id)
	}
	if after := configOf(t, odd.id); !reflect.DeepEqual(after, before) {
		t.Errorf("the machine config changed:\n  before %v\n  after  %v", before, after)
	}
	if calls := updateCalls(rec); len(calls) != 0 {
		t.Errorf("%d UpdateItem calls, want none", len(calls))
	}
	if n := len(cleanSyncEntries(t)); n != 0 {
		t.Errorf("%d fleet.clean_sync audit entries for a run that requested nothing", n)
	}
	wantPointerCounts(t, 0, 1)
}

// At most fleet.MaxCleanSyncRequestsPerRun requests a run; the outliers beyond it are listed as before, and the next
// run requests them.
func TestFleet_AutoCleanSyncCap(t *testing.T) {
	if fleet.MaxCleanSyncRequestsPerRun != 100 {
		t.Errorf("MaxCleanSyncRequestsPerRun %d, want 100", fleet.MaxCleanSyncRequestsPerRun)
	}
	limit := fleet.MaxCleanSyncRequestsPerRun
	reset(t)
	h := &hashMacs{t: t}
	base := now.Add(-3 * time.Hour)
	extra := 3
	for i := 0; i < limit+extra+1; i++ {
		h.add("d1", "h1", base)
	}
	var odd []string
	for i := 0; i < limit+extra; i++ {
		odd = append(odd, h.add("d1", "h2", base.Add(time.Minute)).id)
	}
	h.seed()

	rec := run(t)
	listed := rulesRows(t)
	var requested, notRequested []string
	for _, id := range odd {
		if configOf(t, id) != nil {
			requested = append(requested, id)
			if listed[id] != nil {
				t.Errorf("%s has a clean sync requested and is listed", id)
			}
			continue
		}
		notRequested = append(notRequested, id)
		row := listed[id]
		if row == nil {
			t.Errorf("%s, beyond the cap, is neither requested nor listed", id)
			continue
		}
		if v, _ := b(row, "AfterCleanSync"); v {
			t.Errorf("%s, beyond the cap, is listed with AfterCleanSync", id)
		}
	}
	if len(requested) != limit || len(notRequested) != extra {
		t.Errorf("%d requested and %d not, want %d and %d", len(requested), len(notRequested), limit, extra)
	}
	if n := len(updateCalls(rec)); n != limit {
		t.Errorf("%d UpdateItem calls, want %d", n, limit)
	}
	if p := mustPointer(t); p.Health["Rules"] != extra {
		t.Errorf("pointer Health Rules %d, want %d", p.Health["Rules"], extra)
	}
	wantPointerCounts(t, limit, 0)
	entries := cleanSyncEntries(t)
	if len(entries) != 1 {
		t.Fatalf("%d fleet.clean_sync audit entries, want 1 for the run", len(entries))
	}
	for _, id := range requested {
		if !strings.Contains(entries[0].Summary, id) {
			t.Errorf("the audit entry does not name %s", id)
			break
		}
	}
	for _, id := range notRequested {
		if strings.Contains(entries[0].Summary, id) {
			t.Errorf("the audit entry names %s, which was not requested", id)
		}
	}

	// The next run requests the rest; the first hundred are pending.
	runAt(t, now.Add(time.Hour))
	for _, id := range notRequested {
		if configOf(t, id) == nil {
			t.Errorf("the next run did not request a clean sync for %s", id)
		}
	}
	wantPointerCounts(t, extra, limit)
	if p := mustPointer(t); p.Health["Rules"] != 0 {
		t.Errorf("next run pointer Health Rules %d, want 0", p.Health["Rules"])
	}
}

// Off: every outlier is listed as before, without AfterCleanSync, and nothing is written to machine configs.
func TestFleet_AutoCleanSyncOff(t *testing.T) {
	h, odd := fleetWithOddMac(t, now.Add(-20*time.Hour), now.Add(-12*time.Hour))
	second := h.add("d1", "h3", now.Add(-11*time.Hour))
	h.macs[len(h.macs)-1].disableUpload = boolPtr(true)
	h.seed()
	autoCleanSyncOff(t)
	before := configOf(t, second.id)

	rec := run(t)
	listed := rulesRows(t)
	for _, id := range []string{odd.id, second.id} {
		row := listed[id]
		if row == nil {
			t.Errorf("%s is not listed with automatic clean syncs off", id)
			continue
		}
		if _, ok := row["AfterCleanSync"]; ok {
			t.Errorf("%s row has AfterCleanSync with automatic clean syncs off", id)
		}
	}
	if configOf(t, odd.id) != nil {
		t.Errorf("a machine config was created for %s with automatic clean syncs off", odd.id)
	}
	if after := configOf(t, second.id); !reflect.DeepEqual(after, before) {
		t.Errorf("the machine config of %s changed with automatic clean syncs off", second.id)
	}
	if calls := updateCalls(rec); len(calls) != 0 {
		t.Errorf("%d UpdateItem calls with automatic clean syncs off", len(calls))
	}
	if n := len(cleanSyncEntries(t)); n != 0 {
		t.Errorf("%d fleet.clean_sync audit entries with automatic clean syncs off", n)
	}
	if v, ok := pointerCount(t, "CleanSyncsRequested"); ok && v != 0 {
		t.Errorf("pointer CleanSyncsRequested %d with automatic clean syncs off", v)
	}
}

// One audit entry for a run that requested clean syncs, naming every Mac; none for a run that requested nothing.
func TestFleet_AutoCleanSyncAudit(t *testing.T) {
	h, odd := fleetWithOddMac(t, now.Add(-20*time.Hour), now.Add(-12*time.Hour))
	for i := 0; i < 2; i++ {
		h.add("d1", "h1", now.Add(-12*time.Hour+time.Duration(10+i)*time.Minute))
	}
	second := h.add("d1", "h3", now.Add(-11*time.Hour))
	h.seed()

	rec := run(t)
	entries := cleanSyncEntries(t)
	if len(entries) != 1 {
		t.Fatalf("%d fleet.clean_sync audit entries, want 1", len(entries))
	}
	e := entries[0]
	if e.Actor != "job:fleet" || e.Target != "fleet" {
		t.Errorf("audit entry Actor %q Target %q, want job:fleet and fleet", e.Actor, e.Target)
	}
	for _, id := range []string{odd.id, second.id} {
		if !strings.Contains(e.Summary, id) {
			t.Errorf("audit summary %q does not name %s", e.Summary, id)
		}
	}
	var audits []*call
	for _, c := range rec.calls {
		if c.op == "PutItem" && len(c.keys) == 1 && strings.HasPrefix(c.keys[0], "Audit#") {
			audits = append(audits, c)
		}
	}
	if len(audits) != 1 || audits[0].table != ddblocal.TableName {
		t.Errorf("audit PutItem calls %+v, want one, on the store table", audits)
	}

	// The next run requests nothing: no entry.
	runAt(t, now.Add(time.Hour))
	if n := len(cleanSyncEntries(t)); n != 1 {
		t.Errorf("%d fleet.clean_sync audit entries after a run that requested nothing, want still 1", n)
	}
}

// The request touches only the clean sync attributes: a Mac's other settings stay as they are, and an item created
// for a Mac without one holds nothing else, so the Mac keeps inheriting every setting from the global config.
func TestFleet_AutoCleanSyncWritesOnlyItsAttributes(t *testing.T) {
	h, odd := fleetWithOddMac(t, now.Add(-20*time.Hour), now.Add(-12*time.Hour))
	configured := h.add("d1", "h3", now.Add(-11*time.Hour))
	h.macs[len(h.macs)-1].disableUpload = boolPtr(false)
	h.macs[len(h.macs)-1].config = map[string]any{"BatchSize": 100, "EventDetailText": "Ask the service desk", "UploadLogsUrl": "https://logs.example.com/upload"}
	h.macs[len(h.macs)-1].temporaryMode = lockdown
	h.macs[len(h.macs)-1].temporaryExpires = now.Add(48 * time.Hour)
	for i := 0; i < 2; i++ {
		h.add("d1", "h1", now.Add(-12*time.Hour+time.Duration(10+i)*time.Minute))
	}
	h.seed()
	globalConfig(t, rudolphtypes.Monitor, boolPtr(true))
	before := configOf(t, configured.id)

	rec := run(t)
	allowed := map[string]bool{}
	for _, name := range fleetWritable {
		allowed[name] = true
	}

	created := configOf(t, odd.id)
	var names []string
	for name := range created {
		names = append(names, name)
		if !allowed[name] {
			t.Errorf("the item created for %s holds %s; only %v may be written", odd.id, name, fleetWritable)
		}
	}
	sort.Strings(names)
	if len(names) != len(fleetWritable) {
		t.Errorf("the item created for %s holds %v, want exactly %v", odd.id, names, fleetWritable)
	}
	if s(created, "DataType") != "MachineConfig" {
		t.Errorf("the item created for %s has DataType %q, want MachineConfig", odd.id, s(created, "DataType"))
	}
	intended, err := machineconfiguration.GetIntendedConfig(ddblocal.Client(t), odd.id)
	if err != nil {
		t.Fatal(err)
	}
	if intended.DisableUnknownEventUpload == nil || !*intended.DisableUnknownEventUpload {
		t.Errorf("%s no longer inherits DisableUnknownEventUpload from the global config after the request: %+v", odd.id, intended.DisableUnknownEventUpload)
	}

	after := configOf(t, configured.id)
	for name, value := range before {
		if name == "CleanSync" || name == "CleanSyncRequest" {
			continue
		}
		if !reflect.DeepEqual(after[name], value) {
			t.Errorf("%s of %s changed from %v to %v", name, configured.id, value, after[name])
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok && !allowed[name] {
			t.Errorf("the request added %s to the config of %s", name, configured.id)
		}
	}
	wantRequested(t, configured.id, rfc(now.Add(-20*time.Hour)), now)

	for _, c := range rec.calls {
		if c.table != ddblocal.TableName || c.op == "UpdateItem" || c.op == "GetItem" || c.op == "Query" {
			continue
		}
		for _, key := range c.keys {
			if strings.HasPrefix(key, "Machine#") {
				t.Errorf("%s on the store table's %s: a clean sync request is one UpdateItem", c.op, key)
			}
		}
	}
	if n := len(updateCalls(rec)); n != 2 {
		t.Errorf("%d UpdateItem calls, want 2 (one per Mac)", n)
	}
}
