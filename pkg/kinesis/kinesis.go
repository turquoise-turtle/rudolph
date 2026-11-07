package kinesis

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/airbnb/rudolph/pkg/eventsink"
	"github.com/aws/aws-sdk-go-v2/aws"
	awskinesis "github.com/aws/aws-sdk-go-v2/service/kinesis"
	kinesistypes "github.com/aws/aws-sdk-go-v2/service/kinesis/types"
)

// Limits. Record sizes count the data blob and the partition key.
//
// PutRecords itself takes 500 records and 5 MiB per request and 1 MiB per record
// (https://docs.aws.amazon.com/kinesis/latest/APIReference/API_PutRecords.html). But every record of an upload has
// the same partition key (the machine ID), so they all go to one shard, which accepts 1 MiB and 1,000 records per
// second. A request above 1 MiB could never be accepted whole by that shard, so requests are capped at the shard's
// per-second write limit and a record refused for throughput is retried in the next second.
const (
	maxRecordsPerRequest = 500
	maxBytesPerRequest   = 1024 * 1024
	maxBytesPerRecord    = 1024 * 1024
)

// errorCodeThrottled is the per-record error code Kinesis uses when a shard's throughput is exceeded.
const errorCodeThrottled = "ProvisionedThroughputExceededException"

// Send is SendContext without a deadline of the caller's; it is bounded by eventsink.DefaultBudget.
func (c client) Send(machineID string, events KinesisEvents) error {
	// Kinesis PutRecords requires at least one record; calling it with an empty
	// Records slice returns "InvalidParameter: minimum field size of 1". Nothing
	// to send, so return success rather than issuing an invalid API call.
	if len(events.Items) == 0 {
		return nil
	}
	ctx, cancel := eventsink.EnsureDeadline(context.Background())
	defer cancel()
	return c.SendContext(ctx, machineID, events)
}

// SendContext writes every event to the stream as one JSON record, partitioned by machine ID.
//
// Records are split into PutRecords requests within the limits above. Records Kinesis fails individually
// (FailedRecordCount) are retried, alone, with backoff (see eventsink.Backoff), up to eventsink.MaxAttempts times
// and never past ctx's deadline. A record larger than Kinesis stores is not sent. SendContext returns nil only if
// every record was delivered; an *eventsink.UnstorableError alone if the only records not delivered can never be
// stored; and another error if records failed in a way that may succeed later.
func (c client) SendContext(ctx context.Context, machineID string, events KinesisEvents) error {
	if len(events.Items) == 0 {
		return nil
	}

	var records [][]byte
	var sizes []int
	unmarshalable := 0
	for _, event := range events.Items {
		data, err := json.Marshal(event)
		if err != nil {
			unmarshalable++
			continue
		}
		records = append(records, data)
		sizes = append(sizes, len(data))
	}

	requests, tooLarge := eventsink.Split(sizes, eventsink.Limits{
		MaxRecords:        maxRecordsPerRequest,
		MaxRequestBytes:   maxBytesPerRequest,
		MaxRecordBytes:    maxBytesPerRecord,
		PerRecordOverhead: len(machineID),
	})

	var failures []error
	for _, request := range requests {
		batch := make([][]byte, len(request))
		for j, i := range request {
			batch[j] = records[i]
		}
		requestFailed, err := c.putWithRetries(ctx, machineID, batch)
		if err != nil {
			failures = append(failures, err)
		}
		if requestFailed {
			// Kinesis refused a whole request even after the SDK's retries (throttled or unavailable), or the
			// deadline is near. The send fails either way, so do not hold the caller for the remaining requests.
			break
		}
	}

	var unstorable *eventsink.UnstorableError
	if dropped := len(tooLarge) + unmarshalable; dropped > 0 {
		unstorable = &eventsink.UnstorableError{Sink: "kinesis", Dropped: dropped, Reason: fmt.Sprintf(
			"%d above the %d byte record limit, %d not encodable as JSON", len(tooLarge), maxBytesPerRecord, unmarshalable)}
	}
	return eventsink.Outcome(failures, unstorable)
}

// putWithRetries sends one request's records, re-sending only those that failed, and returns an error if any record
// was still not accepted after the last attempt. requestFailed reports that a whole request failed or that there
// was no time left to retry.
func (c client) putWithRetries(ctx context.Context, machineID string, batch [][]byte) (requestFailed bool, err error) {
	throttled := false
	for attempt := 1; ; attempt++ {
		if err := eventsink.Wait(ctx, attempt, throttled); err != nil {
			return true, fmt.Errorf("kinesis: %d record(s) not delivered: %w", len(batch), err)
		}
		failed, err := c.putRecords(ctx, machineID, batch)
		if err != nil {
			// The SDK has already retried the request; it carries no per-record result.
			return true, fmt.Errorf("kinesis PutRecords of %d records failed: %w", len(batch), err)
		}
		if len(failed) == 0 {
			return false, nil
		}
		if attempt >= eventsink.MaxAttempts {
			return false, fmt.Errorf("kinesis did not accept %d record(s) after %d attempts (last error code %s)", len(failed), attempt, failed[0].code)
		}
		log.Printf("kinesis failed %d of %d record(s), retrying them", len(failed), len(batch))
		throttled = false
		next := make([][]byte, len(failed))
		for j, f := range failed {
			next[j] = batch[f.index]
			throttled = throttled || f.code == errorCodeThrottled
		}
		batch = next
	}
}

type failedRecord struct {
	index int
	code  string
}

func (c client) putRecords(ctx context.Context, machineID string, batch [][]byte) ([]failedRecord, error) {
	entries := make([]kinesistypes.PutRecordsRequestEntry, len(batch))
	for i, data := range batch {
		entries[i] = kinesistypes.PutRecordsRequestEntry{
			Data: data,

			// 2021-05-13 - Using the machineID as a partition key
			//   It's not a "bad idea" per se. The alternative of generating a random uuid is not strictly
			//   "better" as the machineID is just a uuid anyway (sometimes. depends on the user).
			//   The implication of this is any data records from a specific machine get put deterministically
			//   onto the same partition. If we have a partition failure, we lose all data from one set of
			//   specific machines, but data for other machines remains intact. It also means that a single
			//   machine with a lot of logs uploaded simultaneously will "burst" onto a single shard.
			PartitionKey: aws.String(machineID),
		}
	}
	response, err := c.kinesisService.PutRecords(ctx, &awskinesis.PutRecordsInput{
		Records:    entries,
		StreamName: aws.String(c.kinesisStream),
	})
	if err != nil {
		return nil, err
	}
	if response.FailedRecordCount == nil || *response.FailedRecordCount == 0 {
		return nil, nil
	}
	if len(response.Records) != len(batch) {
		return nil, fmt.Errorf("kinesis answered %d results for %d records", len(response.Records), len(batch))
	}
	var failed []failedRecord
	for i, r := range response.Records {
		if code := aws.ToString(r.ErrorCode); code != "" {
			failed = append(failed, failedRecord{index: i, code: code})
		}
	}
	if len(failed) == 0 {
		// FailedRecordCount says records failed but none is marked: which ones were stored is unknown.
		return nil, fmt.Errorf("kinesis reported %d failed record(s) without saying which", *response.FailedRecordCount)
	}
	return failed, nil
}
