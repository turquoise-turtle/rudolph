//go:build integration

package ranker_test

// Top unknowns (owner, 2026-11-11): the web view lists the binaries no rule decided, so admins can see what to allow
// or block next. Santa's own decision is the signal: BLOCK_UNKNOWN (lockdown) and ALLOW_UNKNOWN (monitor) mean no
// rule of any scope matched on that Mac at that time. The event writer counts them on the day counter as
// UnknownBlocked and UnknownAllowed (alongside Blocked and Allowed, which still count every stored decision).
//
// The ranker builds three more kinds, each over the same windows (1, 7, 30, 90 days), so the page's "all / blocked /
// allowed" filter reads one list:
//   - Unknown: UnknownBlocked + UnknownAllowed;
//   - UnknownBlocked;
//   - UnknownAllowed.
// They are chosen like the other lists (the 500 highest counts), but the first 100 rows, which carry Machines, are
// ordered for deciding what to allow: binaries that have no global rule yet first, then by Machines (highest first),
// then Count (highest first), then FileSHA256. Rows past 100 keep Count order.
//
// GlobalRule: on the first 100 rows of the unknown lists, the sort key of a global rule that now covers the binary,
// looked up by GetItem on the store table's GlobalRules partition: Binary#<sha>, then SigningID#<signing ID>, then
// TeamID#<team ID> (the first that exists). Certificate and CDHash rules can't be matched (counters don't hold them),
// and machine and group rules don't count: an admin deciding fleet-wide wants to know about global rules. When the
// ranker isn't given the store table, no lookups are made and no row has GlobalRule.

import (
	"fmt"
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

// unknownCounter writes a day counter whose executions were all unknown: Blocked and Allowed include them, as the
// event writer counts them.
func unknownCounter(t *testing.T, daysAgo, i, unknownBlocked, unknownAllowed int, name string) {
	t.Helper()
	put(t, map[string]any{"PK": "BinaryDay#" + day(daysAgo), "SK": sha(i),
		"Blocked": unknownBlocked, "Allowed": unknownAllowed,
		"UnknownBlocked": unknownBlocked, "UnknownAllowed": unknownAllowed,
		"FileName": name, "TeamID": fmt.Sprintf("TEAM%d", i), "SigningID": fmt.Sprintf("TEAM%d:%s", i, name)})
}

func storeRule(t *testing.T, pk, sk string) {
	t.Helper()
	av, err := attributevalue.MarshalMap(map[string]any{"PK": pk, "SK": sk, "Policy": 2, "RuleType": 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ddblocal.Raw(t).PutItem(ctx, &awsdynamodb.PutItemInput{TableName: aws.String(ddblocal.TableName), Item: av}); err != nil {
		t.Fatal(err)
	}
}

func runWithStore(t *testing.T) {
	t.Helper()
	err := ranker.Run(ctx, ranker.Options{Client: ddblocal.Raw(t), Table: ddblocal.EventsTableName, StoreTable: ddblocal.TableName,
		Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("ranker: %v", err)
	}
}

type urow struct {
	SHA        string
	Count      int
	Machines   int
	GlobalRule string
}

func unknownList(t *testing.T, kind string, window int) []urow {
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
		t.Fatalf("no pointer for %s %d", kind, window)
	}
	var rows []urow
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
		for _, it := range q.Items {
			var r struct {
				FileSHA256, GlobalRule string
				Count, Machines        int
			}
			if err := attributevalue.UnmarshalMap(it, &r); err != nil {
				t.Fatal(err)
			}
			rows = append(rows, urow{r.FileSHA256, r.Count, r.Machines, r.GlobalRule})
		}
		if len(q.LastEvaluatedKey) == 0 {
			return rows
		}
		start = q.LastEvaluatedKey
	}
}

func usummary(rows []urow) string {
	var out []string
	for _, r := range rows {
		s := fmt.Sprintf("%d:%d/%d", mustIndex(r.SHA), r.Count, r.Machines)
		if r.GlobalRule != "" {
			s += "[" + r.GlobalRule + "]"
		}
		out = append(out, s)
	}
	return strings.Join(out, " ")
}

func TestRanker_UnknownKindsAndWindows(t *testing.T) {
	ddblocal.Reset(t)
	// Binary 1: blocked as unknown today (4) and 10 days ago (6).
	unknownCounter(t, 0, 1, 4, 0, "one")
	unknownCounter(t, 10, 1, 6, 0, "one")
	// Binary 2: allowed as unknown 2 days ago (3), blocked as unknown today (1).
	unknownCounter(t, 2, 2, 0, 3, "two")
	unknownCounter(t, 0, 2, 1, 0, "two")
	// Binary 3: blocked by a rule only (no unknown counts): in Blocked, never in an unknown list.
	counter(t, 0, 3, 50, 0, "three")
	marker(t, 1, "MAC-A", 0)
	marker(t, 2, "MAC-A", 0)
	runWithStore(t)

	for _, c := range []struct {
		kind   string
		window int
		want   string
	}{
		{"Unknown", 1, "1:4/1 2:1/1"},
		{"Unknown", 7, "1:4/1 2:4/1"},
		{"Unknown", 30, "1:10/1 2:4/1"},
		{"UnknownBlocked", 1, "1:4/1 2:1/1"},
		{"UnknownBlocked", 30, "1:10/1 2:1/1"},
		{"UnknownAllowed", 1, ""},
		{"UnknownAllowed", 7, "2:3/1"},
		{"UnknownAllowed", 90, "2:3/1"},
		// The existing lists are unchanged by the new attributes.
		{"Blocked", 1, "3:50/0 1:4/1 2:1/1"},
		{"Allowed", 7, "2:3/1"},
	} {
		if got := usummary(unknownList(t, c.kind, c.window)); got != c.want {
			t.Errorf("%s %d days: %q, want %q (sha:count/machines)", c.kind, c.window, got, c.want)
		}
	}
}

// Many Macs running a binary say more about what to allow than one Mac running it in a loop.
func TestRanker_UnknownFirstRowsOrderedByMachines(t *testing.T) {
	ddblocal.Reset(t)
	unknownCounter(t, 0, 1, 100, 0, "loop") // one Mac, crash loop
	marker(t, 1, "MAC-A", 0)
	for _, i := range []int{2, 3} {
		unknownCounter(t, 0, i, 5, 0, "tool")
		for _, m := range []string{"MAC-A", "MAC-B", "MAC-C"} {
			marker(t, i, m, 0)
		}
	}
	unknownCounter(t, 0, 4, 7, 0, "two-macs")
	marker(t, 4, "MAC-A", 0)
	marker(t, 4, "MAC-B", 0)
	runWithStore(t)

	if got, want := usummary(unknownList(t, "Unknown", 7)), "2:5/3 3:5/3 4:7/2 1:100/1"; got != want {
		t.Errorf("Unknown 7 days: %s, want %s (Macs, then count, then SHA)", got, want)
	}
	if got, want := usummary(unknownList(t, "Blocked", 7)), "1:100/1 4:7/2 2:5/3 3:5/3"; got != want {
		t.Errorf("Blocked 7 days: %s, want %s (the existing lists stay in count order)", got, want)
	}
}

// Rows past the first 100 have no Machines and keep count order; reordering never pulls a row into the first 100.
func TestRanker_UnknownRowsPastAHundredKeepCountOrder(t *testing.T) {
	ddblocal.Reset(t)
	for i := 1; i <= 150; i++ {
		unknownCounter(t, 0, i, 1000-i, 0, "bin")
		marker(t, i, "MAC-A", 0)
	}
	for _, m := range []string{"MAC-B", "MAC-C", "MAC-D"} {
		marker(t, 150, m, 0) // most Macs, lowest count
		marker(t, 60, m, 0)
	}
	runWithStore(t)
	rows := unknownList(t, "Unknown", 1)
	if len(rows) != 150 {
		t.Fatalf("%d rows, want 150", len(rows))
	}
	if mustIndex(rows[0].SHA) != 60 || rows[0].Machines != 4 {
		t.Errorf("row 1 is %d with %d Macs, want binary 60 with 4 Macs", mustIndex(rows[0].SHA), rows[0].Machines)
	}
	for i := 100; i < 150; i++ {
		if got := mustIndex(rows[i].SHA); got != i+1 {
			t.Errorf("row %d is binary %d, want %d (count order past the first 100)", i+1, got, i+1)
		}
	}
}

func TestRanker_UnknownMarksGlobalRulesAndSortsThemLast(t *testing.T) {
	ddblocal.Reset(t)
	for i := 1; i <= 6; i++ {
		unknownCounter(t, 0, i, 10*i, 0, fmt.Sprintf("bin%d", i))
		marker(t, i, "MAC-A", 0)
	}
	storeRule(t, "GlobalRules", "Binary#"+sha(6))
	storeRule(t, "GlobalRules", "SigningID#TEAM5:bin5")
	storeRule(t, "GlobalRules", "TeamID#TEAM4")
	// Not global: these don't mark the row.
	storeRule(t, "MachineRules#MAC-A", "Binary#"+sha(3))
	storeRule(t, "GroupRules#lab", "Binary#"+sha(2))
	// A binary with both a binary and a team ID rule names the binary rule.
	storeRule(t, "GlobalRules", "TeamID#TEAM6")
	runWithStore(t)

	want := "3:30/1 2:20/1 1:10/1 6:60/1[Binary#" + sha(6) + "] 5:50/1[SigningID#TEAM5:bin5] 4:40/1[TeamID#TEAM4]"
	if got := usummary(unknownList(t, "Unknown", 1)); got != want {
		t.Errorf("Unknown 1 day:\n got %s\nwant %s\n(no global rule first; then Macs, count, SHA)", got, want)
	}
	if got := unknownList(t, "Blocked", 1); len(got) > 0 && got[0].GlobalRule != "" {
		t.Errorf("the Blocked list carries GlobalRule %q; only the unknown lists do", got[0].GlobalRule)
	}
}

// The rule lookups are point reads, at most three per binary (each binary once, however many lists it is in), and
// never a Scan.
func TestRanker_UnknownRuleLookupsAreBounded(t *testing.T) {
	ddblocal.Reset(t)
	for i := 1; i <= 120; i++ {
		unknownCounter(t, 0, i, 200-i, i%2, "bin")
		unknownCounter(t, 20, i, 1, 0, "bin")
	}
	ddblocal.ResetCounts()
	runWithStore(t)
	counts := ddblocal.Counts()
	if counts["Scan"] != 0 {
		t.Errorf("%d Scan calls", counts["Scan"])
	}
	// The first 100 rows of every unknown list are among binaries 1–100 (counts fall with i), plus up to 20 more
	// whose allowed-only or 30/90-day totals rank them in.
	if n := counts["GetItem"] + counts["BatchGetItem"]; n == 0 || n > 3*120 {
		t.Errorf("%d rule lookups, want between 1 and %d (three per binary at most)", n, 3*120)
	}
}

func TestRanker_WithoutTheStoreTableNoLookups(t *testing.T) {
	ddblocal.Reset(t)
	unknownCounter(t, 0, 1, 1, 0, "one")
	storeRule(t, "GlobalRules", "Binary#"+sha(1))
	ddblocal.ResetCounts()
	run(t, now) // no StoreTable
	if n := ddblocal.Counts()["GetItem"]; n != 0 {
		t.Errorf("%d GetItem calls without a store table", n)
	}
	if got := unknownList(t, "Unknown", 1); len(got) != 1 || got[0].GlobalRule != "" {
		t.Errorf("Unknown 1 day %+v; want binary 1 without GlobalRule", got)
	}
}
