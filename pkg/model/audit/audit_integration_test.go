//go:build integration

package audit_test

// The audit API rudolph-web will use (plan F2): Record writes an entry for any actor; List reads them back newest
// first, across days, honouring since and limit.

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/model/audit"
)

func TestMain(m *testing.M) { os.Exit(ddblocal.Main(m)) }

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

var _ clock.TimeProvider = fixedClock{}

func TestRecordAndList(t *testing.T) {
	ddblocal.Reset(t)
	client := ddblocal.Client(t)
	base := time.Now().UTC().Truncate(time.Second)
	for i, at := range []time.Time{base.Add(-49 * time.Hour), base.Add(-25 * time.Hour), base.Add(-time.Minute), base} {
		e := audit.Entry{Actor: "web:admin@example.com (oid 1234)", Action: "rule.add", Target: "global", Summary: "BINARY abc BLOCKLIST"}
		if i == 3 {
			e.Action = "rule.remove"
		}
		if err := audit.Record(fixedClock{at}, client, e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := audit.List(client, base.Add(-48*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("%d entries since 48 h ago, want 3 (one is older): %+v", len(got), got)
	}
	if got[0].Action != "rule.remove" || !got[0].Time.Equal(base) || got[2].Time.After(got[1].Time) {
		t.Errorf("not newest first: %+v", got)
	}
	if got[0].Actor != "web:admin@example.com (oid 1234)" || got[0].Target != "global" || got[0].Summary != "BINARY abc BLOCKLIST" {
		t.Errorf("entry round trip: %+v", got[0])
	}
	two, err := audit.List(client, base.Add(-48*time.Hour), 2)
	if err != nil || len(two) != 2 || two[0].Action != "rule.remove" {
		t.Errorf("limit 2: %+v, %v", two, err)
	}
	// Two entries in the same instant are both kept.
	if err := audit.Record(fixedClock{base}, client, audit.Entry{Actor: "cli:a@b", Action: "group.create", Target: "group:x", Summary: "x"}); err != nil {
		t.Fatal(err)
	}
	if all, _ := audit.List(client, base.Add(-time.Hour), 10); len(all) != 3 {
		t.Errorf("entries in the last hour after a same-instant write: %d, want 3", len(all))
	}
	if err := audit.Record(fixedClock{base}, client, audit.Entry{Action: "rule.add", Target: "global"}); err == nil {
		t.Error("Record accepted an entry without an actor")
	}
}

// A writer whose clock runs ahead past UTC midnight puts its entry in tomorrow's partition; readers with a correct
// clock still list it (review of feat/audit-trail).
func TestList_SeesAnEntryFromAClockAheadPastMidnight(t *testing.T) {
	ddblocal.Reset(t)
	client := ddblocal.Client(t)
	now := time.Now().UTC()
	tomorrow := now.Truncate(24 * time.Hour).Add(24*time.Hour + 30*time.Minute)
	if err := audit.Record(fixedClock{tomorrow}, client, audit.Entry{Actor: "cli:fast@clock", Action: "group.create", Target: "group:x", Summary: "x"}); err != nil {
		t.Fatal(err)
	}
	got, err := audit.List(client, now.Add(-time.Hour), 10)
	if err != nil || len(got) != 1 {
		t.Errorf("List from a correct clock: %+v, %v; want the entry written by a clock ahead past midnight", got, err)
	}
}

// Paging (for rudolph-web's audit page): ListPage returns at most limit entries newest first and an opaque cursor for
// the next page, "" after the last; pages never repeat or skip an entry, across days.
func TestListPage(t *testing.T) {
	ddblocal.Reset(t)
	client := ddblocal.Client(t)
	base := time.Now().UTC().Truncate(time.Second)
	var want []time.Time
	for _, at := range []time.Time{base.Add(-50 * time.Hour), base.Add(-26 * time.Hour), base.Add(-25 * time.Hour), base.Add(-2 * time.Hour), base.Add(-time.Hour)} {
		if err := audit.Record(fixedClock{at}, client, audit.Entry{Actor: "cli:a@b", Action: "group.create", Target: "group:x", Summary: at.String()}); err != nil {
			t.Fatal(err)
		}
		want = append([]time.Time{at}, want...)
	}
	var got []time.Time
	cursor := ""
	for page := 0; page < 10; page++ {
		entries, next, err := audit.ListPage(client, base.Add(-72*time.Hour), 2, cursor)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) > 2 {
			t.Fatalf("page of %d entries, limit 2", len(entries))
		}
		for _, e := range entries {
			got = append(got, e.Time)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(got) != len(want) {
		t.Fatalf("paged through %d entries, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Errorf("entry %d at %v, want %v (newest first, no repeats)", i, got[i], want[i])
		}
	}
	if _, _, err := audit.ListPage(client, base.Add(-72*time.Hour), 2, "garbage"); !errors.Is(err, audit.ErrInvalidCursor) {
		t.Errorf("a garbage cursor: %v, want ErrInvalidCursor", err)
	}
}
