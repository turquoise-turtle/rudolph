package syncstate

// Lockdown readiness (Fix 3 of the fleet health design, owner approved 2026-11-22): the sync state carries
// FirstSeenDay, the UTC date (2006-01-02) of the first preflight Rudolph has on record for the Mac, so the fleet job can
// tell how many days of unknown binaries it has observed. Every preflight replaces the row; ContinueFrom carries the
// date from the previous row and sets it only when there is none:
//   - no previous row (a new Mac, or its row expired): today, from the preflight's clock;
//   - a previous row with FirstSeenDay: kept as it is, however long ago;
//   - a previous row without one (written before this change): today.
// It is stored as the string attribute "FirstSeenDay". There is no backfill: Macs that synced before the deploy are
// first seen on the day of their first sync after it.

import (
	"testing"
	"time"

	"github.com/airbnb/rudolph/pkg/clock"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// firstSeenDay marshals the row as preflight stores it and returns its FirstSeenDay.
func firstSeenDay(t *testing.T, row SyncStateRow) (string, bool) {
	t.Helper()
	item, err := attributevalue.MarshalMap(row)
	if err != nil {
		t.Fatal(err)
	}
	v, ok := item["FirstSeenDay"].(*types.AttributeValueMemberS)
	if !ok {
		return "", false
	}
	return v.Value, true
}

// previousRow is a stored sync state, with FirstSeenDay when day is not empty.
func previousRow(t *testing.T, day string) *SyncStateRow {
	t.Helper()
	item := map[string]types.AttributeValue{
		"PK":          &types.AttributeValueMemberS{Value: "Machine#AAAAAAAA-1111-4111-8111-AAAAAAAAAAAA"},
		"SK":          &types.AttributeValueMemberS{Value: "SyncState"},
		"MachineID":   &types.AttributeValueMemberS{Value: "AAAAAAAA-1111-4111-8111-AAAAAAAAAAAA"},
		"PreflightAt": &types.AttributeValueMemberS{Value: "2026-11-21T09:00:00Z"},
		"SyncSeq":     &types.AttributeValueMemberN{Value: "1795000000000"},
		"DataType":    &types.AttributeValueMemberS{Value: "SyncState"},
	}
	if day != "" {
		item["FirstSeenDay"] = &types.AttributeValueMemberS{Value: day}
	}
	var row SyncStateRow
	if err := attributevalue.UnmarshalMap(item, &row); err != nil {
		t.Fatal(err)
	}
	if got, ok := firstSeenDay(t, row); day != "" && (!ok || got != day) {
		t.Fatalf("a stored FirstSeenDay %q does not survive loading and storing the row (got %q, present %v)", day, got, ok)
	}
	return &row
}

func TestSyncState_FirstSeenDayCarriedAndSetOnce(t *testing.T) {
	now := time.Date(2026, 11, 22, 23, 45, 0, 0, time.UTC)
	tp := timeProviderAt(now)
	for name, c := range map[string]struct {
		prev *SyncStateRow
		want string
	}{
		"no previous row":                 {nil, "2026-11-22"},
		"previous row with a first day":   {previousRow(t, "2026-01-05"), "2026-01-05"},
		"previous row without first day":  {previousRow(t, ""), "2026-11-22"},
		"previous row first seen earlier": {previousRow(t, "2026-11-21"), "2026-11-21"},
	} {
		t.Run(name, func(t *testing.T) {
			row := CreateNewSyncState(tp, "AAAAAAAA-1111-4111-8111-AAAAAAAAAAAA", false, "", 50, "")
			row.ContinueFrom(c.prev, "digest", now)
			got, ok := firstSeenDay(t, row)
			if !ok || got != c.want {
				t.Errorf("FirstSeenDay %q (present %v), want %q", got, ok, c.want)
			}
		})
	}
}

// The date is the UTC date even when the process runs in another time zone.
func TestSyncState_FirstSeenDayIsTheUTCDate(t *testing.T) {
	sydney := time.FixedZone("AEDT", 11*60*60)
	now := time.Date(2026, 11, 23, 8, 0, 0, 0, sydney) // 2026-11-22T21:00:00Z
	row := CreateNewSyncState(timeProviderAt(now), "AAAAAAAA-1111-4111-8111-AAAAAAAAAAAA", false, "", 50, "")
	row.ContinueFrom(nil, "digest", now)
	if got, _ := firstSeenDay(t, row); got != "2026-11-22" {
		t.Errorf("FirstSeenDay %q, want the UTC date 2026-11-22", got)
	}
}

func timeProviderAt(now time.Time) clock.TimeProvider { return clock.FrozenTimeProvider{Current: now} }
