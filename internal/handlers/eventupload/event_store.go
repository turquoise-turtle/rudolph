package eventupload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"

	"github.com/airbnb/rudolph/pkg/eventsink"
)

// The execution events table (EVENTS_DYNAMODB_NAME) is read by the separate web view: it lists events by machine, by
// fleet, by decision and by team ID, and ranks the binaries blocked or allowed on a day from the day counters. Its
// layout is a contract with that web view (rudolph-web's docs/data-layer-design.md, "D+ in detail"); see
// docs/events-table.md.
const (
	eventPKPrefix       = "Exec#"
	dayCounterPKPrefix  = "BinaryDay#"
	machineMarkerPrefix = "BinarySeen#"
	eventFleet          = "Exec"

	eventTTL   = 90 * 24 * time.Hour
	counterTTL = 100 * 24 * time.Hour

	// storeParallelism bounds the DynamoDB calls in flight for one upload. A full upload is a few hundred events;
	// one at a time they would not fit in the Lambda's deadline.
	storeParallelism = 16

	// maxStoredPerUpload bounds the events stored from one upload, and so its DynamoDB calls and its time. Santa
	// uploads in batches of the preflight's batch_size (50 by default); only a misconfigured or forged client sends
	// more, and up to 6 MB of tiny events would not fit in the Lambda's deadline anyway.
	maxStoredPerUpload = 1000

	// Events dated further ahead would stay at the top of every newest-first list for 90 days (a wrong clock or a
	// forged upload); events older than the time kept would expire at once.
	maxFutureSkew = 24 * time.Hour
	maxEventAge   = eventTTL

	// conflictAttempts bounds the attempts of a transaction cancelled by a conflicting write to its day counter
	// (another upload of the same binary on the same day).
	conflictAttempts = 5
)

type eventStoreAPI interface {
	PutItem(ctx context.Context, in *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	TransactWriteItems(ctx context.Context, in *dynamodb.TransactWriteItemsInput, optFns ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error)
}

type eventStore struct {
	api   eventStoreAPI
	table string
	now   func() time.Time
}

func newEventStore(api eventStoreAPI, table string) *eventStore {
	return &eventStore{api: api, table: table, now: time.Now}
}

// storedDecision reports whether execution events with this decision go to the events table. Executions allowed by
// a rule wait for per-rule collection (R7b in rudolph-web's design): Santa uploads them only with
// EnableAllEventUpload, and storing all of them would multiply the table's cost about tenfold.
func storedDecision(decision string) (store, blocked bool) {
	switch {
	case strings.HasPrefix(decision, "BLOCK_"):
		return true, true
	case decision == "ALLOW_UNKNOWN":
		return true, false
	default:
		return false, false
	}
}

type storableEvent struct {
	event   EventUploadEvent
	micros  int64
	blocked bool
	sk      string
	counter counterKey
}

type counterKey struct{ day, sha string }

// Store writes the execution events of one upload that the events table keeps, and counts each newly stored event
// exactly once in its day counter. An event that can never be stored (no SHA-256, an execution time outside the kept
// window, one DynamoDB rejects, or beyond maxStoredPerUpload) is dropped and logged. Any other failure is returned,
// so the handler answers 5xx and Santa uploads the events again; events already stored and counted are skipped then.
func (s *eventStore) Store(ctx context.Context, machineID string, events []EventUploadEvent) error {
	now := s.now()
	earliest := float64(now.Add(-maxEventAge).Unix())
	latest := float64(now.Add(maxFutureSkew).Unix())

	var candidates []storableEvent
	seen := map[string]bool{}
	var incomplete, outside, overLimit int
	for _, e := range events {
		store, blocked := storedDecision(e.Decision)
		if !store {
			continue
		}
		if e.FileSHA256 == "" || e.ExecutionTime == 0 {
			incomplete++
			continue
		}
		// NaN fails both comparisons, so it is rejected here too.
		if !(e.ExecutionTime >= earliest && e.ExecutionTime <= latest) {
			outside++
			continue
		}
		micros := int64(math.Round(e.ExecutionTime * 1e6))
		sk := fmt.Sprintf("%020d#%s#%d", micros, e.FileSHA256, e.ProcessID)
		if seen[sk] {
			continue
		}
		if len(candidates) >= maxStoredPerUpload {
			overLimit++
			continue
		}
		seen[sk] = true
		candidates = append(candidates, storableEvent{
			event: e, micros: micros, blocked: blocked, sk: sk,
			counter: counterKey{day: time.UnixMicro(micros).UTC().Format("2006-01-02"), sha: e.FileSHA256},
		})
	}
	logDropped(machineID, incomplete, "no file SHA-256 or execution time")
	logDropped(machineID, outside, "execution time more than a day ahead or older than the 90 days kept")
	logDropped(machineID, overLimit, fmt.Sprintf("more than %d events in one upload", maxStoredPerUpload))
	if len(candidates) == 0 {
		return nil
	}

	eventExpiry := strconv.FormatInt(now.Add(eventTTL).Unix(), 10)
	counterExpiry := strconv.FormatInt(now.Add(counterTTL).Unix(), 10)

	// Events that add to the same day counter go one after another: concurrent transactions on one item cancel each
	// other. Different counters go in parallel.
	var groups [][]int
	groupOf := map[counterKey]int{}
	for i, c := range candidates {
		g, ok := groupOf[c.counter]
		if !ok {
			g = len(groups)
			groupOf[c.counter] = g
			groups = append(groups, nil)
		}
		groups[g] = append(groups[g], i)
	}

	// stored marks events that are in the table and counted, whether by this call or by an earlier upload.
	stored := make([]bool, len(candidates))
	var rejected int
	var mu sync.Mutex
	writeErr := parallel(ctx, len(groups), func(ctx context.Context, g int) error {
		for _, i := range groups[g] {
			err := s.storeEvent(ctx, machineID, candidates[i], eventExpiry, counterExpiry)
			var apiErr smithy.APIError
			switch {
			case err == nil:
				stored[i] = true
			case errors.As(err, &apiErr) && apiErr.ErrorCode() == "ValidationException":
				// A retry would be rejected the same way, and failing the upload would block every later event of
				// the Mac (Santa re-sends the failed batch first).
				mu.Lock()
				rejected++
				mu.Unlock()
			default:
				return err
			}
		}
		return nil
	})
	logDropped(machineID, rejected, "rejected by DynamoDB as invalid")

	// Markers are written for every stored event, new or not: a marker write lost with an earlier upload is then
	// made up for by Santa's retry. Writing a marker again only moves its expiry.
	var markers []string
	marked := map[string]bool{}
	for i, c := range candidates {
		if stored[i] && !marked[c.event.FileSHA256] {
			marked[c.event.FileSHA256] = true
			markers = append(markers, c.event.FileSHA256)
		}
	}
	markerErr := parallel(ctx, len(markers), func(ctx context.Context, i int) error {
		_, err := s.api.PutItem(ctx, &dynamodb.PutItemInput{
			TableName: aws.String(s.table),
			Item: map[string]types.AttributeValue{
				"PK":           str(machineMarkerPrefix + markers[i]),
				"SK":           str(machineID),
				"ExpiresAfter": &types.AttributeValueMemberN{Value: counterExpiry},
			},
		})
		return err
	})

	if err := errors.Join(writeErr, markerErr); err != nil {
		return fmt.Errorf("events table: %w", err)
	}
	return nil
}

func logDropped(machineID string, n int, reason string) {
	if n > 0 {
		log.Printf("eventupload: dropped %d event(s) from machine %s that the events table can never store (%s)", n, machineID, reason)
	}
}

// storeEvent writes one event and adds it to its day counter in one transaction, so an event is never stored without
// being counted, whatever happens to the response. It returns nil when the event is stored and counted, by this call
// or an earlier one.
//
// The client request token makes the SDK's own retries of the transaction idempotent: DynamoDB answers a repeated
// token within 10 minutes with the first outcome instead of applying it again. The token covers the expiries, which
// change with each upload, because a token reused with different parameters is refused; Santa's retry therefore runs
// a new transaction, which the event's condition cancels.
func (s *eventStore) storeEvent(ctx context.Context, machineID string, c storableEvent, eventExpiry, counterExpiry string) error {
	item := eventItem(machineID, c, eventExpiry)
	update := counterUpdate(c, counterExpiry)
	token := sha256.Sum256([]byte(s.table + "\x00" + eventPKPrefix + machineID + "\x00" + c.sk + "\x00" + eventExpiry + "\x00" + counterExpiry))
	input := &dynamodb.TransactWriteItemsInput{
		ClientRequestToken: aws.String(hex.EncodeToString(token[:])[:36]),
		TransactItems: []types.TransactWriteItem{
			{Put: &types.Put{
				TableName:           aws.String(s.table),
				Item:                item,
				ConditionExpression: aws.String("attribute_not_exists(PK)"),
			}},
			{Update: &types.Update{
				TableName:                 aws.String(s.table),
				Key:                       update.key,
				UpdateExpression:          aws.String(update.expression),
				ExpressionAttributeValues: update.values,
			}},
		},
	}
	for attempt := 1; ; attempt++ {
		_, err := s.api.TransactWriteItems(ctx, input)
		if err == nil {
			return nil
		}
		var cancelled *types.TransactionCanceledException
		if !errors.As(err, &cancelled) {
			return err
		}
		reasons := cancelled.CancellationReasons
		if len(reasons) > 0 && aws.ToString(reasons[0].Code) == "ConditionalCheckFailed" {
			// Already stored, and so counted, by an earlier upload.
			return nil
		}
		conflict := false
		for _, r := range reasons {
			if aws.ToString(r.Code) == "TransactionConflict" {
				conflict = true
			}
		}
		if !conflict || attempt >= conflictAttempts {
			return err
		}
		if waitErr := eventsink.Wait(ctx, attempt+1, false); waitErr != nil {
			return errors.Join(err, waitErr)
		}
	}
}

func str(v string) types.AttributeValue { return &types.AttributeValueMemberS{Value: v} }

// eventItem builds the event's item. Empty attributes are left out: TeamID, Decision and Fleet are index keys, and
// DynamoDB rejects an empty string there; the web view treats a missing attribute as empty.
func eventItem(machineID string, c storableEvent, expiry string) map[string]types.AttributeValue {
	e := c.event
	ts := fmt.Sprintf("%020d", c.micros)
	item := map[string]types.AttributeValue{
		"PK":           str(eventPKPrefix + machineID),
		"SK":           str(c.sk),
		"MachineID":    str(machineID),
		"TS":           str(ts),
		"Fleet":        str(eventFleet),
		"Decision":     str(e.Decision),
		"FileSHA256":   str(e.FileSHA256),
		"ExpiresAfter": &types.AttributeValueMemberN{Value: expiry},
	}
	for name, value := range map[string]string{
		"FilePath":      e.FilePath,
		"FileName":      e.FileName,
		"ExecutingUser": e.ExecutingUser,
		"TeamID":        e.TeamID,
		"SigningID":     e.SigningIDs,
		"CDHash":        e.CDHash,
	} {
		if value != "" {
			item[name] = str(value)
		}
	}
	return item
}

type counterChange struct {
	key        map[string]types.AttributeValue
	expression string
	values     map[string]types.AttributeValue
}

// counterUpdate adds one event to its day counter. The display attributes and the expiry are set only if absent, so
// concurrent uploads never overwrite each other and the counter expires 100 days after its first event.
func counterUpdate(c storableEvent, expiry string) counterChange {
	count := "Allowed"
	if c.blocked {
		count = "Blocked"
	}
	values := map[string]types.AttributeValue{
		":one": &types.AttributeValueMemberN{Value: "1"},
		":exp": &types.AttributeValueMemberN{Value: expiry},
	}
	set := []string{"ExpiresAfter = if_not_exists(ExpiresAfter, :exp)"}
	for _, field := range []struct{ name, value string }{
		{"FileName", c.event.FileName}, {"SigningID", c.event.SigningIDs}, {"TeamID", c.event.TeamID},
	} {
		if field.value == "" {
			continue
		}
		placeholder := ":" + strings.ToLower(field.name)
		set = append(set, fmt.Sprintf("%s = if_not_exists(%s, %s)", field.name, field.name, placeholder))
		values[placeholder] = str(field.value)
	}
	return counterChange{
		key: map[string]types.AttributeValue{
			"PK": str(dayCounterPKPrefix + c.counter.day),
			"SK": str(c.counter.sha),
		},
		expression: "ADD " + count + " :one SET " + strings.Join(set, ", "),
		values:     values,
	}
}

// parallel runs fn for 0..n-1 with at most storeParallelism calls at once. After the first error no new calls start,
// but calls already running finish: cancelling them would leave writes whose outcome is unknown. It returns every
// error that occurred.
func parallel(ctx context.Context, n int, fn func(ctx context.Context, i int) error) error {
	if n == 0 {
		return nil
	}
	var (
		mu     sync.Mutex
		errs   []error
		wg     sync.WaitGroup
		failed = make(chan struct{})
		once   sync.Once
	)
	work := make(chan int)
	for w := 0; w < min(storeParallelism, n); w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				if err := fn(ctx, i); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
					once.Do(func() { close(failed) })
				}
			}
		}()
	}
	fed := 0
feed:
	for ; fed < n; fed++ {
		select {
		case work <- fed:
		case <-failed:
			break feed
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()
	if len(errs) == 0 && fed < n {
		// The caller's deadline ended before every call started.
		return fmt.Errorf("%d of %d writes not started: %w", n-fed, n, ctx.Err())
	}
	return errors.Join(errs...)
}
