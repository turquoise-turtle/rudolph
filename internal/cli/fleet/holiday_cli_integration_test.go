//go:build integration

package fleet

// Holiday mode for fleet health (owner decision 2, 2026-11-22). Over school holidays Macs are simply off, so the stale
// threshold of the fleet health page can be raised until a date, after which the normal one applies again by itself:
//
//	rudolph fleet holiday --stale-days <days> --until <YYYY-MM-DD>
//	rudolph fleet holiday --end
//
// The setting is one store-table item, PK "FleetHealth", SK "Config": StaleDays (the normal threshold; the job uses 10
// when it is absent, and this command never writes it), HolidayStaleDays (number) and HolidayUntil (UTC date,
// inclusive). --end removes HolidayStaleDays and HolidayUntil and leaves StaleDays as it is.
//
// Refused, with nothing stored or audited: stale days outside 1 to 365; a date that is not YYYY-MM-DD, is before today
// (UTC) or is more than 366 days after it; holiday days below the normal threshold (StaleDays, or 10); --until without
// --stale-days or the other way round; --end with either.
//
// Each change is audited (pkg/model/audit) with Action "fleet.holiday" and Target "fleet": the summary of a holiday
// names its days and date, the summary of an end says it ended.

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/clitest"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	modelaudit "github.com/airbnb/rudolph/pkg/model/audit"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestMain(m *testing.M) { os.Exit(ddblocal.Main(m)) }

var env = clitest.Env{Table: ddblocal.TableName, Region: ddblocal.Region}

func day(offset int) string { return time.Now().UTC().AddDate(0, 0, offset).Format("2006-01-02") }

func holidayItem(t *testing.T) map[string]awstypes.AttributeValue {
	t.Helper()
	out, err := ddblocal.Raw(t).GetItem(context.Background(), &awsdynamodb.GetItemInput{
		TableName:      aws.String(ddblocal.TableName),
		Key:            map[string]awstypes.AttributeValue{"PK": &awstypes.AttributeValueMemberS{Value: "FleetHealth"}, "SK": &awstypes.AttributeValueMemberS{Value: "Config"}},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.Item
}

func putNormalStaleDays(t *testing.T, days int) {
	t.Helper()
	if _, err := ddblocal.Raw(t).PutItem(context.Background(), &awsdynamodb.PutItemInput{
		TableName: aws.String(ddblocal.TableName),
		Item: map[string]awstypes.AttributeValue{
			"PK":        &awstypes.AttributeValueMemberS{Value: "FleetHealth"},
			"SK":        &awstypes.AttributeValueMemberS{Value: "Config"},
			"StaleDays": &awstypes.AttributeValueMemberN{Value: strconv.Itoa(days)},
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func num(item map[string]awstypes.AttributeValue, name string) (int, bool) {
	v, ok := item[name].(*awstypes.AttributeValueMemberN)
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(v.Value)
	return n, err == nil
}

func str(item map[string]awstypes.AttributeValue, name string) (string, bool) {
	v, ok := item[name].(*awstypes.AttributeValueMemberS)
	if !ok {
		return "", false
	}
	return v.Value, true
}

func holidayEntries(t *testing.T) []modelaudit.Entry {
	t.Helper()
	all, err := modelaudit.List(ddblocal.Client(t), time.Now().Add(-time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	var got []modelaudit.Entry
	for _, e := range all {
		if e.Action == "fleet.holiday" {
			got = append(got, e)
		}
	}
	return got
}

func fleetCLI(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return clitest.Run(t, env, FleetCmd, "yes\n", args...)
}

func TestFleetHoliday_SetAndEnd(t *testing.T) {
	ddblocal.Reset(t)
	until := day(30)
	out, err := fleetCLI(t, "holiday", "--stale-days", "60", "--until", until)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out, until) || !strings.Contains(out, "60") {
		t.Errorf("the output does not confirm 60 days until %s:\n%s", until, out)
	}
	item := holidayItem(t)
	if got, ok := num(item, "HolidayStaleDays"); !ok || got != 60 {
		t.Errorf("HolidayStaleDays %d (present %v), want 60", got, ok)
	}
	if got, _ := str(item, "HolidayUntil"); got != until {
		t.Errorf("HolidayUntil %q, want %q", got, until)
	}
	if _, ok := item["StaleDays"]; ok {
		t.Error("setting a holiday wrote StaleDays; the normal threshold stays absent (10) unless set")
	}
	entries := holidayEntries(t)
	if len(entries) != 1 {
		t.Fatalf("%d fleet.holiday audit entries, want 1", len(entries))
	}
	if entries[0].Target != "fleet" || !strings.Contains(entries[0].Summary, "60") || !strings.Contains(entries[0].Summary, until) {
		t.Errorf("audit entry %+v, want Target fleet and a summary naming 60 days and %s", entries[0], until)
	}
	if !strings.HasPrefix(entries[0].Actor, "cli:") {
		t.Errorf("audit Actor %q, want cli:<user>@<host>", entries[0].Actor)
	}

	if out, err := fleetCLI(t, "holiday", "--end"); err != nil {
		t.Fatalf("--end: %v\n%s", err, out)
	}
	item = holidayItem(t)
	for _, attr := range []string{"HolidayStaleDays", "HolidayUntil"} {
		if _, ok := item[attr]; ok {
			t.Errorf("--end left %s", attr)
		}
	}
	entries = holidayEntries(t)
	if len(entries) != 2 {
		t.Fatalf("%d fleet.holiday audit entries after --end, want 2", len(entries))
	}
	ended := false
	for _, e := range entries {
		if e.Target == "fleet" && strings.Contains(strings.ToLower(e.Summary), "end") {
			ended = true
		}
	}
	if !ended {
		t.Errorf("no audit entry says the holiday ended: %+v", entries)
	}
}

// The normal threshold, set elsewhere (the web view), survives a holiday and its end.
func TestFleetHoliday_KeepsTheNormalThreshold(t *testing.T) {
	ddblocal.Reset(t)
	putNormalStaleDays(t, 20)
	if out, err := fleetCLI(t, "holiday", "--stale-days", "20", "--until", day(0)); err != nil {
		t.Fatalf("holiday days equal to the normal threshold, until today: %v\n%s", err, out)
	}
	if got, _ := num(holidayItem(t), "StaleDays"); got != 20 {
		t.Errorf("StaleDays %d after setting a holiday, want 20 kept", got)
	}
	if out, err := fleetCLI(t, "holiday", "--end"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if got, _ := num(holidayItem(t), "StaleDays"); got != 20 {
		t.Errorf("StaleDays %d after --end, want 20 kept", got)
	}
}

func TestFleetHoliday_Boundaries(t *testing.T) {
	for name, args := range map[string][]string{
		"365 days":            {"holiday", "--stale-days", "365", "--until", day(10)},
		"the default normal":  {"holiday", "--stale-days", "10", "--until", day(10)},
		"until today":         {"holiday", "--stale-days", "30", "--until", day(0)},
		"until 366 days away": {"holiday", "--stale-days", "30", "--until", day(366)},
	} {
		t.Run(name, func(t *testing.T) {
			ddblocal.Reset(t)
			if out, err := fleetCLI(t, args...); err != nil {
				t.Fatalf("refused: %v\n%s", err, out)
			}
			if _, ok := holidayItem(t)["HolidayUntil"]; !ok {
				t.Error("nothing stored")
			}
		})
	}
}

func TestFleetHoliday_RefusedArguments(t *testing.T) {
	for name, c := range map[string]struct {
		normal int // StaleDays stored first; 0 for none
		args   []string
	}{
		"zero days":                   {0, []string{"holiday", "--stale-days", "0", "--until", day(10)}},
		"negative days":               {0, []string{"holiday", "--stale-days", "-5", "--until", day(10)}},
		"366 days":                    {0, []string{"holiday", "--stale-days", "366", "--until", day(10)}},
		"below the default normal":    {0, []string{"holiday", "--stale-days", "9", "--until", day(10)}},
		"below the stored normal":     {20, []string{"holiday", "--stale-days", "15", "--until", day(10)}},
		"until yesterday":             {0, []string{"holiday", "--stale-days", "30", "--until", day(-1)}},
		"until 367 days away":         {0, []string{"holiday", "--stale-days", "30", "--until", day(367)}},
		"until not a date":            {0, []string{"holiday", "--stale-days", "30", "--until", "tomorrow"}},
		"until an impossible date":    {0, []string{"holiday", "--stale-days", "30", "--until", "2027-02-30"}},
		"until with a time":           {0, []string{"holiday", "--stale-days", "30", "--until", day(10) + "T00:00:00Z"}},
		"days without until":          {0, []string{"holiday", "--stale-days", "30"}},
		"until without days":          {0, []string{"holiday", "--until", day(10)}},
		"end with days":               {0, []string{"holiday", "--end", "--stale-days", "30"}},
		"end with until":              {0, []string{"holiday", "--end", "--until", day(10)}},
		"neither a holiday nor --end": {0, []string{"holiday"}},
	} {
		t.Run(name, func(t *testing.T) {
			ddblocal.Reset(t)
			if c.normal != 0 {
				putNormalStaleDays(t, c.normal)
			}
			if out, err := fleetCLI(t, c.args...); err == nil {
				t.Fatalf("accepted:\n%s", out)
			}
			item := holidayItem(t)
			for _, attr := range []string{"HolidayStaleDays", "HolidayUntil"} {
				if _, ok := item[attr]; ok {
					t.Errorf("a refused holiday stored %s", attr)
				}
			}
			if got, _ := num(item, "StaleDays"); got != c.normal {
				t.Errorf("StaleDays %d after a refused command, want %d", got, c.normal)
			}
			if n := len(holidayEntries(t)); n != 0 {
				t.Errorf("a refused command was audited (%d entries)", n)
			}
		})
	}
}

// A new holiday replaces the one in force.
func TestFleetHoliday_ReplacesAHolidayInForce(t *testing.T) {
	ddblocal.Reset(t)
	if out, err := fleetCLI(t, "holiday", "--stale-days", "60", "--until", day(30)); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if out, err := fleetCLI(t, "holiday", "--stale-days", "21", "--until", day(5)); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	item := holidayItem(t)
	if got, _ := num(item, "HolidayStaleDays"); got != 21 {
		t.Errorf("HolidayStaleDays %d, want 21", got)
	}
	if got, _ := str(item, "HolidayUntil"); got != day(5) {
		t.Errorf("HolidayUntil %q, want %q", got, day(5))
	}
	if n := len(holidayEntries(t)); n != 2 {
		t.Errorf("%d audit entries, want 2", n)
	}
}
