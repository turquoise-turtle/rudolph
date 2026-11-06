// Package ranker builds the web view's lists of the most blocked and most allowed binaries (R8 of rudolph-web's
// docs/data-layer-design.md) from the events table's day counters and machine markers, so that a page view reads one
// pointer and one snapshot instead of up to 90 days of counters. It runs hourly as its own Lambda (cmd/ranker).
// Layout and cost: docs/events-table.md.
package ranker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	// Written by the event writer (internal/handlers/eventupload).
	dayCounterPKPrefix  = "BinaryDay#"
	machineMarkerPrefix = "BinarySeen#"

	snapshotPKPrefix = "TopSnapshot#"
	pointerPKPrefix  = "Top#"
	pointerSK        = "Current"

	// MaxRows bounds each list.
	MaxRows = 500

	// MachineRows bounds the rows of each list that get Machines: counting a binary's Macs reads one marker per Mac,
	// about 6 MB for a binary on 100,000 Macs.
	MachineRows = 100

	// Kept well past the next run, so a ranker that stops for a while (failing, or the schedule disabled) never leaves
	// a pointer naming deleted rows; readers show GeneratedAt. DynamoDB's TTL then deletes the rows for free.
	snapshotTTL = 8 * 24 * time.Hour

	dateLayout = "2006-01-02"

	// parallelism bounds the DynamoDB calls in flight. Up to ~4,000 marker queries one at a time would take most of
	// a minute.
	parallelism = 16

	// writeAttempts bounds the BatchWriteItem calls for one batch whose items DynamoDB keeps returning unprocessed
	// (throttling); the run then fails and the pointer stays on the previous snapshot.
	writeAttempts = 8
)

// Windows are the lengths of the lists, in days ending today (UTC).
var Windows = []int{1, 7, 30, 90}

// Kinds are the counter attributes ranked.
var Kinds = []string{"Blocked", "Allowed"}

// Client is the part of *dynamodb.Client the ranker uses. It never scans.
type Client interface {
	Query(ctx context.Context, in *dynamodb.QueryInput, optFns ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
	BatchWriteItem(ctx context.Context, in *dynamodb.BatchWriteItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.BatchWriteItemOutput, error)
	PutItem(ctx context.Context, in *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
}

type Options struct {
	Client Client
	// Table is the events table (EVENTS_DYNAMODB_NAME).
	Table string
	// Now defaults to time.Now.
	Now func() time.Time
}

type counter struct {
	SK                          string
	Blocked, Allowed            int64
	FileName, SigningID, TeamID string
}

// field keeps a display attribute from the newest day that has it.
type field struct {
	value string
	day   int // days before today
}

func (f *field) offer(value string, day int) {
	if value != "" && (f.value == "" || day < f.day) {
		f.value, f.day = value, day
	}
}

// binary holds one binary's totals for every window, folded in as the day partitions are read, so a run never holds
// 90 days of counter rows.
type binary struct {
	counts                      [2][]int64 // by kind index, then window index
	fileName, signingID, teamID field
}

type entry struct {
	sha                         string
	count                       int64
	fileName, signingID, teamID string
}

type list struct {
	kind   string
	window int
	wi     int // index in Windows
	rows   []entry
}

// Run builds every list and moves each pointer to it. Each day's counters are read once and serve every window and
// kind; markers are read only for the first MachineRows binaries of each list, once per binary. Snapshot rows are
// written before any pointer, so a reader never sees a half-written snapshot; a failed run leaves the pointers on the
// previous snapshot.
func Run(ctx context.Context, opts Options) error {
	if opts.Client == nil || opts.Table == "" {
		return errors.New("ranker: Client and Table are required")
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	maxWindow := 0
	starts := make([]string, len(Windows))
	for wi, w := range Windows {
		maxWindow = max(maxWindow, w)
		starts[wi] = today.AddDate(0, 0, -(w - 1)).Format(dateLayout)
	}

	binaries := map[string]*binary{}
	var mu sync.Mutex
	err := parallel(ctx, maxWindow, func(ctx context.Context, d int) error {
		day := today.AddDate(0, 0, -d).Format(dateLayout)
		err := query(ctx, opts, dayCounterPKPrefix+day, "SK, Blocked, Allowed, FileName, SigningID, TeamID", func(item map[string]types.AttributeValue) error {
			var c counter
			if err := attributevalue.UnmarshalMap(item, &c); err != nil {
				return err
			}
			mu.Lock()
			defer mu.Unlock()
			b := binaries[c.SK]
			if b == nil {
				b = &binary{counts: [2][]int64{make([]int64, len(Windows)), make([]int64, len(Windows))}}
				binaries[c.SK] = b
			}
			for wi, w := range Windows {
				if d < w {
					b.counts[0][wi] += c.Blocked
					b.counts[1][wi] += c.Allowed
				}
			}
			b.fileName.offer(c.FileName, d)
			b.signingID.offer(c.SigningID, d)
			b.teamID.offer(c.TeamID, d)
			return nil
		})
		if err != nil {
			return fmt.Errorf("ranker: day counters of %s: %w", day, err)
		}
		return nil
	})
	if err != nil {
		return err
	}

	var lists []list
	for ki, kind := range Kinds {
		for wi, w := range Windows {
			lists = append(lists, list{kind: kind, window: w, wi: wi, rows: top(binaries, ki, wi)})
		}
	}
	binaries = nil

	var shas []string
	seen := map[string]bool{}
	for _, l := range lists {
		for _, r := range l.rows[:min(MachineRows, len(l.rows))] {
			if !seen[r.sha] {
				seen[r.sha] = true
				shas = append(shas, r.sha)
			}
		}
	}
	perSHA := make([][]int64, len(shas))
	err = parallel(ctx, len(shas), func(ctx context.Context, i int) error {
		var err error
		perSHA[i], err = countMachines(ctx, opts, shas[i], starts)
		return err
	})
	if err != nil {
		return err
	}
	machines := make(map[string][]int64, len(shas))
	for i, sha := range shas {
		machines[sha] = perSHA[i]
	}

	snapshot, err := snapshotID(now)
	if err != nil {
		return err
	}
	expires := strconv.FormatInt(now.Add(snapshotTTL).Unix(), 10)
	var requests []types.WriteRequest
	for _, l := range lists {
		pk := fmt.Sprintf("%s%s#%d#%s", snapshotPKPrefix, l.kind, l.window, snapshot)
		for i, r := range l.rows {
			item := map[string]types.AttributeValue{
				"PK":           str(pk),
				"SK":           str(fmt.Sprintf("%06d", i+1)),
				"FileSHA256":   str(r.sha),
				"Count":        num(r.count),
				"ExpiresAfter": &types.AttributeValueMemberN{Value: expires},
			}
			if i < MachineRows {
				item["Machines"] = num(machines[r.sha][l.wi])
			}
			for name, value := range map[string]string{"FileName": r.fileName, "SigningID": r.signingID, "TeamID": r.teamID} {
				if value != "" {
					item[name] = str(value)
				}
			}
			requests = append(requests, types.WriteRequest{PutRequest: &types.PutRequest{Item: item}})
		}
	}
	batches := (len(requests) + 24) / 25
	err = parallel(ctx, batches, func(ctx context.Context, b int) error {
		return writeBatch(ctx, opts, requests[b*25:min((b+1)*25, len(requests))])
	})
	if err != nil {
		return err
	}

	generatedAt := now.Format(time.RFC3339)
	for _, l := range lists {
		_, err := opts.Client.PutItem(ctx, &dynamodb.PutItemInput{
			TableName: aws.String(opts.Table),
			Item: map[string]types.AttributeValue{
				"PK":          str(fmt.Sprintf("%s%s#%d", pointerPKPrefix, l.kind, l.window)),
				"SK":          str(pointerSK),
				"Snapshot":    str(snapshot),
				"GeneratedAt": str(generatedAt),
			},
			// Runs can overlap (EventBridge may deliver twice, an operator may invoke the Lambda by hand); one that
			// started earlier must not move the pointer back to its older snapshot. IDs sort by time.
			ConditionExpression:       aws.String("attribute_not_exists(PK) OR #snapshot < :snapshot"),
			ExpressionAttributeNames:  map[string]string{"#snapshot": "Snapshot"},
			ExpressionAttributeValues: map[string]types.AttributeValue{":snapshot": str(snapshot)},
		})
		var failed *types.ConditionalCheckFailedException
		if errors.As(err, &failed) {
			log.Printf("ranker: pointer %s %d already names a newer snapshot; left as it is", l.kind, l.window)
			continue
		}
		if err != nil {
			return fmt.Errorf("ranker: pointer %s %d: %w", l.kind, l.window, err)
		}
	}
	return nil
}

// top returns the MaxRows binaries with the highest count of kind ki in window wi, ties ordered by SHA-256; zero
// counts are left out.
func top(binaries map[string]*binary, ki, wi int) []entry {
	var rows []entry
	for sha, b := range binaries {
		if n := b.counts[ki][wi]; n > 0 {
			rows = append(rows, entry{sha: sha, count: n, fileName: b.fileName.value, signingID: b.signingID.value, teamID: b.teamID.value})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].count != rows[j].count {
			return rows[i].count > rows[j].count
		}
		return rows[i].sha < rows[j].sha
	})
	if len(rows) > MaxRows {
		rows = rows[:MaxRows]
	}
	return rows
}

// countMachines counts, for each window start, the binary's machine markers whose LastSeenDay is on or after it,
// while paging: a binary run on 100,000 Macs has 100,000 markers. Markers written before the writer recorded
// LastSeenDay have none and count in no window until the Mac uploads an event of the binary again.
func countMachines(ctx context.Context, opts Options, sha string, starts []string) ([]int64, error) {
	counts := make([]int64, len(starts))
	err := query(ctx, opts, machineMarkerPrefix+sha, "LastSeenDay", func(item map[string]types.AttributeValue) error {
		if v, ok := item["LastSeenDay"].(*types.AttributeValueMemberS); ok {
			for i, start := range starts {
				if v.Value >= start {
					counts[i]++
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("ranker: machine markers of %s: %w", sha, err)
	}
	return counts, nil
}

func query(ctx context.Context, opts Options, pk, projection string, each func(map[string]types.AttributeValue) error) error {
	var start map[string]types.AttributeValue
	for {
		out, err := opts.Client.Query(ctx, &dynamodb.QueryInput{
			TableName:                 aws.String(opts.Table),
			KeyConditionExpression:    aws.String("PK = :pk"),
			ExpressionAttributeValues: map[string]types.AttributeValue{":pk": str(pk)},
			ProjectionExpression:      aws.String(projection),
			ExclusiveStartKey:         start,
		})
		if err != nil {
			return err
		}
		for _, item := range out.Items {
			if err := each(item); err != nil {
				return err
			}
		}
		if len(out.LastEvaluatedKey) == 0 {
			return nil
		}
		start = out.LastEvaluatedKey
	}
}

func writeBatch(ctx context.Context, opts Options, requests []types.WriteRequest) error {
	pending := map[string][]types.WriteRequest{opts.Table: requests}
	for attempt := 1; ; attempt++ {
		out, err := opts.Client.BatchWriteItem(ctx, &dynamodb.BatchWriteItemInput{RequestItems: pending})
		if err != nil {
			return fmt.Errorf("ranker: snapshot rows: %w", err)
		}
		if len(out.UnprocessedItems[opts.Table]) == 0 {
			return nil
		}
		if attempt >= writeAttempts {
			return fmt.Errorf("ranker: %d snapshot rows still unprocessed after %d attempts", len(out.UnprocessedItems[opts.Table]), attempt)
		}
		pending = out.UnprocessedItems
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(50<<min(attempt, 6)) * time.Millisecond):
		}
	}
}

// snapshotID sorts by time and is unique even for runs in the same instant.
func snapshotID(now time.Time) (string, error) {
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return "", err
	}
	return now.Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(suffix), nil
}

// parallel runs fn for 0..n-1 with at most parallelism calls at once and returns every error. After the first error
// no new calls start.
func parallel(ctx context.Context, n int, fn func(ctx context.Context, i int) error) error {
	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
	)
	work := make(chan int)
	for w := 0; w < min(parallelism, n); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				if err := fn(ctx, i); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
			}
		}()
	}
	for i := 0; i < n; i++ {
		mu.Lock()
		failed := len(errs) > 0
		mu.Unlock()
		if failed || ctx.Err() != nil {
			break
		}
		work <- i
	}
	close(work)
	wg.Wait()
	if len(errs) == 0 && ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.Join(errs...)
}

func str(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }

func num(v int64) types.AttributeValue {
	return &types.AttributeValueMemberN{Value: strconv.FormatInt(v, 10)}
}
