package firehose

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/airbnb/rudolph/pkg/eventsink"
	"github.com/aws/aws-sdk-go/aws"
	awsfirehose "github.com/aws/aws-sdk-go/service/firehose"
)

// PutRecordBatch limits (https://docs.aws.amazon.com/firehose/latest/dev/limits.html).
const (
	maxRecordsPerRequest = 500
	maxBytesPerRequest   = 4 * 1024 * 1024
	maxBytesPerRecord    = 1000 * 1024
)

type FirehoseEvents struct {
	Items []interface{}
}

// Send writes every event to the delivery stream as one newline-terminated JSON record.
//
// Records are split into PutRecordBatch requests within the service's count and size limits. Records Firehose
// fails individually (FailedPutCount) are retried, alone, with backoff, up to eventsink.MaxAttempts times. A record
// larger than Firehose stores is not sent. Send returns nil only if every record was delivered; an
// *eventsink.UnstorableError alone if the only records not delivered can never be stored; and another error if
// records failed in a way that may succeed later.
func (c client) Send(machineID string, events FirehoseEvents) error {
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
		data = append(data, '\n')
		records = append(records, data)
		sizes = append(sizes, len(data))
	}

	// A record above the per-record limit would make Firehose reject the whole request.
	requests, tooLarge := eventsink.Split(sizes, eventsink.Limits{
		MaxRecords:      maxRecordsPerRequest,
		MaxRequestBytes: maxBytesPerRequest,
		MaxRecordBytes:  maxBytesPerRecord,
	})

	var failures []error
	for _, request := range requests {
		batch := make([][]byte, len(request))
		for j, i := range request {
			batch[j] = records[i]
		}
		requestFailed, err := c.putWithRetries(batch)
		if err != nil {
			failures = append(failures, err)
		}
		if requestFailed {
			// Firehose refused a whole request even after the SDK's retries (throttled or unavailable). The send
			// fails either way, so do not hold the caller for every remaining request.
			break
		}
	}

	var unstorable *eventsink.UnstorableError
	if dropped := len(tooLarge) + unmarshalable; dropped > 0 {
		unstorable = &eventsink.UnstorableError{Sink: "firehose", Dropped: dropped, Reason: fmt.Sprintf(
			"%d above the %d byte record limit, %d not encodable as JSON", len(tooLarge), maxBytesPerRecord, unmarshalable)}
	}
	return eventsink.Outcome(failures, unstorable)
}

// putWithRetries sends one request's records, re-sending only those that failed, and returns an error if any record
// was still not accepted after the last attempt. requestFailed reports that Firehose refused a whole request.
func (c client) putWithRetries(batch [][]byte) (requestFailed bool, err error) {
	for attempt := 1; ; attempt++ {
		eventsink.Wait(attempt)
		failed, err := c.putRecordBatch(batch)
		if err != nil {
			// The SDK has already retried the request; it carries no per-record result.
			return true, fmt.Errorf("firehose PutRecordBatch of %d records failed: %w", len(batch), err)
		}
		if len(failed) == 0 {
			return false, nil
		}
		if attempt >= eventsink.MaxAttempts {
			return false, fmt.Errorf("firehose did not accept %d record(s) after %d attempts (last error code %s)", len(failed), attempt, failed[0].code)
		}
		log.Printf("firehose failed %d of %d record(s), retrying them", len(failed), len(batch))
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

func (c client) putRecordBatch(batch [][]byte) ([]failedRecord, error) {
	records := make([]*awsfirehose.Record, len(batch))
	for i, data := range batch {
		records[i] = &awsfirehose.Record{Data: data}
	}
	response, err := c.firehoseService.PutRecordBatch(&awsfirehose.PutRecordBatchInput{
		DeliveryStreamName: aws.String(c.firehoseStreamName),
		Records:            records,
	})
	if err != nil {
		return nil, err
	}
	if response.FailedPutCount == nil || *response.FailedPutCount == 0 {
		return nil, nil
	}
	if len(response.RequestResponses) != len(batch) {
		return nil, fmt.Errorf("firehose answered %d results for %d records", len(response.RequestResponses), len(batch))
	}
	var failed []failedRecord
	for i, r := range response.RequestResponses {
		if r.ErrorCode != nil && *r.ErrorCode != "" {
			failed = append(failed, failedRecord{index: i, code: *r.ErrorCode})
		}
	}
	return failed, nil
}
