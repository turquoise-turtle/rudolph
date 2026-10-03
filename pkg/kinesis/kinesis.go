package kinesis

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/airbnb/rudolph/pkg/eventsink"
	"github.com/aws/aws-sdk-go/aws"
	awskinesis "github.com/aws/aws-sdk-go/service/kinesis"
)

// PutRecords limits (https://docs.aws.amazon.com/kinesis/latest/APIReference/API_PutRecords.html). Record sizes
// count the data blob and the partition key.
const (
	maxRecordsPerRequest = 500
	maxBytesPerRequest   = 5 * 1024 * 1024
	maxBytesPerRecord    = 1024 * 1024
)

// Send writes every event to the stream as one JSON record, partitioned by machine ID.
//
// Records are split into PutRecords requests within the service's count and size limits. Records Kinesis fails
// individually (FailedRecordCount) are retried, alone, with backoff, up to eventsink.MaxAttempts times. A record
// larger than Kinesis stores is not sent. Send returns nil only if every record was delivered; an
// *eventsink.UnstorableError alone if the only records not delivered can never be stored; and another error if
// records failed in a way that may succeed later.
func (c client) Send(machineID string, events KinesisEvents) error {
	// Kinesis PutRecords requires at least one record; calling it with an empty
	// Records slice returns "InvalidParameter: minimum field size of 1". Nothing
	// to send, so return success rather than issuing an invalid API call.
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
		requestFailed, err := c.putWithRetries(machineID, batch)
		if err != nil {
			failures = append(failures, err)
		}
		if requestFailed {
			// Kinesis refused a whole request even after the SDK's retries (throttled or unavailable). The send
			// fails either way, so do not hold the caller for every remaining request.
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
// was still not accepted after the last attempt. requestFailed reports that Kinesis refused a whole request.
func (c client) putWithRetries(machineID string, batch [][]byte) (requestFailed bool, err error) {
	for attempt := 1; ; attempt++ {
		eventsink.Wait(attempt)
		failed, err := c.putRecords(machineID, batch)
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
		next := make([][]byte, len(failed))
		for j, f := range failed {
			next[j] = batch[f.index]
		}
		batch = next
	}
}

type failedRecord struct {
	index int
	code  string
}

func (c client) putRecords(machineID string, batch [][]byte) ([]failedRecord, error) {
	entries := make([]*awskinesis.PutRecordsRequestEntry, len(batch))
	for i, data := range batch {
		entries[i] = &awskinesis.PutRecordsRequestEntry{
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
	response, err := c.kinesisService.PutRecords(&awskinesis.PutRecordsInput{
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
		if r.ErrorCode != nil && *r.ErrorCode != "" {
			failed = append(failed, failedRecord{index: i, code: *r.ErrorCode})
		}
	}
	return failed, nil
}
