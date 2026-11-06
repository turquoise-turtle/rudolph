package eventupload

import (
	"context"
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
)

type eventStoreAPI interface {
	PutItem(ctx context.Context, in *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	UpdateItem(ctx context.Context, in *dynamodb.UpdateItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
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
}

type counterKey struct{ day, sha string }

type dayCounter struct {
	blocked, allowed            int
	fileName, signingID, teamID string
}

// Store writes the execution events of one upload that the events table keeps. Each event is written only if it is
// not stored yet, so a retried upload stores it once; only newly stored events bump the day counters and machine
// markers. An event that can never be stored (no SHA-256 or execution time, or one DynamoDB rejects) is dropped and
// logged. Any other failure is returned, so the handler answers 5xx and Santa uploads the events again.
//
// The counters are not updated atomically with the events: if the counter updates fail after the events were stored,
// the retry finds the events already there and does not count them. That undercount is accepted in exchange for
// combining the counter updates of an upload (at most three calls per event).
func (s *eventStore) Store(ctx context.Context, machineID string, events []EventUploadEvent) error {
	var candidates []storableEvent
	seen := map[string]bool{}
	incomplete := 0
	for _, e := range events {
		store, blocked := storedDecision(e.Decision)
		if !store {
			continue
		}
		if e.FileSHA256 == "" || e.ExecutionTime <= 0 || math.IsNaN(e.ExecutionTime) || math.IsInf(e.ExecutionTime, 0) {
			incomplete++
			continue
		}
		micros := int64(math.Round(e.ExecutionTime * 1e6))
		sk := fmt.Sprintf("%020d#%s#%d", micros, e.FileSHA256, e.ProcessID)
		if seen[sk] {
			continue
		}
		seen[sk] = true
		candidates = append(candidates, storableEvent{event: e, micros: micros, blocked: blocked, sk: sk})
	}
	if incomplete > 0 {
		log.Printf("eventupload: dropped %d event(s) from machine %s that the events table can never store (no file SHA-256 or execution time)", incomplete, machineID)
	}
	if len(candidates) == 0 {
		return nil
	}

	now := s.now()
	eventExpiry := strconv.FormatInt(now.Add(eventTTL).Unix(), 10)
	counterExpiry := strconv.FormatInt(now.Add(counterTTL).Unix(), 10)

	isNew := make([]bool, len(candidates))
	var rejected int
	var mu sync.Mutex
	putErr := parallel(ctx, len(candidates), func(ctx context.Context, i int) error {
		_, err := s.api.PutItem(ctx, &dynamodb.PutItemInput{
			TableName:           aws.String(s.table),
			Item:                eventItem(machineID, candidates[i], eventExpiry),
			ConditionExpression: aws.String("attribute_not_exists(PK)"),
		})
		var conditionFailed *types.ConditionalCheckFailedException
		var apiErr smithy.APIError
		switch {
		case err == nil:
			isNew[i] = true
			return nil
		case errors.As(err, &conditionFailed):
			return nil
		case errors.As(err, &apiErr) && apiErr.ErrorCode() == "ValidationException":
			// A retry would be rejected the same way, and failing the upload would block every later event of
			// the Mac (Santa re-sends the failed batch first).
			mu.Lock()
			rejected++
			mu.Unlock()
			return nil
		default:
			return err
		}
	})
	if rejected > 0 {
		log.Printf("eventupload: dropped %d event(s) from machine %s that the events table can never store (rejected by DynamoDB as invalid)", rejected, machineID)
	}

	counters := map[counterKey]*dayCounter{}
	var counterOrder []counterKey
	markers := map[string]bool{}
	var markerOrder []string
	for i, c := range candidates {
		if !isNew[i] {
			continue
		}
		e := c.event
		key := counterKey{day: time.UnixMicro(c.micros).UTC().Format("2006-01-02"), sha: e.FileSHA256}
		counter, ok := counters[key]
		if !ok {
			counter = &dayCounter{}
			counters[key] = counter
			counterOrder = append(counterOrder, key)
		}
		if c.blocked {
			counter.blocked++
		} else {
			counter.allowed++
		}
		if counter.fileName == "" {
			counter.fileName = e.FileName
		}
		if counter.signingID == "" {
			counter.signingID = e.SigningIDs
		}
		if counter.teamID == "" {
			counter.teamID = e.TeamID
		}
		if !markers[e.FileSHA256] {
			markers[e.FileSHA256] = true
			markerOrder = append(markerOrder, e.FileSHA256)
		}
	}

	// Counters and markers are updated even when some event writes failed, so the events that were stored are
	// counted: the retry will not count them again.
	updateErr := parallel(ctx, len(counterOrder)+len(markerOrder), func(ctx context.Context, i int) error {
		if i < len(counterOrder) {
			key := counterOrder[i]
			_, err := s.api.UpdateItem(ctx, counterUpdate(s.table, key, counters[key], counterExpiry))
			return err
		}
		sha := markerOrder[i-len(counterOrder)]
		_, err := s.api.PutItem(ctx, &dynamodb.PutItemInput{
			TableName: aws.String(s.table),
			Item: map[string]types.AttributeValue{
				"PK":           str(machineMarkerPrefix + sha),
				"SK":           str(machineID),
				"ExpiresAfter": &types.AttributeValueMemberN{Value: counterExpiry},
			},
		})
		return err
	})

	if err := errors.Join(putErr, updateErr); err != nil {
		return fmt.Errorf("events table: %w", err)
	}
	return nil
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

// counterUpdate adds an upload's new events of one binary on one day to its day counter. The display attributes and
// the expiry are set by the first update of the day only, so concurrent uploads never overwrite each other.
func counterUpdate(table string, key counterKey, c *dayCounter, expiry string) *dynamodb.UpdateItemInput {
	values := map[string]types.AttributeValue{":exp": &types.AttributeValueMemberN{Value: expiry}}
	var add []string
	if c.blocked > 0 {
		add = append(add, "Blocked :blocked")
		values[":blocked"] = &types.AttributeValueMemberN{Value: strconv.Itoa(c.blocked)}
	}
	if c.allowed > 0 {
		add = append(add, "Allowed :allowed")
		values[":allowed"] = &types.AttributeValueMemberN{Value: strconv.Itoa(c.allowed)}
	}
	set := []string{"ExpiresAfter = if_not_exists(ExpiresAfter, :exp)"}
	for _, field := range []struct{ name, value string }{
		{"FileName", c.fileName}, {"SigningID", c.signingID}, {"TeamID", c.teamID},
	} {
		if field.value == "" {
			continue
		}
		placeholder := ":" + strings.ToLower(field.name)
		set = append(set, fmt.Sprintf("%s = if_not_exists(%s, %s)", field.name, field.name, placeholder))
		values[placeholder] = str(field.value)
	}
	expression := "SET " + strings.Join(set, ", ")
	if len(add) > 0 {
		expression = "ADD " + strings.Join(add, ", ") + " " + expression
	}
	return &dynamodb.UpdateItemInput{
		TableName: aws.String(table),
		Key: map[string]types.AttributeValue{
			"PK": str(dayCounterPKPrefix + key.day),
			"SK": str(key.sha),
		},
		UpdateExpression:          aws.String(expression),
		ExpressionAttributeValues: values,
	}
}

// parallel runs fn for 0..n-1 with at most storeParallelism calls at once. After the first error no new calls start;
// it returns every error that occurred.
func parallel(ctx context.Context, n int, fn func(ctx context.Context, i int) error) error {
	if n == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
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
					cancel()
				}
			}
		}()
	}
	fed := 0
feed:
	for ; fed < n; fed++ {
		select {
		case work <- fed:
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
