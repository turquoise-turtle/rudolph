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
	"unicode/utf8"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/smithy-go"

	"github.com/airbnb/rudolph/pkg/eventsink"
	"github.com/airbnb/rudolph/pkg/model/rules"
	rudolphtypes "github.com/airbnb/rudolph/pkg/types"
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

	// transientAttempts bounds the attempts of a transaction cancelled by a conflicting write to its day counter
	// (another upload of the same binary on the same day) or by throttling.
	transientAttempts = 5

	// maxEventsPerTransaction leaves room for the day counter update in a transaction of at most 100 items. A
	// crash-looping binary fills a whole upload (batch_size may be up to 500) with events of one counter; written one
	// transaction each, one after another, they would not fit in the Lambda's deadline.
	maxEventsPerTransaction = 99

	// maxTransactionBytes keeps a transaction clear of DynamoDB's 4 MB limit, with room for the counter update and
	// the request's own overhead.
	maxTransactionBytes = 3 << 20

	maxStoredStringBytes = 4096
	maxIndexKeyBytes     = 1024
)

type eventStoreAPI interface {
	PutItem(ctx context.Context, in *dynamodb.PutItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	TransactWriteItems(ctx context.Context, in *dynamodb.TransactWriteItemsInput, optFns ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error)
}

type eventStore struct {
	api   eventStoreAPI
	table string
	now   func() time.Time

	// collected says which rules have their allowed executions stored; nil stores none.
	collected *collectedRules
}

func newEventStore(api eventStoreAPI, table string, collected *collectedRules) *eventStore {
	return &eventStore{api: api, table: table, now: time.Now, collected: collected}
}

// storedDecision reports whether execution events with this decision always go to the events table. Executions
// allowed by a rule are stored only when that rule is flagged (see allowingRule): Santa uploads them only with
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

// allowingRule returns the sort key of the rule that allowed an execution, found from its decision and its own
// fields, or "" when the decision is not an allow by a rule that can be flagged. ALLOW_SCOPE, ALLOW_PLATFORM and
// BUNDLE_BINARY have no such rule.
func allowingRule(e EventUploadEvent) string {
	var ruleType rudolphtypes.RuleType
	var identifier string
	switch e.Decision {
	case "ALLOW_BINARY":
		ruleType, identifier = rudolphtypes.RuleTypeBinary, e.FileSHA256
	case "ALLOW_CERTIFICATE":
		if len(e.SigningChain) > 0 {
			ruleType, identifier = rudolphtypes.RuleTypeCertificate, e.SigningChain[0].SHA256
		}
	case "ALLOW_TEAMID":
		ruleType, identifier = rudolphtypes.RuleTypeTeamID, e.TeamID
	case "ALLOW_SIGNINGID":
		ruleType, identifier = rudolphtypes.RuleTypeSigningID, e.SigningIDs
	case "ALLOW_CDHASH":
		ruleType, identifier = rudolphtypes.RuleTypeCDHash, e.CDHash
	}
	if identifier == "" {
		return ""
	}
	return rules.RuleSortKeyFromTypeIdentifier(identifier, ruleType)
}

type storableEvent struct {
	event   EventUploadEvent
	micros  int64
	blocked bool
	sk      string
	counter counterKey

	// rejected is set when DynamoDB refused the event as invalid; it was dropped.
	rejected bool
}

// size estimates the bytes the event's item adds to a transaction.
func (c storableEvent) size() int {
	e := c.event
	return 512 + 2*len(c.sk) + len(e.FilePath) + len(e.FileName) + len(e.ExecutingUser) + len(e.TeamID) +
		len(e.SigningIDs) + len(e.CDHash) + len(e.Decision) + len(e.FileSHA256)
}

// cut shortens s to at most max bytes without splitting a UTF-8 character.
func cut(s string, max int) string {
	if len(s) <= max {
		return s
	}
	i := max
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}

// cutFields bounds the strings stored from an event. Santa's fields have no length limit in the request, and an item
// over DynamoDB's 400 KB limit, or a transaction over 4 MB, would be refused on every retry. Decision and TeamID are
// index partition keys, which DynamoDB limits to 2048 bytes; real values are a few dozen.
func cutFields(e EventUploadEvent) EventUploadEvent {
	e.FilePath = cut(e.FilePath, maxStoredStringBytes)
	e.FileName = cut(e.FileName, maxStoredStringBytes)
	e.ExecutingUser = cut(e.ExecutingUser, maxStoredStringBytes)
	e.SigningIDs = cut(e.SigningIDs, maxStoredStringBytes)
	e.CDHash = cut(e.CDHash, maxStoredStringBytes)
	e.TeamID = cut(e.TeamID, maxIndexKeyBytes)
	e.Decision = cut(e.Decision, maxIndexKeyBytes)
	return e
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
	// The flagged rules are read only for an upload holding an execution allowed by a rule.
	lookup := s.collected.lookup(machineID)
	for _, e := range events {
		store, blocked := storedDecision(e.Decision)
		if !store {
			rule := allowingRule(e)
			if rule == "" || !lookup.collects(ctx, rule) {
				continue
			}
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
			event: cutFields(e), micros: micros, blocked: blocked, sk: sk,
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

	// Events that add to the same day counter are written in transactions of up to maxEventsPerTransaction, one after
	// another: concurrent transactions on one item cancel each other. Different counters go in parallel.
	var groupOrder []counterKey
	groups := map[counterKey][]int{}
	for i, c := range candidates {
		if _, ok := groups[c.counter]; !ok {
			groupOrder = append(groupOrder, c.counter)
		}
		groups[c.counter] = append(groups[c.counter], i)
	}

	// stored marks events that are in the table and counted, whether by this call or by an earlier upload.
	stored := make([]bool, len(candidates))
	var rejected int
	var mu sync.Mutex
	writeErr := parallel(ctx, len(groupOrder), func(ctx context.Context, g int) error {
		group := groups[groupOrder[g]]
		for _, chunk := range transactionChunks(candidates, group) {
			batches := [][]int{chunk}
			for len(batches) > 0 {
				batch := batches[0]
				batches = batches[1:]
				events := make([]storableEvent, len(batch))
				for k, i := range batch {
					events[k] = candidates[i]
				}
				invalid, err := s.storeBatch(ctx, machineID, events, eventExpiry, counterExpiry)
				if invalid > 0 {
					mu.Lock()
					rejected += invalid
					mu.Unlock()
				}
				var apiErr smithy.APIError
				switch {
				case err == nil:
					for k, i := range batch {
						stored[i] = !events[k].rejected
					}
				case errors.As(err, &apiErr) && apiErr.ErrorCode() == "ValidationException" && len(batch) > 1:
					// One event DynamoDB refuses rejects the whole transaction: write the events one by one to find it.
					for _, i := range batch {
						batches = append(batches, []int{i})
					}
				case errors.As(err, &apiErr) && apiErr.ErrorCode() == "ValidationException":
					// A retry would be rejected the same way, and failing the upload would block every later event
					// of the Mac (Santa re-sends the failed batch first).
					mu.Lock()
					rejected++
					mu.Unlock()
				default:
					return err
				}
			}
		}
		return nil
	})
	logDropped(machineID, rejected, "rejected by DynamoDB as invalid")

	// Markers are written for every stored event, new or not: a marker write lost with an earlier upload is then
	// made up for by Santa's retry. Writing a marker again only moves its expiry and LastSeenDay, the UTC date of the
	// upload, from which the ranker (internal/ranker) counts the Macs that ran a binary within a window.
	var markers []string
	marked := map[string]bool{}
	for i, c := range candidates {
		if stored[i] && !marked[c.event.FileSHA256] {
			marked[c.event.FileSHA256] = true
			markers = append(markers, c.event.FileSHA256)
		}
	}
	lastSeenDay := now.UTC().Format("2006-01-02")
	markerErr := parallel(ctx, len(markers), func(ctx context.Context, i int) error {
		_, err := s.api.PutItem(ctx, &dynamodb.PutItemInput{
			TableName: aws.String(s.table),
			Item: map[string]types.AttributeValue{
				"PK":           str(machineMarkerPrefix + markers[i]),
				"SK":           str(machineID),
				"ExpiresAfter": &types.AttributeValueMemberN{Value: counterExpiry},
				"LastSeenDay":  str(lastSeenDay),
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

// storeBatch writes events of one day counter and adds them to the counter in one transaction, so an event is never
// stored without being counted, nor counted twice, whatever happens to a response. It returns nil when every event
// is stored and counted, by this call or an earlier one.
//
// Each event's Put has the condition that the event does not exist yet. When some already exist (Santa re-sent the
// upload), DynamoDB cancels the transaction and says which Puts failed their condition; the transaction is sent again
// without them, adding only the others. A transaction cancelled by a conflicting write to the counter (another upload
// of the same binary) or by throttling is retried a bounded number of times.
//
// Each attempt carries its own client request token, derived from its contents, the expiries (which change with
// every upload, so Santa's retry never reuses a token with different parameters) and the attempt number. The SDK's
// own retries of one attempt reuse its token, so DynamoDB answers them with the first outcome instead of applying the
// transaction twice; a new attempt after a cancellation gets a new token, as DynamoDB would otherwise answer it with
// the cancellation again.
func (s *eventStore) storeBatch(ctx context.Context, machineID string, events []storableEvent, eventExpiry, counterExpiry string) (invalid int, err error) {
	pending := make([]*storableEvent, len(events))
	for k := range events {
		pending[k] = &events[k]
	}
	retries := 0
	for attempt := 1; len(pending) > 0; attempt++ {
		input := s.batchInput(machineID, pending, eventExpiry, counterExpiry, attempt)
		_, err := s.api.TransactWriteItems(ctx, input)
		if err == nil {
			return invalid, nil
		}
		var cancelled *types.TransactionCanceledException
		if !errors.As(err, &cancelled) {
			return invalid, err
		}
		reasons := cancelled.CancellationReasons
		var remaining []*storableEvent
		for k, e := range pending {
			code := ""
			if k < len(reasons) {
				code = aws.ToString(reasons[k].Code)
			}
			switch code {
			case "ConditionalCheckFailed":
				// Already stored, and so counted, by an earlier upload.
				continue
			case "ValidationError":
				// DynamoDB refuses this event (for example a key too long) and would on every retry; failing the
				// upload would block every later event of the Mac (Santa re-sends the failed batch first).
				e.rejected = true
				invalid++
				continue
			}
			remaining = append(remaining, e)
		}
		retryable := false
		for _, r := range reasons {
			switch aws.ToString(r.Code) {
			case "", "None", "ConditionalCheckFailed", "ValidationError":
			case "TransactionConflict", "ThrottlingError", "ProvisionedThroughputExceeded", "RequestLimitExceeded":
				retryable = true
			default:
				return invalid, err
			}
		}
		if len(remaining) == 0 {
			return invalid, nil
		}
		if len(remaining) == len(pending) && !retryable {
			// Cancelled for no reason this code knows how to resolve.
			return invalid, err
		}
		if retryable {
			retries++
			if retries >= transientAttempts {
				return invalid, err
			}
			if waitErr := eventsink.Wait(ctx, retries+1, false); waitErr != nil {
				return invalid, errors.Join(err, waitErr)
			}
		}
		pending = remaining
	}
	return invalid, nil
}

// transactionChunks splits the events of one day counter (indexes into candidates) into transactions within
// DynamoDB's limits of 100 items and 4 MB.
func transactionChunks(candidates []storableEvent, group []int) [][]int {
	var chunks [][]int
	var cur []int
	size := 0
	for _, i := range group {
		n := candidates[i].size()
		if len(cur) > 0 && (len(cur) >= maxEventsPerTransaction || size+n > maxTransactionBytes) {
			chunks = append(chunks, cur)
			cur, size = nil, 0
		}
		cur = append(cur, i)
		size += n
	}
	if len(cur) > 0 {
		chunks = append(chunks, cur)
	}
	return chunks
}

// batchInput builds the transaction writing these events and adding them to their day counter.
func (s *eventStore) batchInput(machineID string, events []*storableEvent, eventExpiry, counterExpiry string, attempt int) *dynamodb.TransactWriteItemsInput {
	token := sha256.New()
	fmt.Fprintf(token, "%s\x00%s\x00%s\x00%s\x00%d", s.table, machineID, eventExpiry, counterExpiry, attempt)
	items := make([]types.TransactWriteItem, 0, len(events)+1)
	for _, e := range events {
		fmt.Fprintf(token, "\x00%s", e.sk)
		items = append(items, types.TransactWriteItem{Put: &types.Put{
			TableName:           aws.String(s.table),
			Item:                eventItem(machineID, *e, eventExpiry),
			ConditionExpression: aws.String("attribute_not_exists(PK)"),
		}})
	}
	update := counterUpdate(events, counterExpiry)
	items = append(items, types.TransactWriteItem{Update: &types.Update{
		TableName:                 aws.String(s.table),
		Key:                       update.key,
		UpdateExpression:          aws.String(update.expression),
		ExpressionAttributeValues: update.values,
	}})
	return &dynamodb.TransactWriteItemsInput{
		ClientRequestToken: aws.String(hex.EncodeToString(token.Sum(nil))[:36]),
		TransactItems:      items,
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

// counterUpdate adds events of one binary on one day to its day counter. The display attributes and the expiry are
// set only if absent, so concurrent uploads never overwrite each other and the counter expires 100 days after its
// first event.
func counterUpdate(events []*storableEvent, expiry string) counterChange {
	var blocked, allowed int
	var fileName, signingID, teamID string
	for _, e := range events {
		if e.blocked {
			blocked++
		} else {
			allowed++
		}
		if fileName == "" {
			fileName = e.event.FileName
		}
		if signingID == "" {
			signingID = e.event.SigningIDs
		}
		if teamID == "" {
			teamID = e.event.TeamID
		}
	}
	values := map[string]types.AttributeValue{":exp": &types.AttributeValueMemberN{Value: expiry}}
	var add []string
	if blocked > 0 {
		add = append(add, "Blocked :blocked")
		values[":blocked"] = &types.AttributeValueMemberN{Value: strconv.Itoa(blocked)}
	}
	if allowed > 0 {
		add = append(add, "Allowed :allowed")
		values[":allowed"] = &types.AttributeValueMemberN{Value: strconv.Itoa(allowed)}
	}
	set := []string{"ExpiresAfter = if_not_exists(ExpiresAfter, :exp)"}
	for _, field := range []struct{ name, value string }{
		{"FileName", fileName}, {"SigningID", signingID}, {"TeamID", teamID},
	} {
		if field.value == "" {
			continue
		}
		placeholder := ":" + strings.ToLower(field.name)
		set = append(set, fmt.Sprintf("%s = if_not_exists(%s, %s)", field.name, field.name, placeholder))
		values[placeholder] = str(field.value)
	}
	counter := events[0].counter
	return counterChange{
		key: map[string]types.AttributeValue{
			"PK": str(dayCounterPKPrefix + counter.day),
			"SK": str(counter.sha),
		},
		expression: "ADD " + strings.Join(add, ", ") + " SET " + strings.Join(set, ", "),
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
