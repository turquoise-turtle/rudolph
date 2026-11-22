//go:build integration

package fleet_test

// Lockdown readiness (Fix 3 of the fleet health design; owner decisions of 2026-11-22). In the same walk, for each Mac
// the job reads its unknown markers (`UnknownSeen#<id>`, one Query with Limit 1001) and computes:
//
//   - Unknown30, Unknown7: markers with LastDay >= today - 29 and >= today - 6 (expired markers TTL has not deleted
//     yet are filtered by date).
//   - Pending30, Pending7: those not covered by a global rule (`Binary#<sha>`, `SigningID#<marker SigningID>`,
//     `TeamID#<marker TeamID>`, from the GlobalRules partition). Group and machine rules do not count.
//   - Capped: the Query returned 1,001 rows.
//   - Bucket, first match: 4 Locked (the intended mode is LOCKDOWN or STANDALONE: the markers are not read at all);
//     3 Unmeasured, with Why "stale", "incomplete", "upload-off" (DisableUnknownEventUpload true for the Mac or
//     globally), "no-rules" (the Mac reports zero rules) or "observed" (fewer than 30 days since FirstSeenDay, or
//     none); 2 Many (Pending30 >= 10, or capped); 1 Few (1 to 9); 0 Ready (Pending30 = 0).
//   - ObservedDays: today minus FirstSeenDay (0 without one). There is no backfill and no first-seen floor.
//
// One readiness row per Mac in `FleetSnapshot#<snapshot>#Ready#All` and one in `…#Ready#Group#<group>` for each of
// its effective groups (groups.Effective: registered, reported ones only when the group accepts reported membership;
// `rudolph:ungrouped` when none). SK `<bucket>#<Pending30, 6 digits>#<machine ID>`. Attributes MachineID, Hostname,
// SerialNum, Unknown30, Pending30, Unknown7, Pending7 (all four absent for Locked Macs, whose markers are not read),
// Capped (BOOL), ReportedMode, IntendedMode, ObservedDays, Why (Unmeasured only), ExpiresAfter (now + 3 days).
//
// The pointer's Readiness maps "All", every registered group and "rudolph:ungrouped" to Macs, Ready, Few, Many,
// Unmeasured and Locked (the five add up to Macs); its Capped counts capped Macs.

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	bucketReady      = "0"
	bucketFew        = "1"
	bucketMany       = "2"
	bucketUnmeasured = "3"
	bucketLocked     = "4"
)

func readinessSK(bucket string, pending int, id string) string {
	return fmt.Sprintf("%s#%06d#%s", bucket, pending, id)
}

func TestFleet_ReadinessRowsPerGroupAndForAll(t *testing.T) {
	reset(t)
	registry(t, group{name: "eng"}, group{name: "lab"}, group{name: "empty"})
	ready := mac{id: machineID(1), hostname: "ready", serial: "C02READY", assigned: []string{"eng", "lab"}}
	few3 := mac{id: machineID(2), hostname: "few3", assigned: []string{"eng"}}
	few1 := mac{id: machineID(3), hostname: "few1"}
	many := mac{id: machineID(4), hostname: "many", assigned: []string{"lab"}}
	unmeasured := mac{id: machineID(5), hostname: "unmeasured", firstSeenDay: daysAgo(3)}
	locked := mac{id: machineID(6), hostname: "locked", ownMode: lockdown, reportedMode: lockdown, assigned: []string{"eng"}}
	seed(t, ready, few3, few1, many, unmeasured, locked)
	markers(t, few3.id, nMarkers(100, 3)...)
	markers(t, few1.id, nMarkers(200, 1)...)
	markers(t, many.id, nMarkers(300, 12)...)
	run(t)
	p := mustPointer(t)

	all := rows(t, p, "Ready#All")
	want := []string{
		readinessSK(bucketReady, 0, ready.id),
		readinessSK(bucketFew, 1, few1.id),
		readinessSK(bucketFew, 3, few3.id),
		readinessSK(bucketMany, 12, many.id),
		readinessSK(bucketUnmeasured, 0, unmeasured.id),
		readinessSK(bucketLocked, 0, locked.id),
	}
	if got := strings.Join(sks(all), " "); got != strings.Join(want, " ") {
		t.Errorf("Ready#All sort keys\n got  %s\n want %s", got, strings.Join(want, " "))
	}
	for list, ids := range map[string][]string{
		"Ready#Group#eng":               {ready.id, few3.id, locked.id},
		"Ready#Group#lab":               {ready.id, many.id},
		"Ready#Group#rudolph:ungrouped": {few1.id, unmeasured.id},
		"Ready#Group#empty":             nil,
	} {
		got := byMachine(rows(t, p, list))
		if len(got) != len(ids) {
			t.Errorf("%s has %d rows (%v), want %d", list, len(got), sortedKeys(got), len(ids))
		}
		for _, id := range ids {
			if _, ok := got[id]; !ok {
				t.Errorf("%s lacks %s", list, id)
			}
		}
	}

	r := byMachine(all)[ready.id]
	for attr, want := range map[string]string{"MachineID": ready.id, "Hostname": "ready", "SerialNum": "C02READY", "ReportedMode": "MONITOR", "IntendedMode": "MONITOR"} {
		if got := s(r, attr); got != want {
			t.Errorf("ready row %s %q, want %q", attr, got, want)
		}
	}
	for attr, want := range map[string]int{"Unknown30": 0, "Pending30": 0, "Unknown7": 0, "Pending7": 0, "ObservedDays": 40} {
		if got, ok := n(r, attr); !ok || got != want {
			t.Errorf("ready row %s %d (present %v), want %d", attr, got, ok, want)
		}
	}
	if v, ok := b(r, "Capped"); !ok || v {
		t.Errorf("ready row Capped %v (present %v), want false", v, ok)
	}
	if _, ok := r["Why"]; ok {
		t.Error("a Ready row has Why; only Unmeasured rows say why")
	}
	if exp, _ := n(r, "ExpiresAfter"); int64(exp) != now.Add(72*time.Hour).Unix() {
		t.Errorf("readiness row ExpiresAfter %d, want now + 3 days", exp)
	}
	if got := s(byMachine(all)[unmeasured.id], "Why"); got != "observed" {
		t.Errorf("unmeasured row Why %q, want observed", got)
	}

	wantCounts := map[string]counts{
		"All":               {Macs: 6, Ready: 1, Few: 2, Many: 1, Unmeasured: 1, Locked: 1},
		"eng":               {Macs: 3, Ready: 1, Few: 1, Locked: 1},
		"lab":               {Macs: 2, Ready: 1, Many: 1},
		"empty":             {},
		"rudolph:ungrouped": {Macs: 2, Few: 1, Unmeasured: 1},
	}
	for scope, want := range wantCounts {
		got, ok := p.Readiness[scope]
		if !ok {
			t.Errorf("pointer Readiness has no %q", scope)
			continue
		}
		if got != want {
			t.Errorf("Readiness[%s] = %+v, want %+v", scope, got, want)
		}
		if got.Ready+got.Few+got.Many+got.Unmeasured+got.Locked != got.Macs {
			t.Errorf("Readiness[%s]: the buckets do not add up to Macs: %+v", scope, got)
		}
	}
	if len(p.Readiness) != len(wantCounts) {
		t.Errorf("pointer Readiness scopes %v, want %v", sortedKeys(p.Readiness), sortedKeys(wantCounts))
	}
}

func TestFleet_ReadinessBuckets(t *testing.T) {
	reset(t)
	type bucketCase struct {
		m       mac
		markers int
		bucket  string
		why     string
	}
	cases := []bucketCase{
		{m: mac{hostname: "zero"}, markers: 0, bucket: bucketReady},
		{m: mac{hostname: "one"}, markers: 1, bucket: bucketFew},
		{m: mac{hostname: "nine"}, markers: 9, bucket: bucketFew},
		{m: mac{hostname: "ten"}, markers: 10, bucket: bucketMany},
		{m: mac{hostname: "stale", preflightAt: now.AddDate(0, 0, -15)}, bucket: bucketUnmeasured, why: "stale"},
		{m: mac{hostname: "incomplete", preflightAt: now.Add(-2 * time.Hour), noPostflight: true, completed: []time.Time{now.AddDate(0, 0, -1)}}, bucket: bucketUnmeasured, why: "incomplete"},
		{m: mac{hostname: "upload-off", disableUpload: boolPtr(true)}, bucket: bucketUnmeasured, why: "upload-off"},
		{m: mac{hostname: "upload-on", disableUpload: boolPtr(false)}, bucket: bucketReady},
		{m: mac{hostname: "no-rules", noRules: true}, bucket: bucketUnmeasured, why: "no-rules"},
		{m: mac{hostname: "observed-29", firstSeenDay: daysAgo(29)}, bucket: bucketUnmeasured, why: "observed"},
		{m: mac{hostname: "observed-30", firstSeenDay: daysAgo(30)}, bucket: bucketReady},
		{m: mac{hostname: "never-seen-first", firstSeenDay: "-"}, bucket: bucketUnmeasured, why: "observed"},
		// Unmeasured wins over pending counts.
		{m: mac{hostname: "observed-many", firstSeenDay: daysAgo(2)}, markers: 20, bucket: bucketUnmeasured, why: "observed"},
		// Locked wins over everything.
		{m: mac{hostname: "locked-stale", ownMode: lockdown, preflightAt: now.AddDate(0, 0, -15), noRules: true}, markers: 20, bucket: bucketLocked},
		{m: mac{hostname: "standalone", ownMode: standalone, reportedMode: standalone}, markers: 3, bucket: bucketLocked},
	}
	for i := range cases {
		cases[i].m.id = machineID(i + 1)
		cases[i].m.seed(t)
		if cases[i].markers > 0 {
			markers(t, cases[i].m.id, nMarkers(1000*(i+1), cases[i].markers)...)
		}
	}
	run(t)
	all := byMachine(rows(t, mustPointer(t), "Ready#All"))
	for _, c := range cases {
		row, ok := all[c.m.id]
		if !ok {
			t.Errorf("%s: no Ready#All row", c.m.hostname)
			continue
		}
		if got := strings.SplitN(s(row, "SK"), "#", 2)[0]; got != c.bucket {
			t.Errorf("%s: bucket %s, want %s", c.m.hostname, got, c.bucket)
		}
		if got := s(row, "Why"); got != c.why {
			t.Errorf("%s: Why %q, want %q", c.m.hostname, got, c.why)
		}
	}
	if got, _ := n(all[machineID(11)], "ObservedDays"); got != 30 {
		t.Errorf("ObservedDays %d for a Mac first seen 30 days ago, want 30", got)
	}
	if got, ok := n(all[machineID(12)], "ObservedDays"); ok && got != 0 {
		t.Errorf("ObservedDays %d for a Mac with no FirstSeenDay, want 0", got)
	}
}

// DisableUnknownEventUpload in the global config silences every Mac that does not set it itself.
func TestFleet_ReadinessUploadOffGlobally(t *testing.T) {
	reset(t)
	globalConfig(t, monitor, boolPtr(true))
	inherits := mac{id: machineID(1), hostname: "inherits"}
	overrides := mac{id: machineID(2), hostname: "overrides", disableUpload: boolPtr(false)}
	seed(t, inherits, overrides)
	run(t)
	all := byMachine(rows(t, mustPointer(t), "Ready#All"))
	if got := s(all[inherits.id], "Why"); got != "upload-off" {
		t.Errorf("a Mac inheriting DisableUnknownEventUpload=true: Why %q, want upload-off", got)
	}
	if got := strings.SplitN(s(all[overrides.id], "SK"), "#", 2)[0]; got != bucketReady {
		t.Errorf("a Mac setting DisableUnknownEventUpload=false itself: bucket %s, want Ready", got)
	}
}

// "Ran in the last N days" is LastDay >= today - (N - 1).
func TestFleet_ReadinessWindowsFromLastDay(t *testing.T) {
	reset(t)
	m := mac{id: machineID(1), hostname: "windows"}
	seed(t, m)
	var ms []marker
	for i, age := range []int{0, 6, 7, 29, 30, 40} {
		ms = append(ms, marker{sha: sha(i), lastDay: age})
	}
	markers(t, m.id, ms...)
	run(t)
	row := byMachine(rows(t, mustPointer(t), "Ready#All"))[m.id]
	for attr, want := range map[string]int{"Unknown7": 2, "Pending7": 2, "Unknown30": 4, "Pending30": 4} {
		if got, _ := n(row, attr); got != want {
			t.Errorf("%s %d, want %d (markers 0, 6, 7, 29, 30 and 40 days old)", attr, got, want)
		}
	}
	if got := s(row, "SK"); got != readinessSK(bucketFew, 4, m.id) {
		t.Errorf("SK %q, want %q", got, readinessSK(bucketFew, 4, m.id))
	}
}

// Pending leaves out binaries a global rule now covers: by SHA-256, signing ID or team ID. A group rule does not count.
func TestFleet_PendingSkipsBinariesAGlobalRuleCovers(t *testing.T) {
	reset(t)
	registry(t, group{name: "eng"})
	m := mac{id: machineID(1), hostname: "pending", assigned: []string{"eng"}}
	seed(t, m)
	globalRule(t, "Binary#"+sha(1))
	globalRule(t, "SigningID#ABCDE12345:com.example.two")
	globalRule(t, "TeamID#FGHIJ67890")
	groupRule(t, "eng", "Binary#"+sha(4))
	markers(t, m.id,
		marker{sha: sha(1)},
		marker{sha: sha(2), signingID: "ABCDE12345:com.example.two", teamID: "ABCDE12345"},
		marker{sha: sha(3), signingID: "FGHIJ67890:com.example.three", teamID: "FGHIJ67890"},
		marker{sha: sha(4)},
		marker{sha: sha(5), signingID: "KLMNO12345:com.example.five", teamID: "KLMNO12345", lastDay: 10},
	)
	run(t)
	row := byMachine(rows(t, mustPointer(t), "Ready#All"))[m.id]
	for attr, want := range map[string]int{"Unknown30": 5, "Pending30": 2, "Unknown7": 4, "Pending7": 1} {
		if got, _ := n(row, attr); got != want {
			t.Errorf("%s %d, want %d", attr, got, want)
		}
	}
}

// A Mac's marker partition is read with one Query of Limit 1001 and no further: 1,001 rows mean "1,000+", and the Mac
// is Many whatever its pending count.
func TestFleet_MarkerQueryIsCapped(t *testing.T) {
	reset(t)
	capped := mac{id: machineID(1), hostname: "capped"}
	exact := mac{id: machineID(2), hostname: "exactly-1000"}
	seed(t, capped, exact)
	markers(t, capped.id, nMarkers(0, 5000)...)
	markers(t, exact.id, nMarkers(10000, 1000)...)
	// Covered binaries: a capped Mac is still Many, even when what was read is all covered.
	for i := 0; i < 1001; i++ {
		globalRule(t, "Binary#"+sha(i))
	}
	rec := run(t)

	queries := map[string][]*call{}
	for _, c := range rec.calls {
		if c.op == "Query" && len(c.keys) == 1 && strings.HasPrefix(c.keys[0], "UnknownSeen#") {
			queries[c.keys[0]] = append(queries[c.keys[0]], c)
		}
	}
	for _, m := range []mac{capped, exact} {
		qs := queries["UnknownSeen#"+m.id]
		if len(qs) != 1 {
			t.Errorf("%s: %d marker Queries, want exactly 1", m.hostname, len(qs))
			continue
		}
		if qs[0].limit != 1001 {
			t.Errorf("%s: marker Query Limit %d, want 1001", m.hostname, qs[0].limit)
		}
	}

	p := mustPointer(t)
	all := byMachine(rows(t, p, "Ready#All"))
	if v, _ := b(all[capped.id], "Capped"); !v {
		t.Error("the Mac with 5,000 markers is not Capped")
	}
	if got := strings.SplitN(s(all[capped.id], "SK"), "#", 2)[0]; got != bucketMany {
		t.Errorf("capped Mac bucket %s, want Many (2)", got)
	}
	if v, _ := b(all[exact.id], "Capped"); v {
		t.Error("a Mac with exactly 1,000 markers is Capped; 1,001 rows mean more than 1,000")
	}
	if p.Capped != 1 {
		t.Errorf("pointer Capped %d, want 1", p.Capped)
	}
}

// Macs Rudolph already intends to be in LOCKDOWN or STANDALONE are Locked: their markers are never read.
func TestFleet_LockedMacsAreSkippedWithoutReadingTheirMarkers(t *testing.T) {
	reset(t)
	globalConfig(t, monitor, nil)
	registry(t, group{name: "cohort", mode: lockdown}, group{name: "plain"})
	cases := []mac{
		{id: machineID(1), hostname: "own", ownMode: lockdown, assigned: []string{"plain"}},
		{id: machineID(2), hostname: "temporary", temporaryMode: standalone, temporaryExpires: now.Add(time.Hour), assigned: []string{"plain"}},
		{id: machineID(3), hostname: "group", assigned: []string{"cohort"}},
	}
	monitored := mac{id: machineID(4), hostname: "monitored", assigned: []string{"plain"}}
	seed(t, append(cases, monitored)...)
	for _, m := range append(cases, monitored) {
		markers(t, m.id, nMarkers(0, 3)...)
	}
	rec := run(t)
	read := map[string]bool{}
	for _, c := range rec.calls {
		if c.op == "Query" && len(c.keys) == 1 {
			read[c.keys[0]] = true
		}
	}
	all := byMachine(rows(t, mustPointer(t), "Ready#All"))
	for _, m := range cases {
		if read["UnknownSeen#"+m.id] {
			t.Errorf("%s: the markers of a Mac Rudolph intends to lock were read", m.hostname)
		}
		row := all[m.id]
		if got := s(row, "SK"); got != readinessSK(bucketLocked, 0, m.id) {
			t.Errorf("%s: SK %q, want %q", m.hostname, got, readinessSK(bucketLocked, 0, m.id))
		}
		for _, attr := range []string{"Unknown30", "Pending30", "Unknown7", "Pending7"} {
			if _, ok := row[attr]; ok {
				t.Errorf("%s: a Locked row has %s, but its markers were not read", m.hostname, attr)
			}
		}
	}
	if !read["UnknownSeen#"+monitored.id] {
		t.Error("the monitored Mac's markers were not read")
	}

	// The whole fleet locked by the global config: no marker is read at all.
	reset(t)
	globalConfig(t, lockdown, nil)
	m := mac{id: machineID(5), hostname: "global", reportedMode: lockdown}
	seed(t, m)
	markers(t, m.id, nMarkers(0, 3)...)
	for _, c := range run(t).calls {
		if c.op == "Query" && len(c.keys) == 1 && strings.HasPrefix(c.keys[0], "UnknownSeen#") {
			t.Errorf("a marker Query (%s) with the global config in LOCKDOWN", c.keys[0])
		}
	}
	if got := mustPointer(t).Readiness["All"]; got.Locked != 1 {
		t.Errorf("Readiness[All] %+v, want the Mac Locked", got)
	}
}

// Groups the job does not count for a Mac: assigned but not registered (deleted since), or reported to a group that
// does not accept reported membership. Such a Mac is ungrouped, and no scope is made up for those groups.
func TestFleet_UnregisteredOrNonCountingGroupsAreIgnored(t *testing.T) {
	reset(t)
	registry(t, group{name: "eng", acceptReported: true}, group{name: "staff", acceptReported: false})
	ghost := mac{id: machineID(1), hostname: "ghost", assigned: []string{"deleted-group"}}
	reportedStaff := mac{id: machineID(2), hostname: "reported-staff", reported: []string{"staff"}}
	reportedEng := mac{id: machineID(3), hostname: "reported-eng", reported: []string{"eng"}}
	seed(t, ghost, reportedStaff, reportedEng)
	run(t)
	p := mustPointer(t)
	if _, ok := p.Readiness["deleted-group"]; ok {
		t.Error("the pointer has a Readiness scope for an unregistered group")
	}
	if rs := rows(t, p, "Ready#Group#deleted-group"); len(rs) != 0 {
		t.Errorf("%d rows for an unregistered group", len(rs))
	}
	if got := p.Readiness["staff"].Macs; got != 0 {
		t.Errorf("Readiness[staff].Macs %d, want 0: staff does not accept reported membership", got)
	}
	if got := p.Readiness["eng"].Macs; got != 1 {
		t.Errorf("Readiness[eng].Macs %d, want 1", got)
	}
	ungrouped := byMachine(rows(t, p, "Ready#Group#rudolph:ungrouped"))
	for _, m := range []mac{ghost, reportedStaff} {
		if _, ok := ungrouped[m.id]; !ok {
			t.Errorf("%s is not under rudolph:ungrouped", m.hostname)
		}
	}
	if _, ok := ungrouped[reportedEng.id]; ok {
		t.Error("a Mac in eng is also under rudolph:ungrouped")
	}
}
