package eventupload

// Lockdown readiness (Fix 3 of the fleet health design, owner approved 2026-11-22): beside the BinarySeen# markers,
// eventupload writes one marker per Mac and unknown binary, so the fleet job can count each Mac's unknown binaries of
// the last 7 and 30 days with one bounded Query:
//
//	PK "UnknownSeen#<machine ID>", SK "<file SHA-256>"
//	LastDay      UTC date of the upload, from Rudolph's clock (a Mac cannot backdate it)
//	ExpiresAfter upload time + 35 days (Unix seconds)
//	FileName, SigningID, TeamID  only values pkg/counterfields calls usable (left out whole, never cut), each from the
//	             first event of the binary in the upload that has a usable one, as on the day counters
//
// - Only binaries with a stored BLOCK_UNKNOWN or ALLOW_UNKNOWN event of the upload get one; a binary that also has
//   events of other decisions still gets one. Blocked-by-rule and collected allowed-by-rule executions do not, nor do
//   events the store drops.
// - One plain PutItem per binary per upload, however many events of it the upload holds.
// - Written for every such binary, new or already stored, so Santa's retry of an upload repairs a lost marker write.
// - No TS, Fleet or Decision attribute: the marker enters none of the events table's indexes and costs one write unit.
// - A failed marker write fails Store, so the handler answers 5xx and Santa uploads again.
//
// The end-to-end behaviour (500, retry, capacity) is in internal/handlers/unknown_marker_integration_test.go.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamotypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const unknownMarkerPrefix = "UnknownSeen#"

// markerRecorder is an eventStoreAPI that keeps every item put, can answer every event transaction as if its events
// were already stored, and can fail the marker puts.
type markerRecorder struct {
	mu            sync.Mutex
	puts          []map[string]dynamotypes.AttributeValue
	alreadyStored bool
	failMarkers   bool
}

func (r *markerRecorder) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if pk, _ := in.Item["PK"].(*dynamotypes.AttributeValueMemberS); pk != nil && strings.HasPrefix(pk.Value, unknownMarkerPrefix) && r.failMarkers {
		return nil, errors.New("injected marker failure")
	}
	r.puts = append(r.puts, in.Item)
	return &dynamodb.PutItemOutput{}, nil
}

func (r *markerRecorder) TransactWriteItems(_ context.Context, in *dynamodb.TransactWriteItemsInput, _ ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.alreadyStored {
		var reasons []dynamotypes.CancellationReason
		for _, item := range in.TransactItems {
			code := "None"
			if item.Put != nil {
				code = "ConditionalCheckFailed"
			}
			reasons = append(reasons, dynamotypes.CancellationReason{Code: aws.String(code)})
		}
		return nil, &dynamotypes.TransactionCanceledException{Message: aws.String("Transaction cancelled"), CancellationReasons: reasons}
	}
	return &dynamodb.TransactWriteItemsOutput{}, nil
}

// markers returns the unknown markers put, by sort key, failing the test when one binary got more than one.
func (r *markerRecorder) markers(t *testing.T) map[string]map[string]dynamotypes.AttributeValue {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]map[string]dynamotypes.AttributeValue{}
	for _, item := range r.puts {
		pk, _ := item["PK"].(*dynamotypes.AttributeValueMemberS)
		if pk == nil || !strings.HasPrefix(pk.Value, unknownMarkerPrefix) {
			continue
		}
		if pk.Value != unknownMarkerPrefix+machineID {
			t.Errorf("unknown marker partition %q, want %q", pk.Value, unknownMarkerPrefix+machineID)
		}
		sk := item["SK"].(*dynamotypes.AttributeValueMemberS).Value
		if _, dup := out[sk]; dup {
			t.Errorf("binary %.8s… got more than one unknown marker write in one upload", sk)
		}
		out[sk] = item
	}
	return out
}

func (r *markerRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.puts = nil
}

func markerSHA(i int) string { return fmt.Sprintf("%064x", 0x5ee0000+i) }

func unknownEvent(sha, decision string, pid int, at time.Time) EventUploadEvent {
	return EventUploadEvent{FileSHA256: sha, Decision: decision, ProcessID: pid, ExecutionTime: float64(at.UnixMilli()) / 1000,
		FileName: "tool", FilePath: "/Applications/Tool.app/Contents/MacOS"}
}

func storeWith(t *testing.T, api *markerRecorder, now time.Time, events []EventUploadEvent) error {
	t.Helper()
	store := newEventStore(api, "events", nil)
	store.now = func() time.Time { return now }
	_, _, err := store.Store(context.Background(), machineID, events, nil)
	return err
}

func attrS(item map[string]dynamotypes.AttributeValue, name string) (string, bool) {
	v, ok := item[name].(*dynamotypes.AttributeValueMemberS)
	if !ok {
		return "", false
	}
	return v.Value, true
}

var markerNow = time.Date(2026, 11, 22, 23, 30, 0, 0, time.UTC)

func TestUnknownMarker_OnlyForUnknownDecisions(t *testing.T) {
	at := markerNow.Add(-time.Hour)
	events := []EventUploadEvent{
		unknownEvent(markerSHA(1), "BLOCK_UNKNOWN", 1, at),
		unknownEvent(markerSHA(2), "ALLOW_UNKNOWN", 2, at),
		unknownEvent(markerSHA(3), "BLOCK_BINARY", 3, at),
		unknownEvent(markerSHA(4), "BLOCK_TEAMID", 4, at),
		unknownEvent(markerSHA(5), "BLOCK_BINARY", 5, at),
		unknownEvent(markerSHA(5), "BLOCK_UNKNOWN", 6, at.Add(time.Second)),
		// Not stored (allowed by a rule nobody collects; older than the 90 days kept): no marker either.
		unknownEvent(markerSHA(6), "ALLOW_TEAMID", 7, at),
		unknownEvent(markerSHA(7), "BLOCK_UNKNOWN", 8, markerNow.AddDate(0, 0, -100)),
	}
	api := &markerRecorder{}
	if err := storeWith(t, api, markerNow, events); err != nil {
		t.Fatal(err)
	}
	got := api.markers(t)
	for i, want := range map[int]bool{1: true, 2: true, 3: false, 4: false, 5: true, 6: false, 7: false} {
		if _, has := got[markerSHA(i)]; has != want {
			t.Errorf("binary %d: unknown marker written %v, want %v", i, has, want)
		}
	}
}

func TestUnknownMarker_OnePerBinaryPerUpload(t *testing.T) {
	var events []EventUploadEvent
	// Five executions of one binary on two different days (two day counters), three of another.
	for i := 0; i < 5; i++ {
		events = append(events, unknownEvent(markerSHA(1), "BLOCK_UNKNOWN", 100+i, markerNow.Add(-time.Duration(i)*12*time.Hour)))
	}
	for i := 0; i < 3; i++ {
		events = append(events, unknownEvent(markerSHA(2), "ALLOW_UNKNOWN", 200+i, markerNow.Add(-time.Duration(i+1)*time.Minute)))
	}
	api := &markerRecorder{}
	if err := storeWith(t, api, markerNow, events); err != nil {
		t.Fatal(err)
	}
	got := api.markers(t) // fails on a second write for one binary
	if len(got) != 2 {
		t.Errorf("%d unknown markers written, want 2 (one per binary)", len(got))
	}
}

// Santa uploads again what it could not confirm: the events are already stored, but the marker is written again, so
// a marker write lost with the first upload is repaired, and LastDay moves to the new upload's day.
func TestUnknownMarker_WrittenAgainForAnAlreadyStoredEvent(t *testing.T) {
	events := []EventUploadEvent{unknownEvent(markerSHA(1), "BLOCK_UNKNOWN", 1, markerNow.Add(-time.Hour))}
	api := &markerRecorder{}
	if err := storeWith(t, api, markerNow, events); err != nil {
		t.Fatal(err)
	}
	api.reset()
	api.alreadyStored = true
	next := markerNow.Add(24 * time.Hour)
	if err := storeWith(t, api, next, events); err != nil {
		t.Fatal(err)
	}
	m, ok := api.markers(t)[markerSHA(1)]
	if !ok {
		t.Fatal("no unknown marker written for an upload whose event was already stored")
	}
	if day, _ := attrS(m, "LastDay"); day != "2026-11-23" {
		t.Errorf("LastDay %q after the second upload, want 2026-11-23 (the day of that upload)", day)
	}
}

// The marker carries display fields only as pkg/counterfields allows them on day counters: a value it calls unusable
// is left out whole, never cut. Each field comes from the first event of the binary that has a usable one.
func TestUnknownMarker_DisplayFieldsUseCounterfieldRules(t *testing.T) {
	at := markerNow.Add(-time.Hour)
	long := unknownEvent(markerSHA(1), "BLOCK_UNKNOWN", 1, at)
	long.FileName = strings.Repeat("f", 255)
	long.SigningIDs = "ABCDE12345:" + strings.Repeat("s", 289) // 300 bytes
	long.TeamID = "abcde12345"                                 // lower case: not a team ID

	tooLongName := unknownEvent(markerSHA(2), "BLOCK_UNKNOWN", 2, at)
	tooLongName.FileName = strings.Repeat("n", 256)
	tooLongName.SigningIDs = "ABCDE12345:com.example.tool"
	tooLongName.TeamID = "ABCDE12345"

	// The first event's signing ID is unusable; the second's is used.
	first := unknownEvent(markerSHA(3), "BLOCK_UNKNOWN", 3, at)
	first.SigningIDs = strings.Repeat("x", 256)
	second := unknownEvent(markerSHA(3), "ALLOW_UNKNOWN", 4, at.Add(time.Second))
	second.SigningIDs = "ABCDE12345:com.example.second"

	api := &markerRecorder{}
	if err := storeWith(t, api, markerNow, []EventUploadEvent{long, tooLongName, first, second}); err != nil {
		t.Fatal(err)
	}
	got := api.markers(t)

	m1 := got[markerSHA(1)]
	if name, _ := attrS(m1, "FileName"); name != long.FileName {
		t.Errorf("a 255-byte FileName was not kept whole (%d bytes)", len(name))
	}
	for _, attr := range []string{"SigningID", "TeamID"} {
		if v, ok := m1[attr]; ok {
			t.Errorf("an unusable %s was stored (%v); it must be left out, not cut", attr, v)
		}
	}

	m2 := got[markerSHA(2)]
	if v, ok := m2["FileName"]; ok {
		t.Errorf("a 256-byte FileName was stored (%T); it must be left out, not cut", v)
	}
	if v, _ := attrS(m2, "SigningID"); v != "ABCDE12345:com.example.tool" {
		t.Errorf("SigningID %q, want ABCDE12345:com.example.tool", v)
	}
	if v, _ := attrS(m2, "TeamID"); v != "ABCDE12345" {
		t.Errorf("TeamID %q, want ABCDE12345", v)
	}

	if v, _ := attrS(got[markerSHA(3)], "SigningID"); v != "ABCDE12345:com.example.second" {
		t.Errorf("SigningID %q, want the first usable one of the binary's events (ABCDE12345:com.example.second)", v)
	}
}

// LastDay and the expiry come from Rudolph's clock at the upload, never from the execution time the Mac reports.
func TestUnknownMarker_ExpiryAndLastDayUseTheUploadClock(t *testing.T) {
	executed := markerNow.AddDate(0, 0, -2)
	api := &markerRecorder{}
	if err := storeWith(t, api, markerNow, []EventUploadEvent{unknownEvent(markerSHA(1), "BLOCK_UNKNOWN", 1, executed)}); err != nil {
		t.Fatal(err)
	}
	m, ok := api.markers(t)[markerSHA(1)]
	if !ok {
		t.Fatal("no unknown marker written")
	}
	if day, _ := attrS(m, "LastDay"); day != "2026-11-22" {
		t.Errorf("LastDay %q, want 2026-11-22 (the upload's UTC date, not the execution's 2026-11-20)", day)
	}
	exp, ok := m["ExpiresAfter"].(*dynamotypes.AttributeValueMemberN)
	want := strconv.FormatInt(markerNow.Add(35*24*time.Hour).Unix(), 10)
	if !ok || exp.Value != want {
		t.Errorf("ExpiresAfter %v, want %s (upload time + 35 days)", m["ExpiresAfter"], want)
	}
}

// The marker holds nothing an index of the events table is keyed on, so it costs one write unit, and nothing else.
func TestUnknownMarker_HasNoIndexKeys(t *testing.T) {
	e := unknownEvent(markerSHA(1), "BLOCK_UNKNOWN", 1, markerNow.Add(-time.Hour))
	e.TeamID = "ABCDE12345"
	e.SigningIDs = "ABCDE12345:com.example.tool"
	e.CDHash = strings.Repeat("cd", 20)
	e.ExecutingUser = "alice"
	api := &markerRecorder{}
	if err := storeWith(t, api, markerNow, []EventUploadEvent{e}); err != nil {
		t.Fatal(err)
	}
	m, ok := api.markers(t)[markerSHA(1)]
	if !ok {
		t.Fatal("no unknown marker written")
	}
	allowed := map[string]bool{"PK": true, "SK": true, "LastDay": true, "ExpiresAfter": true, "FileName": true, "SigningID": true, "TeamID": true}
	for name := range m {
		if !allowed[name] {
			t.Errorf("the unknown marker has attribute %s; want only PK, SK, LastDay, ExpiresAfter, FileName, SigningID, TeamID", name)
		}
	}
	for _, key := range []string{"TS", "Fleet", "Decision"} {
		if _, ok := m[key]; ok {
			t.Errorf("the unknown marker has %s, an index key of the events table", key)
		}
	}
}

func TestUnknownMarker_FailureFailsTheStore(t *testing.T) {
	api := &markerRecorder{failMarkers: true}
	err := storeWith(t, api, markerNow, []EventUploadEvent{unknownEvent(markerSHA(1), "BLOCK_UNKNOWN", 1, markerNow.Add(-time.Hour))})
	if err == nil {
		t.Fatal("Store succeeded although the unknown marker write failed; the upload must fail so Santa retries")
	}
	// The failure is in the unknown marker, not anything else: the attempt was made.
	if !strings.Contains(err.Error(), "injected marker failure") {
		t.Errorf("Store failed with %v, want the marker write's error", err)
	}
}
