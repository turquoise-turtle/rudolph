//go:build integration

package audit_test

// The audit API rudolph-web will use (plan F2): Record writes an entry for any actor; List reads them back newest
// first, across days, honouring since and limit.

import (
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
