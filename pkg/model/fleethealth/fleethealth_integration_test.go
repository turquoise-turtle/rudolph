//go:build integration

package fleethealth_test

// SetMinSantaVersion and ClearMinSantaVersion, against DynamoDB Local. The CLI (`rudolph fleet min-santa-version`) and
// rudolph-web change the minimum Santa version only through them; neither writes an audit entry (callers do, as with
// holidays).
//
//   - SetMinSantaVersion(client, version) (Config, error) checks version with ValidateMinSantaVersion and stores the
//     normalised value as the string attribute MinSantaVersion of FleetHealth / Config, creating the item when there is
//     none. It sets that one attribute: StaleDays, HolidayStaleDays and HolidayUntil stay exactly as stored. It returns
//     the setting as stored (the other attributes as read, MinSantaVersion the new value). A refused value writes
//     nothing.
//   - ClearMinSantaVersion(client) (Config, error) removes MinSantaVersion, so the default applies again, and leaves
//     the other attributes as they are. It returns the setting as it was before (so the caller can say what was
//     cleared), or ErrNoMinSantaVersion when the item holds no MinSantaVersion attribute (nothing is written then, and
//     no item is created). The check is on the raw attribute, not on what FromItem decodes: an unusable stored value,
//     which FromItem hides as "", is removed too and is not ErrNoMinSantaVersion.

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/model/fleethealth"
)

func TestMain(m *testing.M) { os.Exit(ddblocal.Main(m)) }

// A day the 2027-02-01 holiday of these tests is ahead of.
var holidayClock = clock.FrozenTimeProvider{Current: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)}

func writer(t *testing.T) fleethealth.Writer {
	t.Helper()
	w, ok := ddblocal.Client(t).(fleethealth.Writer)
	if !ok {
		t.Fatal("Rudolph's DynamoDB client is not a fleethealth.Writer")
	}
	return w
}

func key() map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"PK": &types.AttributeValueMemberS{Value: "FleetHealth"},
		"SK": &types.AttributeValueMemberS{Value: "Config"},
	}
}

func rawItem(t *testing.T) map[string]types.AttributeValue {
	t.Helper()
	out, err := ddblocal.Raw(t).GetItem(context.Background(), &awsdynamodb.GetItemInput{
		TableName: aws.String(ddblocal.TableName), Key: key(), ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	return out.Item
}

// putRaw stores the item as given, as a hand edit or another writer would leave it.
func putRaw(t *testing.T, attrs map[string]types.AttributeValue) {
	t.Helper()
	item := key()
	for k, v := range attrs {
		item[k] = v
	}
	if _, err := ddblocal.Raw(t).PutItem(context.Background(), &awsdynamodb.PutItemInput{TableName: aws.String(ddblocal.TableName), Item: item}); err != nil {
		t.Fatal(err)
	}
}

// holidayAttrs is a normal threshold and a holiday, as `rudolph fleet holiday` and the web view store them.
func holidayAttrs() map[string]types.AttributeValue {
	return map[string]types.AttributeValue{
		"StaleDays":        &types.AttributeValueMemberN{Value: "14"},
		"HolidayStaleDays": &types.AttributeValueMemberN{Value: "60"},
		"HolidayUntil":     &types.AttributeValueMemberS{Value: "2027-02-01"},
	}
}

func assertHolidayKept(t *testing.T, item map[string]types.AttributeValue) {
	t.Helper()
	for name, want := range holidayAttrs() {
		got, ok := item[name]
		if !ok {
			t.Errorf("%s was removed", name)
			continue
		}
		if !attrEqual(got, want) {
			t.Errorf("%s is %#v, want %#v kept", name, got, want)
		}
	}
}

func attrEqual(a, b types.AttributeValue) bool {
	switch a := a.(type) {
	case *types.AttributeValueMemberS:
		b, ok := b.(*types.AttributeValueMemberS)
		return ok && a.Value == b.Value
	case *types.AttributeValueMemberN:
		b, ok := b.(*types.AttributeValueMemberN)
		return ok && a.Value == b.Value
	}
	return false
}

func storedMinSantaVersion(item map[string]types.AttributeValue) (string, bool) {
	v, ok := item["MinSantaVersion"].(*types.AttributeValueMemberS)
	if !ok {
		return "", false
	}
	return v.Value, true
}

func TestSetMinSantaVersion_CreatesTheItem(t *testing.T) {
	ddblocal.Reset(t)
	got, err := fleethealth.SetMinSantaVersion(writer(t), "2026.10")
	if err != nil {
		t.Fatal(err)
	}
	if want := (fleethealth.Config{MinSantaVersion: "2026.10"}); got != want {
		t.Errorf("returned %+v, want %+v", got, want)
	}
	item := rawItem(t)
	if v, ok := storedMinSantaVersion(item); !ok || v != "2026.10" {
		t.Errorf("stored MinSantaVersion %q (a string: %v), want the string 2026.10", v, ok)
	}
	for _, name := range []string{"StaleDays", "HolidayStaleDays", "HolidayUntil"} {
		if _, ok := item[name]; ok {
			t.Errorf("setting the minimum Santa version wrote %s", name)
		}
	}
	if read, err := fleethealth.Get(writer(t), true); err != nil || read.MinSantaVersion != "2026.10" {
		t.Errorf("Get after setting: %+v, %v; want MinSantaVersion 2026.10", read, err)
	}
}

func TestSetMinSantaVersion_KeepsTheStaleThresholdAndHoliday(t *testing.T) {
	ddblocal.Reset(t)
	putRaw(t, holidayAttrs())
	got, err := fleethealth.SetMinSantaVersion(writer(t), "off")
	if err != nil {
		t.Fatal(err)
	}
	want := fleethealth.Config{StaleDays: 14, HolidayStaleDays: 60, HolidayUntil: "2027-02-01", MinSantaVersion: "off"}
	if got != want {
		t.Errorf("returned %+v, want %+v", got, want)
	}
	item := rawItem(t)
	assertHolidayKept(t, item)
	if v, _ := storedMinSantaVersion(item); v != "off" {
		t.Errorf("stored MinSantaVersion %q, want off", v)
	}
}

// The value stored is the normalised one, and a new value replaces the one stored, usable or not.
func TestSetMinSantaVersion_NormalisesAndReplaces(t *testing.T) {
	for _, c := range []struct {
		before types.AttributeValue
		in     string
		want   string
	}{
		{nil, " 2026.10.1 ", "2026.10.1"},
		{nil, "OFF", "off"},
		{&types.AttributeValueMemberS{Value: "2026.10"}, "2026.11", "2026.11"},
		{&types.AttributeValueMemberS{Value: "off"}, "2026.9", "2026.9"},
		{&types.AttributeValueMemberS{Value: "garbage"}, "2026.10", "2026.10"},
		{&types.AttributeValueMemberN{Value: "2026"}, "2026.10", "2026.10"},
	} {
		ddblocal.Reset(t)
		if c.before != nil {
			putRaw(t, map[string]types.AttributeValue{"MinSantaVersion": c.before})
		}
		got, err := fleethealth.SetMinSantaVersion(writer(t), c.in)
		if err != nil {
			t.Errorf("SetMinSantaVersion(%q) over %#v: %v", c.in, c.before, err)
			continue
		}
		if got.MinSantaVersion != c.want {
			t.Errorf("SetMinSantaVersion(%q) returned MinSantaVersion %q, want %q", c.in, got.MinSantaVersion, c.want)
		}
		if v, ok := storedMinSantaVersion(rawItem(t)); !ok || v != c.want {
			t.Errorf("SetMinSantaVersion(%q) stored %q (a string: %v), want %q", c.in, v, ok, c.want)
		}
	}
}

func TestSetMinSantaVersion_RefusesAnUnusableValue(t *testing.T) {
	for _, in := range []string{"", "  ", "garbage", "2026", "v2026.9", "latest", "2026.10." + strings.Repeat("0", 24) + "1"} {
		ddblocal.Reset(t)
		attrs := holidayAttrs()
		attrs["MinSantaVersion"] = &types.AttributeValueMemberS{Value: "2026.10"}
		putRaw(t, attrs)
		got, err := fleethealth.SetMinSantaVersion(writer(t), in)
		if err == nil {
			t.Errorf("SetMinSantaVersion(%q) accepted: %+v", in, got)
			continue
		}
		if msg := err.Error(); msg == "not implemented" || !strings.Contains(msg, "off") || !strings.Contains(msg, "2026.") {
			t.Errorf("SetMinSantaVersion(%q) error %q does not name \"off\" and an example release", in, msg)
		}
		item := rawItem(t)
		if v, _ := storedMinSantaVersion(item); v != "2026.10" {
			t.Errorf("a refused SetMinSantaVersion(%q) changed the stored value to %q", in, v)
		}
		assertHolidayKept(t, item)
	}
}

func TestSetMinSantaVersion_RefusedOnAnEmptyTableCreatesNothing(t *testing.T) {
	ddblocal.Reset(t)
	if _, err := fleethealth.SetMinSantaVersion(writer(t), "garbage"); err == nil {
		t.Fatal("accepted garbage")
	}
	if item := rawItem(t); len(item) != 0 {
		t.Errorf("a refused value created the item: %v", item)
	}
}

func TestClearMinSantaVersion_RemovesItAndKeepsTheRest(t *testing.T) {
	for _, stored := range []string{"2026.10", "off"} {
		ddblocal.Reset(t)
		attrs := holidayAttrs()
		attrs["MinSantaVersion"] = &types.AttributeValueMemberS{Value: stored}
		putRaw(t, attrs)
		before, err := fleethealth.ClearMinSantaVersion(writer(t))
		if err != nil {
			t.Errorf("clearing %q: %v", stored, err)
			continue
		}
		want := fleethealth.Config{StaleDays: 14, HolidayStaleDays: 60, HolidayUntil: "2027-02-01", MinSantaVersion: stored}
		if before != want {
			t.Errorf("clearing %q returned %+v, want the setting as it was, %+v", stored, before, want)
		}
		item := rawItem(t)
		if _, ok := item["MinSantaVersion"]; ok {
			t.Errorf("clearing %q left MinSantaVersion", stored)
		}
		assertHolidayKept(t, item)
		if version, off, fromStore := fleethealth.FromItem(item).MinSantaVersionInForce(); version != "2026.9" || off || fromStore {
			t.Errorf("after clearing %q the version in force is (%q, off %v, stored %v), want the default 2026.9", stored, version, off, fromStore)
		}
	}
}

// A hand-edited value that FromItem hides is still an attribute to clear: --default is how an operator gets rid of it.
func TestClearMinSantaVersion_RemovesAnUnusableValue(t *testing.T) {
	for _, bad := range []types.AttributeValue{
		&types.AttributeValueMemberS{Value: "garbage"},
		&types.AttributeValueMemberS{Value: ""},
		&types.AttributeValueMemberN{Value: "2026"},
	} {
		ddblocal.Reset(t)
		attrs := holidayAttrs()
		attrs["MinSantaVersion"] = bad
		putRaw(t, attrs)
		before, err := fleethealth.ClearMinSantaVersion(writer(t))
		if err != nil {
			t.Errorf("clearing the unusable %#v: %v", bad, err)
			continue
		}
		if before.MinSantaVersion != "" {
			t.Errorf("clearing the unusable %#v returned MinSantaVersion %q, want \"\" (FromItem drops it)", bad, before.MinSantaVersion)
		}
		item := rawItem(t)
		if _, ok := item["MinSantaVersion"]; ok {
			t.Errorf("clearing left the unusable %#v", bad)
		}
		assertHolidayKept(t, item)
	}
}

func TestClearMinSantaVersion_NothingStored(t *testing.T) {
	ddblocal.Reset(t)
	if _, err := fleethealth.ClearMinSantaVersion(writer(t)); !errors.Is(err, fleethealth.ErrNoMinSantaVersion) {
		t.Errorf("clearing with no item: %v, want ErrNoMinSantaVersion", err)
	}
	if item := rawItem(t); len(item) != 0 {
		t.Errorf("clearing with no item created one: %v", item)
	}

	putRaw(t, holidayAttrs())
	if _, err := fleethealth.ClearMinSantaVersion(writer(t)); !errors.Is(err, fleethealth.ErrNoMinSantaVersion) {
		t.Errorf("clearing with an item without MinSantaVersion: %v, want ErrNoMinSantaVersion", err)
	}
	assertHolidayKept(t, rawItem(t))
}

// The holiday functions leave the minimum Santa version alone, and the other way round.
func TestHolidayAndMinSantaVersionAreIndependent(t *testing.T) {
	ddblocal.Reset(t)
	if _, err := fleethealth.SetMinSantaVersion(writer(t), "2026.10"); err != nil {
		t.Fatal(err)
	}
	if _, err := fleethealth.SetHoliday(holidayClock, writer(t), 60, "2027-02-01"); err != nil {
		t.Fatal(err)
	}
	if v, _ := storedMinSantaVersion(rawItem(t)); v != "2026.10" {
		t.Errorf("SetHoliday changed MinSantaVersion to %q", v)
	}
	if _, err := fleethealth.EndHoliday(writer(t)); err != nil {
		t.Fatal(err)
	}
	if v, _ := storedMinSantaVersion(rawItem(t)); v != "2026.10" {
		t.Errorf("EndHoliday changed MinSantaVersion to %q", v)
	}
	if _, err := fleethealth.SetHoliday(holidayClock, writer(t), 60, "2027-02-01"); err != nil {
		t.Fatal(err)
	}
	if _, err := fleethealth.ClearMinSantaVersion(writer(t)); err != nil {
		t.Fatal(err)
	}
	if c, err := fleethealth.Get(writer(t), true); err != nil || c.HolidayStaleDays != 60 || c.HolidayUntil != "2027-02-01" {
		t.Errorf("after ClearMinSantaVersion the holiday is %+v (%v), want 60 days until 2027-02-01 kept", c, err)
	}
}
