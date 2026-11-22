//go:build integration

package fleet_test

// How a run reads and writes (fleet health design, "How a run works", "IAM", "Bounds and failure modes"):
//
//   - Macs are listed by paging the store table's DataType_MachineID index with DataType = SensorData: never a Scan.
//   - The job only touches what its IAM role allows (deployments/fleet_test.go holds the role to the same list):
//     store index DataType_MachineID Query (SensorData); store table Query of `Machine#*` and `GlobalRules`; store table
//     GetItem of `GroupRegistry`, `GlobalConfig` and `FleetHealth`; events table Query of `UnknownSeen#*`; events table
//     PutItem and BatchWriteItem of `FleetSnapshot#*` and `FleetPointer`. Nothing else, no other operation.
//   - Rows are written before the pointer, and the pointer only once every row write has finished, so a reader never
//     sees a half-written snapshot. The pointer is a conditional PutItem: an overlapping run that started earlier
//     cannot move it back. A run that fails leaves the previous snapshot in place.
//   - Rows expire 3 days after they are written and carry no TS, Fleet or Decision, so they enter none of the events
//     table's indexes.
//   - A run that finds more than MaxMacs Macs fails before writing anything.

import (
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/fleet"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
)

func smallFleet(t *testing.T) {
	t.Helper()
	registry(t, group{name: "eng"})
	globalConfig(t, monitor, nil)
	globalRule(t, "Binary#"+sha(1))
	seed(t,
		mac{id: machineID(1), hostname: "a", assigned: []string{"eng"}},
		mac{id: machineID(2), hostname: "b", preflightAt: now.AddDate(0, 0, -20)},
		mac{id: machineID(3), hostname: "c", ownMode: lockdown},
		mac{id: machineID(4), hostname: "d", sip: sipFlags(2), santaVersion: "2026.7.0"},
	)
	markers(t, machineID(1), nMarkers(0, 3)...)
}

func TestFleet_NeverScans(t *testing.T) {
	reset(t)
	smallFleet(t)
	ddblocal.ResetCounts()
	rec := run(t)
	if n := ddblocal.Counts()["Scan"]; n != 0 {
		t.Errorf("%d Scan calls", n)
	}
	listed := false
	for _, c := range rec.calls {
		if c.op == "Query" && c.index == "DataType_MachineID" && strings.Join(c.keys, ",") == "SensorData" {
			listed = true
		}
	}
	if !listed {
		t.Error("the Macs were not listed by a Query of DataType_MachineID with DataType = SensorData")
	}
}

func TestFleet_ReadsOnlyItsPartitions(t *testing.T) {
	reset(t)
	smallFleet(t)
	rec := run(t)
	allowed := func(c *call) bool {
		if len(c.keys) == 0 {
			return false
		}
		for _, k := range c.keys {
			ok := false
			switch {
			case c.table == ddblocal.TableName && c.op == "Query" && c.index == "DataType_MachineID":
				ok = k == "SensorData"
			case c.table == ddblocal.TableName && c.op == "Query" && c.index == "":
				ok = strings.HasPrefix(k, "Machine#") || k == "GlobalRules"
			case c.table == ddblocal.TableName && c.op == "GetItem":
				ok = k == "GroupRegistry" || k == "GlobalConfig" || k == "FleetHealth"
			case c.table == ddblocal.EventsTableName && c.op == "Query" && c.index == "":
				ok = strings.HasPrefix(k, "UnknownSeen#")
			case c.table == ddblocal.EventsTableName && (c.op == "PutItem" || c.op == "BatchWriteItem"):
				ok = strings.HasPrefix(k, "FleetSnapshot#") || k == "FleetPointer"
			}
			if !ok {
				return false
			}
		}
		return true
	}
	for _, c := range rec.calls {
		if !allowed(c) {
			t.Errorf("call outside the job's IAM grants: %s on %s index %q keys %v", c.op, c.table, c.index, c.keys)
		}
	}
	if len(rec.calls) == 0 {
		t.Fatal("no calls recorded")
	}
}

// Every row of the snapshot is written, and every row write has finished, before the pointer is written; the pointer
// is the run's last write.
func TestFleet_PointerIsWrittenLast(t *testing.T) {
	reset(t)
	smallFleet(t)
	rec := run(t)
	var pointerCall *call
	for _, c := range rec.calls {
		if c.op == "PutItem" && len(c.keys) == 1 && c.keys[0] == "FleetPointer" {
			if pointerCall != nil {
				t.Error("the pointer was written twice in one run")
			}
			pointerCall = c
		}
	}
	if pointerCall == nil {
		t.Fatal("no PutItem of the pointer")
	}
	writes := 0
	for _, c := range rec.calls {
		if c == pointerCall || (c.op != "BatchWriteItem" && c.op != "PutItem") {
			continue
		}
		writes++
		if c.seq > pointerCall.seq {
			t.Errorf("a %s of %v started after the pointer was written", c.op, c.keys)
		}
		if c.finishedAt > pointerCall.started {
			t.Errorf("a %s of %v was still in flight when the pointer write started", c.op, c.keys)
		}
	}
	if writes == 0 {
		t.Error("no snapshot rows were written")
	}
	p := mustPointer(t)
	// The rows the pointer counts are all there.
	if got := len(rows(t, p, "Ready#All")); got != p.Macs {
		t.Errorf("Ready#All has %d rows, the pointer counts %d Macs", got, p.Macs)
	}
}

// Runs can overlap (EventBridge Scheduler may deliver twice, an operator may invoke the Lambda by hand). A run that
// started earlier must not move the pointer back to its older snapshot; each run leaves its rows readable.
func TestFleet_AnOlderRunDoesNotMoveThePointerBack(t *testing.T) {
	reset(t)
	smallFleet(t)
	runAt(t, now.Add(time.Hour))
	newer := mustPointer(t)
	runAt(t, now) // started earlier, finished later
	if got := mustPointer(t); got.Snapshot != newer.Snapshot {
		t.Errorf("an older run moved the pointer back from %s to %s", newer.Snapshot, got.Snapshot)
	}
	runAt(t, now.Add(2*time.Hour))
	if got := mustPointer(t); got.Snapshot == newer.Snapshot || got.Snapshot < newer.Snapshot {
		t.Errorf("a newer run did not move the pointer forward (%s, before %s)", got.Snapshot, newer.Snapshot)
	}
	if len(rows(t, newer, "Ready#All")) == 0 {
		t.Error("the previous snapshot's rows are gone; readers paging it must still find them until they expire")
	}
}

func TestFleet_AFailedRunLeavesThePreviousSnapshot(t *testing.T) {
	reset(t)
	smallFleet(t)
	run(t)
	before := mustPointer(t)

	seed(t, mac{id: machineID(9), hostname: "new"})
	rec := newRecorder(t)
	rec.failBatch = 2
	if err := fleetRun(options(rec, now.Add(time.Hour))); err == nil {
		t.Fatal("the run succeeded although a row write failed")
	}
	after := mustPointer(t)
	if after.Snapshot != before.Snapshot || after.Macs != before.Macs {
		t.Errorf("a failed run moved the pointer: %s (%d Macs), before %s (%d Macs)", after.Snapshot, after.Macs, before.Snapshot, before.Macs)
	}
	for _, c := range rec.calls {
		if c.op == "PutItem" && len(c.keys) == 1 && c.keys[0] == "FleetPointer" {
			t.Error("the failed run wrote the pointer")
		}
	}
}

func TestFleet_RowsExpireAfterThreeDaysAndEnterNoIndex(t *testing.T) {
	reset(t)
	smallFleet(t)
	run(t)
	seen := 0
	for _, item := range eventsItems(t) {
		pk := s(item, "PK")
		if !strings.HasPrefix(pk, "FleetSnapshot#") && pk != "FleetPointer" {
			continue
		}
		seen++
		for _, key := range []string{"TS", "Fleet", "Decision"} {
			if _, ok := item[key]; ok {
				t.Errorf("%s / %s has %s, an index key of the events table", pk, s(item, "SK"), key)
			}
		}
		if pk == "FleetPointer" {
			continue
		}
		if exp, ok := n(item, "ExpiresAfter"); !ok || int64(exp) != now.Add(72*time.Hour).Unix() {
			t.Errorf("%s / %s ExpiresAfter %d (present %v), want now + 3 days", pk, s(item, "SK"), exp, ok)
		}
	}
	if seen == 0 {
		t.Fatal("the run wrote nothing to the events table")
	}
}

func TestFleet_FailsAboveMaxMacs(t *testing.T) {
	reset(t)
	smallFleet(t)
	rec := newRecorder(t)
	opts := options(rec, now)
	opts.MaxMacs = 3
	if err := fleetRun(opts); err == nil {
		t.Fatal("a run over 4 Macs with MaxMacs 3 succeeded")
	}
	if _, ok := readPointer(t); ok {
		t.Error("a run that failed on MaxMacs wrote the pointer")
	}
	for _, c := range rec.calls {
		if c.op == "BatchWriteItem" || c.op == "PutItem" {
			t.Errorf("a run over MaxMacs wrote %v", c.keys)
		}
	}

	opts = options(newRecorder(t), now)
	opts.MaxMacs = 4
	if err := fleetRun(opts); err != nil {
		t.Errorf("a run over 4 Macs with MaxMacs 4 failed: %v", err)
	}
}

func TestFleet_DefaultOptions(t *testing.T) {
	if fleet.DefaultMinSantaVersion != "2026.9" || fleet.DefaultMaxMacs != 250_000 {
		t.Errorf("defaults MinSantaVersion %q and MaxMacs %d, want 2026.9 and 250,000", fleet.DefaultMinSantaVersion, fleet.DefaultMaxMacs)
	}
}
