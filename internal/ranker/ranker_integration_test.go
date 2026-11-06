//go:build integration

package ranker_test

// R8 of rudolph-web's docs/data-layer-design.md: an hourly job (a Lambda on an EventBridge schedule) builds the
// top-blocked and top-allowed binaries for the web view from the events table's day counters, so a page view is one
// GetItem and one Query instead of reading up to 90 days of counters.
//
// Input (written by the event writer, internal/handlers/eventupload): day counters `BinaryDay#<UTC date>` /
// `<sha256>` with numbers Blocked and Allowed and strings FileName, SigningID, TeamID; machine markers
// `BinarySeen#<sha256>` / `<machine ID>` with LastSeenDay (UTC date the Mac last uploaded an event of that binary).
//
// Output, for kind Blocked and Allowed and window 1, 7, 30 and 90 days (the window's last day is today, UTC):
//   - snapshot rows `TopSnapshot#<kind>#<window>#<snapshot id>` / `<rank, 6 digits from 000001>` with FileSHA256,
//     Count (sum of that kind over the window's days), Machines (markers with LastSeenDay inside the window),
//     FileName, SigningID, TeamID (from the newest day that has them), and ExpiresAfter (now + 2 days);
//   - then the pointer `Top#<kind>#<window>` / `Current` with Snapshot (the id) and GeneratedAt (RFC 3339), written
//     last so a reader never sees a half-written snapshot.
// Rows are ordered by Count (highest first), then FileSHA256; binaries with a count of 0 are left out; at most 500
// rows per list. The job never Scans.

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/ranker"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestMain(m *testing.M) { os.Exit(ddblocal.Main(m)) }

var (
	ctx = context.Background()
	now = time.Date(2026, 11, 6, 15, 30, 0, 0, time.UTC)
)

func sha(i int) string { return fmt.Sprintf("%064x", i) }

func day(daysAgo int) string { return now.AddDate(0, 0, -daysAgo).Format("2006-01-02") }

func put(t *testing.T, item map[string]any) {
	t.Helper()
	av, err := attributevalue.MarshalMap(item)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ddblocal.Raw(t).PutItem(ctx, &awsdynamodb.PutItemInput{TableName: aws.String(ddblocal.EventsTableName), Item: av}); err != nil {
		t.Fatal(err)
	}
}

func counter(t *testing.T, daysAgo, i, blocked, allowed int, name string) {
	t.Helper()
	put(t, map[string]any{"PK": "BinaryDay#" + day(daysAgo), "SK": sha(i), "Blocked": blocked, "Allowed": allowed,
		"FileName": name, "TeamID": "TEAM" + strconv.Itoa(i), "SigningID": "TEAM" + strconv.Itoa(i) + ":" + name})
}

func marker(t *testing.T, i int, machine string, daysAgo int) {
	t.Helper()
	put(t, map[string]any{"PK": "BinarySeen#" + sha(i), "SK": machine, "LastSeenDay": day(daysAgo)})
}

func run(t *testing.T, at time.Time) {
	t.Helper()
	if err := ranker.Run(ctx, ranker.Options{Client: ddblocal.Raw(t), Table: ddblocal.EventsTableName, Now: func() time.Time { return at }}); err != nil {
		t.Fatalf("ranker: %v", err)
	}
}

type row struct {
	SHA                         string
	Count, Machines             int
	FileName, SigningID, TeamID string
}

func list(t *testing.T, kind string, window int) []row {
	t.Helper()
	out, err := ddblocal.Raw(t).GetItem(ctx, &awsdynamodb.GetItemInput{
		TableName:      aws.String(ddblocal.EventsTableName),
		Key:            map[string]awstypes.AttributeValue{"PK": &awstypes.AttributeValueMemberS{Value: fmt.Sprintf("Top#%s#%d", kind, window)}, "SK": &awstypes.AttributeValueMemberS{Value: "Current"}},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	snap, ok := out.Item["Snapshot"].(*awstypes.AttributeValueMemberS)
	if !ok {
		t.Fatalf("no pointer for %s %d: %v", kind, window, out.Item)
	}
	if _, ok := out.Item["GeneratedAt"].(*awstypes.AttributeValueMemberS); !ok {
		t.Errorf("pointer for %s %d has no GeneratedAt", kind, window)
	}
	var rows []row
	var start map[string]awstypes.AttributeValue
	for {
		q, err := ddblocal.Raw(t).Query(ctx, &awsdynamodb.QueryInput{
			TableName:                 aws.String(ddblocal.EventsTableName),
			KeyConditionExpression:    aws.String("PK = :pk"),
			ExpressionAttributeValues: map[string]awstypes.AttributeValue{":pk": &awstypes.AttributeValueMemberS{Value: fmt.Sprintf("TopSnapshot#%s#%d#%s", kind, window, snap.Value)}},
			ExclusiveStartKey:         start,
		})
		if err != nil {
			t.Fatal(err)
		}
		for i, it := range q.Items {
			var r struct {
				SK, FileSHA256, FileName, SigningID, TeamID string
				Count, Machines                             int
				ExpiresAfter                                int64
			}
			if err := attributevalue.UnmarshalMap(it, &r); err != nil {
				t.Fatal(err)
			}
			if want := fmt.Sprintf("%06d", len(rows)+i+1); r.SK != want {
				t.Errorf("%s %d row %d has SK %q, want %q", kind, window, len(rows)+i+1, r.SK, want)
			}
			if exp := time.Unix(r.ExpiresAfter, 0); exp.Before(now.Add(24*time.Hour)) || exp.After(now.Add(3*24*time.Hour)) {
				t.Errorf("%s %d row ExpiresAfter %v, want about now + 2 days", kind, window, exp)
			}
			rows = append(rows, row{r.FileSHA256, r.Count, r.Machines, r.FileName, r.SigningID, r.TeamID})
		}
		if len(q.LastEvaluatedKey) == 0 {
			return rows
		}
		start = q.LastEvaluatedKey
	}
}

func summary(rows []row) string {
	var out []string
	for _, r := range rows {
		out = append(out, fmt.Sprintf("%d:%d/%d", mustIndex(r.SHA), r.Count, r.Machines))
	}
	return strings.Join(out, " ")
}

func mustIndex(s string) int {
	n, _ := strconv.ParseInt(s, 16, 64)
	return int(n)
}

func TestRanker_WindowsCountsAndMachines(t *testing.T) {
	ddblocal.Reset(t)
	// Binary 1: blocked today (5), 3 days ago (4) and 10 days ago (100).
	counter(t, 0, 1, 5, 0, "one")
	counter(t, 3, 1, 4, 0, "one")
	counter(t, 10, 1, 100, 0, "one")
	// Binary 2: blocked 3 days ago (20); allowed today (7).
	counter(t, 3, 2, 20, 0, "two")
	counter(t, 0, 2, 0, 7, "two")
	// Binary 3: allowed only, 40 days ago (3) and 6 days ago (2).
	counter(t, 40, 3, 0, 3, "three")
	counter(t, 6, 3, 0, 2, "three")
	marker(t, 1, "MAC-A", 0)
	marker(t, 1, "MAC-B", 3)
	marker(t, 1, "MAC-C", 10)
	marker(t, 2, "MAC-A", 3)
	marker(t, 3, "MAC-D", 40)
	marker(t, 3, "MAC-E", 6)
	run(t, now)

	for _, c := range []struct {
		kind   string
		window int
		want   string
	}{
		{"Blocked", 1, "1:5/1"},
		{"Blocked", 7, "2:20/1 1:9/2"},
		{"Blocked", 30, "1:109/3 2:20/1"},
		{"Blocked", 90, "1:109/3 2:20/1"},
		{"Allowed", 1, "2:7/0"},
		{"Allowed", 7, "2:7/1 3:2/1"},
		{"Allowed", 30, "2:7/1 3:2/1"},
		{"Allowed", 90, "2:7/1 3:5/2"},
	} {
		if got := summary(list(t, c.kind, c.window)); got != c.want {
			t.Errorf("%s %d days: %s, want %s (sha:count/machines)", c.kind, c.window, got, c.want)
		}
	}
	top := list(t, "Blocked", 7)
	if len(top) > 0 && (top[0].FileName != "two" || top[0].TeamID != "TEAM2" || top[0].SigningID != "TEAM2:two") {
		t.Errorf("display fields %+v", top[0])
	}
}

func TestRanker_TiesAndTheCap(t *testing.T) {
	ddblocal.Reset(t)
	for i := 1; i <= 600; i++ {
		counter(t, 0, i, 1+i%3, 0, "bin")
	}
	run(t, now)
	rows := list(t, "Blocked", 1)
	if len(rows) != 500 {
		t.Fatalf("%d rows, want the top 500", len(rows))
	}
	for i := 1; i < len(rows); i++ {
		a, b := rows[i-1], rows[i]
		if a.Count < b.Count || (a.Count == b.Count && a.SHA > b.SHA) {
			t.Fatalf("rows %d and %d out of order: %+v then %+v", i, i+1, a, b)
		}
	}
	// 200 binaries each with counts 3, 2 and 1: the top 500 end with 100 of the count-1 binaries, the lowest hashes.
	if rows[0].Count != 3 || rows[399].Count != 2 || rows[400].Count != 1 || rows[499].Count != 1 {
		t.Errorf("counts at ranks 1, 400, 401, 500: %d %d %d %d; want 3 2 1 1", rows[0].Count, rows[399].Count, rows[400].Count, rows[499].Count)
	}
}

// Each run writes a new snapshot and moves the pointer; a reader holding the old snapshot id can still read it until
// it expires.
func TestRanker_EachRunReplacesThePointer(t *testing.T) {
	ddblocal.Reset(t)
	counter(t, 0, 1, 1, 0, "one")
	run(t, now)
	first := list(t, "Blocked", 1)
	counter(t, 0, 2, 9, 0, "two")
	run(t, now.Add(time.Hour))
	if got := summary(list(t, "Blocked", 1)); got != "2:9/0 1:1/0" {
		t.Errorf("after the second run: %s", got)
	}
	if summary(first) != "1:1/0" {
		t.Errorf("first snapshot read %s", summary(first))
	}
}

func TestRanker_NeverScans(t *testing.T) {
	ddblocal.Reset(t)
	counter(t, 0, 1, 1, 0, "one")
	ddblocal.ResetCounts()
	run(t, now)
	if n := ddblocal.Counts()["Scan"]; n != 0 {
		t.Errorf("%d Scan calls", n)
	}
}
