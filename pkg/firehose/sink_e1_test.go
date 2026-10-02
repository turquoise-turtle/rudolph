package firehose

// E1 (batch 4): the Firehose sink must deliver every record or report an error. Tested against fakeaws, which answers
// like Firehose (per-record failures with FailedPutCount, request limits), through the client's normal constructor.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/airbnb/rudolph/internal/testutil/fakeaws"
)

type event struct {
	N   int    `json:"n"`
	Pad string `json:"pad,omitempty"`
}

func items(n int, pad int) []interface{} {
	out := make([]interface{}, n)
	for i := range out {
		out[i] = event{N: i, Pad: strings.Repeat("p", pad)}
	}
	return out
}

// delivered returns how often each event number was accepted.
func delivered(t *testing.T, records []fakeaws.Record) map[int]int {
	t.Helper()
	got := map[int]int{}
	for _, r := range records {
		var e event
		if err := json.Unmarshal(r.Data, &e); err != nil {
			t.Fatalf("record is not one JSON event: %q", r.Data)
		}
		got[e.N]++
	}
	return got
}

func numberIs(data []byte, n int) bool {
	var e event
	return json.Unmarshal(data, &e) == nil && e.N == n
}

func TestE1_FirehoseRetriesOnlyTheFailedRecords(t *testing.T) {
	aws := fakeaws.Start(t)
	aws.Use(t)
	aws.FirehoseRecordFault = func(data []byte, attempt int) string {
		if numberIs(data, 1) && attempt == 1 {
			return "ServiceUnavailableException"
		}
		return ""
	}

	if err := GetClient("events", "us-east-1").Send("machine", FirehoseEvents{Items: items(3, 0)}); err != nil {
		t.Fatalf("a record that succeeds on retry must not make Send fail: %v", err)
	}
	got := delivered(t, aws.FirehoseRecords())
	for n := 0; n < 3; n++ {
		if got[n] != 1 {
			t.Errorf("event %d delivered %d times, want exactly once (only failed records are retried): %v", n, got[n], got)
		}
	}
}

// A batch that ultimately fails must fail Send, even when a later batch succeeds.
func TestE1_FirehoseReportsABatchThatKeepsFailing(t *testing.T) {
	aws := fakeaws.Start(t)
	aws.Use(t)
	aws.FirehoseRecordFault = func(data []byte, attempt int) string {
		if numberIs(data, 7) { // in the first batch of 500
			return "ServiceUnavailableException"
		}
		return ""
	}

	err := GetClient("events", "us-east-1").Send("machine", FirehoseEvents{Items: items(600, 0)})
	if err == nil {
		t.Fatal("a record that never got delivered was reported as success")
	}
	got := delivered(t, aws.FirehoseRecords())
	if got[7] != 0 || got[599] != 1 {
		t.Fatalf("unexpected deliveries: event 7 %d, event 599 %d", got[7], got[599])
	}
}

// PutRecordBatch takes at most 500 records and 4 MiB; Send must split by size as well as by count.
func TestE1_FirehoseBatchesStayWithinServiceLimits(t *testing.T) {
	aws := fakeaws.Start(t)
	aws.Use(t)

	// 600 records of about 10 KB: 6 MB in total, more than 4 MiB in the first 500.
	if err := GetClient("events", "us-east-1").Send("machine", FirehoseEvents{Items: items(600, 10_000)}); err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if v := aws.Violations(); len(v) > 0 {
		t.Fatalf("requests exceeded Firehose limits: %v", v)
	}
	got := delivered(t, aws.FirehoseRecords())
	if len(got) != 600 {
		t.Fatalf("delivered %d distinct events, want 600", len(got))
	}
	for n, c := range got {
		if c != 1 {
			t.Fatalf("event %d delivered %d times", n, c)
		}
	}
}

// Firehose cannot store a record above 1000 KiB. Send must not send it (the whole PutRecordBatch would be rejected),
// must deliver the other records, and must report the record as a permanent failure (an error).
func TestE1_FirehoseDoesNotLetAnOversizedRecordSinkTheBatch(t *testing.T) {
	aws := fakeaws.Start(t)
	aws.Use(t)
	batch := items(5, 0)
	batch[2] = event{N: 2, Pad: strings.Repeat("x", fakeaws.FirehoseMaxBytesPerRecord+10)}

	err := GetClient("events", "us-east-1").Send("machine", FirehoseEvents{Items: batch})
	if v := aws.Violations(); len(v) > 0 {
		t.Errorf("an oversized record was sent to Firehose: %v", v)
	}
	got := delivered(t, aws.FirehoseRecords())
	for _, n := range []int{0, 1, 3, 4} {
		if got[n] != 1 {
			t.Errorf("event %d delivered %d times, want 1: %v", n, got[n], fmt.Sprint(got))
		}
	}
	if err == nil {
		t.Error("the oversized record was silently lost; Send must report it")
	}
}
