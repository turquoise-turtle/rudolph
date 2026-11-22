package ranker_test

// Ranker memory (owner approved, 2026-11-22). The ranker folds 90 days of day counters into one running total per
// distinct binary. A binary's total used to keep its display fields as stored, and eventupload stores them cut only
// to 4,096 bytes, so about 45 uploads of 1,000 binaries with maximum-length names took the 256 MB Lambda past its
// memory and stopped every run until the counters expired. Now:
//   - A display field the ranker could never use is dropped as it is read: a FileName or SigningID longer than 255
//     bytes (macOS file names are at most 255 bytes, real signing IDs far shorter), a TeamID that is not exactly 10
//     upper-case letters or digits. Dropped, not cut: the ranker looks global rules up by signing and team ID, and a
//     cut value could name another rule. A binary with no usable field still ranks, with that field empty.
//   - What the fold keeps per binary is compact: holding 200,000 binaries with short fields costs under 420 bytes (545 before)
//     each, and 10,000 with maximum-length fields under 16 MB in all (93 MB before).
//   - Each run logs the number of distinct binaries folded as "binaries=<n>".
// The memory checks use a fake DynamoDB client that builds each page as it is asked for, so the test holds none of
// the counters itself, and measure the heap the fold still holds once every day partition has been read (each
// partition's second, empty page is only asked for after its first page has been folded).

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/airbnb/rudolph/internal/ranker"
)

var memNow = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

type counterGen func(day, i int) (sha, fileName, signingID, teamID string)

// fakeEvents serves perDay day counters on each of the 90 day partitions, generated on demand; every other partition
// is empty. When the last day partition's closing page is requested, it records the heap still in use.
type fakeEvents struct {
	perDay int
	gen    counterGen

	mu         sync.Mutex
	closed     int
	heapAtFold uint64
	getKeys    []string
	written    []map[string]types.AttributeValue
}

func s(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }
func n(v int) types.AttributeValue    { return &types.AttributeValueMemberN{Value: strconv.Itoa(v)} }

func (f *fakeEvents) Query(_ context.Context, in *dynamodb.QueryInput, _ ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error) {
	pk := in.ExpressionAttributeValues[":pk"].(*types.AttributeValueMemberS).Value
	if !strings.HasPrefix(pk, "BinaryDay#") {
		return &dynamodb.QueryOutput{}, nil
	}
	day, err := time.Parse("2006-01-02", strings.TrimPrefix(pk, "BinaryDay#"))
	if err != nil {
		return nil, err
	}
	d := int(memNow.Truncate(24*time.Hour).Sub(day).Hours() / 24)
	if in.ExclusiveStartKey != nil {
		f.mu.Lock()
		f.closed++
		if f.closed == 90 {
			runtime.GC()
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			f.heapAtFold = ms.HeapAlloc
		}
		f.mu.Unlock()
		return &dynamodb.QueryOutput{}, nil
	}
	items := make([]map[string]types.AttributeValue, 0, f.perDay)
	for i := 0; i < f.perDay; i++ {
		sha, fileName, signingID, teamID := f.gen(d, i)
		item := map[string]types.AttributeValue{"SK": s(sha), "Blocked": n(1), "UnknownBlocked": n(1)}
		// strings.Clone: each item of a real response has its own copy of every string.
		if fileName != "" {
			item["FileName"] = s(strings.Clone(fileName))
		}
		if signingID != "" {
			item["SigningID"] = s(strings.Clone(signingID))
		}
		if teamID != "" {
			item["TeamID"] = s(strings.Clone(teamID))
		}
		items = append(items, item)
	}
	return &dynamodb.QueryOutput{Items: items, LastEvaluatedKey: map[string]types.AttributeValue{"PK": s(pk), "SK": s("end")}}, nil
}

func (f *fakeEvents) GetItem(_ context.Context, in *dynamodb.GetItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getKeys = append(f.getKeys, in.Key["SK"].(*types.AttributeValueMemberS).Value)
	return &dynamodb.GetItemOutput{}, nil
}

func (f *fakeEvents) BatchWriteItem(_ context.Context, in *dynamodb.BatchWriteItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.BatchWriteItemOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, reqs := range in.RequestItems {
		for _, r := range reqs {
			if r.PutRequest != nil {
				f.written = append(f.written, r.PutRequest.Item)
			}
		}
	}
	return &dynamodb.BatchWriteItemOutput{}, nil
}

func (f *fakeEvents) PutItem(context.Context, *dynamodb.PutItemInput, ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	return &dynamodb.PutItemOutput{}, nil
}

func shaOf(day, i int) string { return fmt.Sprintf("%016x%048x", day, i) }

// runFake runs the ranker over the fake and returns the heap the fold held above what was in use before the run.
func runFake(t *testing.T, f *fakeEvents, storeTable string) uint64 {
	t.Helper()
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	if err := ranker.Run(context.Background(), ranker.Options{Client: f, Table: "events", StoreTable: storeTable, Now: func() time.Time { return memNow }}); err != nil {
		t.Fatal(err)
	}
	if f.closed < 90 {
		t.Fatalf("only %d day partitions were read to their end", f.closed)
	}
	if f.heapAtFold < before.HeapAlloc {
		return 0
	}
	return f.heapAtFold - before.HeapAlloc
}

func TestRankerMemory_ManyBinariesStayCompact(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates a few hundred MB")
	}
	const perDay = 2223 // about 200,000 binaries over 90 days
	f := &fakeEvents{perDay: perDay, gen: func(day, i int) (string, string, string, string) {
		return shaOf(day, i), "tool", "EQHXZ8M8AV:com.example.tool", "EQHXZ8M8AV"
	}}
	held := runFake(t, f, "")
	if per := held / (perDay * 90); per >= 420 {
		t.Errorf("the fold held %d MB for %d binaries, %d bytes each; want under 420 bytes each", held>>20, perDay*90, per)
	}
}

func TestRankerMemory_MaximumLengthFieldsAreNotHeld(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates a few hundred MB")
	}
	long := strings.Repeat("x", 4096)
	const perDay = 112 // about 10,000 binaries
	f := &fakeEvents{perDay: perDay, gen: func(day, i int) (string, string, string, string) {
		return shaOf(day, i), long, "EQHXZ8M8AV:" + long[:4085], long[:1024]
	}}
	if held := runFake(t, f, ""); held >= 16<<20 {
		t.Errorf("the fold held %d MB for %d binaries with maximum-length fields; want under 16 MB", held>>20, perDay*90)
	}
}

func TestRankerMemory_UnusableFieldsAreDroppedNotCut(t *testing.T) {
	name256 := strings.Repeat("n", 256)
	signing256 := "EQHXZ8M8AV:" + strings.Repeat("s", 245)
	f := &fakeEvents{perDay: 6, gen: func(day, i int) (string, string, string, string) {
		if day != 0 {
			return shaOf(day, i), "", "", ""
		}
		switch i {
		case 0: // all usable, at the limits
			return shaOf(0, 0), strings.Repeat("n", 255), "EQHXZ8M8AV:" + strings.Repeat("s", 244), "EQHXZ8M8AV"
		case 1:
			return shaOf(0, 1), name256, signing256, "EQHXZ8M8A" // 9 characters
		case 2:
			return shaOf(0, 2), "tool", "ABCDEFGHIJ:com.example.x", "eqhxz8m8av" // lower case
		case 3:
			return shaOf(0, 3), "tool", "", "EQHXZ8M8AVX" // 11 characters
		case 4:
			return shaOf(0, 4), "tool", "", "EQHXZ8M8-V" // not a letter or digit
		default:
			return shaOf(0, 5), "", "", ""
		}
	}}
	runFake(t, f, "store")

	rows := map[string]map[string]types.AttributeValue{}
	for _, item := range f.written {
		if sk, ok := item["FileSHA256"].(*types.AttributeValueMemberS); ok {
			rows[sk.Value] = item
		}
	}
	attr := func(sha, name string) string {
		row := rows[sha]
		if row == nil {
			t.Fatalf("binary %s is in no list; a binary without usable fields must still rank", sha)
		}
		v, _ := row[name].(*types.AttributeValueMemberS)
		if v == nil {
			return ""
		}
		return v.Value
	}
	for _, c := range []struct {
		sha, name, want string
	}{
		{shaOf(0, 0), "FileName", strings.Repeat("n", 255)},
		{shaOf(0, 0), "SigningID", "EQHXZ8M8AV:" + strings.Repeat("s", 244)},
		{shaOf(0, 0), "TeamID", "EQHXZ8M8AV"},
		{shaOf(0, 1), "FileName", ""},
		{shaOf(0, 1), "SigningID", ""},
		{shaOf(0, 1), "TeamID", ""},
		{shaOf(0, 2), "FileName", "tool"},
		{shaOf(0, 2), "SigningID", "ABCDEFGHIJ:com.example.x"},
		{shaOf(0, 2), "TeamID", ""},
		{shaOf(0, 3), "TeamID", ""},
		{shaOf(0, 4), "TeamID", ""},
	} {
		if got := attr(c.sha, c.name); got != c.want {
			t.Errorf("%s %s = %.40q (%d bytes), want %.40q", c.sha[:20], c.name, got, len(got), c.want)
		}
	}
	for _, key := range f.getKeys {
		for _, bad := range []string{"SigningID#" + signing256, "TeamID#EQHXZ8M8A", "TeamID#eqhxz8m8av", "TeamID#EQHXZ8M8AVX", "TeamID#EQHXZ8M8-V"} {
			if key == bad {
				t.Errorf("a global rule was looked up with an unusable value: %.60q", key)
			}
		}
	}
}

func TestRankerMemory_RunLogsTheBinaryCount(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	f := &fakeEvents{perDay: 3, gen: func(day, i int) (string, string, string, string) { return shaOf(day%2, i), "tool", "", "" }}
	runFake(t, f, "")
	m := regexp.MustCompile(`binaries=(\d+)`).FindStringSubmatch(buf.String())
	if m == nil || m[1] != "6" {
		t.Errorf("the run did not log binaries=6 (3 binaries on even days, 3 on odd days):\n%s", buf.String())
	}
}
