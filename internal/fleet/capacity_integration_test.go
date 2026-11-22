//go:build integration

package fleet_test

// What a run costs, measured against DynamoDB Local's consumed capacity, for the figures in docs/costs.md and
// scripts/cost_grid.py (FLEET_* constants): a Mac in monitor mode with 100 unknown markers of ordinary size, in one or
// two groups, costs about 3.5 read units and 2.5 write units a run; a Mac Rudolph intends to lock costs half a read unit
// and the same writes, because its markers are not read. The budgets below are those figures with a little room; a
// change that goes over them makes the documented cost wrong.

import (
	"fmt"
	"testing"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
)

func TestCOST_FleetRunCapacityPerMac(t *testing.T) {
	const macs = 40
	const markersPerMac = 100
	seedFleet := func(cohortMode int) {
		reset(t)
		registry(t, group{name: "eng"}, group{name: "cohort", mode: cohortMode})
		globalConfig(t, monitor, nil)
		for i := 0; i < 50; i++ {
			globalRule(t, "Binary#"+sha(900000+i))
		}
		for i := 1; i <= macs; i++ {
			assigned := []string{"cohort"}
			if i%2 == 0 {
				assigned = append(assigned, "eng")
			}
			m := mac{id: machineID(i), hostname: fmt.Sprintf("mac-%03d", i), serial: fmt.Sprintf("C02COST%03d", i), assigned: assigned,
				rulesHash: "0123456789abcdef0123456789abcdef", digest: "0123456789abcdef"}
			if cohortMode == lockdown {
				m.reportedMode = lockdown
			}
			seed(t, m)
			ms := make([]marker, markersPerMac)
			for j := range ms {
				ms[j] = marker{sha: sha(i*1000 + j), signingID: fmt.Sprintf("ABCDE12345:com.example.application%03d", j), teamID: "ABCDE12345", lastDay: j % 30}
			}
			markers(t, m.id, ms...)
		}
	}

	seedFleet(0)
	ddblocal.ResetCounts()
	run(t)
	monitored := ddblocal.ConsumedCapacity()
	p := mustPointer(t)
	if p.Macs != macs || p.Readiness["All"].Many != macs {
		t.Fatalf("precondition: %d Macs, %+v; want %d Macs, all Many", p.Macs, p.Readiness["All"], macs)
	}
	t.Logf("monitor mode: %.2f read units and %.2f write units a Mac (%s)", monitored.ReadUnits/macs, monitored.WriteUnits/macs, monitored)
	if monitored.ReadUnits/macs > 4 || monitored.WriteUnits/macs > 2.6 {
		t.Errorf("a monitor-mode Mac costs %.2f read units and %.2f write units a run; budget 4 and 2.6", monitored.ReadUnits/macs, monitored.WriteUnits/macs)
	}

	seedFleet(lockdown)
	ddblocal.ResetCounts()
	run(t)
	locked := ddblocal.ConsumedCapacity()
	if got := mustPointer(t).Readiness["All"].Locked; got != macs {
		t.Fatalf("precondition: %d Macs Locked, want %d", got, macs)
	}
	t.Logf("locked: %.2f read units and %.2f write units a Mac (%s)", locked.ReadUnits/macs, locked.WriteUnits/macs, locked)
	if locked.ReadUnits/macs > 0.7 || locked.WriteUnits/macs > 2.6 {
		t.Errorf("a locked Mac costs %.2f read units and %.2f write units a run; budget 0.7 and 2.6", locked.ReadUnits/macs, locked.WriteUnits/macs)
	}
}
