package kinesis

// E1 (batch 4): the Kinesis sink must respect PutRecords limits (500 records, 5 MiB, 1 MiB per record), retry only
// the records that failed (FailedRecordCount), and report records that are never delivered. Tested against fakeaws
// through the client's normal constructor.

import (
	"encoding/json"
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

func delivered(t *testing.T, records []fakeaws.Record) map[int]int {
	t.Helper()
	got := map[int]int{}
	for _, r := range records {
		var e event
		if err := json.Unmarshal(r.Data, &e); err != nil {
			t.Fatalf("record is not one JSON event: %q", r.Data)
		}
		if r.PartitionKey == "" {
			t.Fatalf("record without partition key")
		}
		got[e.N]++
	}
	return got
}

func numberIs(data []byte, n int) bool {
	var e event
	return json.Unmarshal(data, &e) == nil && e.N == n
}

func assertEachOnce(t *testing.T, got map[int]int, n int) {
	t.Helper()
	if len(got) != n {
		t.Fatalf("delivered %d distinct events, want %d", len(got), n)
	}
	for e, c := range got {
		if c != 1 {
			t.Fatalf("event %d delivered %d times, want once", e, c)
		}
	}
}

func TestE1_KinesisSplitsRequestsByRecordCount(t *testing.T) {
	aws := fakeaws.Start(t)
	aws.Use(t)
	if err := GetClient("stream", "us-east-1").Send("machine", KinesisEvents{Items: items(1200, 0)}); err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if v := aws.Violations(); len(v) > 0 {
		t.Fatalf("requests exceeded Kinesis limits: %v", v)
	}
	assertEachOnce(t, delivered(t, aws.KinesisRecords()), 1200)
}

func TestE1_KinesisSplitsRequestsBySize(t *testing.T) {
	aws := fakeaws.Start(t)
	aws.Use(t)
	// 100 records of about 100 KB: 10 MB.
	if err := GetClient("stream", "us-east-1").Send("machine", KinesisEvents{Items: items(100, 100_000)}); err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if v := aws.Violations(); len(v) > 0 {
		t.Fatalf("requests exceeded Kinesis limits: %v", v)
	}
	assertEachOnce(t, delivered(t, aws.KinesisRecords()), 100)
}

func TestE1_KinesisRetriesOnlyTheFailedRecords(t *testing.T) {
	aws := fakeaws.Start(t)
	aws.Use(t)
	aws.KinesisRecordFault = func(data []byte, attempt int) string {
		if numberIs(data, 1) && attempt == 1 {
			return "ProvisionedThroughputExceededException"
		}
		return ""
	}
	if err := GetClient("stream", "us-east-1").Send("machine", KinesisEvents{Items: items(3, 0)}); err != nil {
		t.Fatalf("a record that succeeds on retry must not make Send fail: %v", err)
	}
	assertEachOnce(t, delivered(t, aws.KinesisRecords()), 3)
}

func TestE1_KinesisReportsARecordThatKeepsFailing(t *testing.T) {
	aws := fakeaws.Start(t)
	aws.Use(t)
	aws.KinesisRecordFault = func(data []byte, attempt int) string {
		if numberIs(data, 1) {
			return "InternalFailure"
		}
		return ""
	}
	if err := GetClient("stream", "us-east-1").Send("machine", KinesisEvents{Items: items(3, 0)}); err == nil {
		t.Fatal("a record Kinesis never accepted was reported as delivered (FailedRecordCount ignored)")
	}
}

// Kinesis cannot store a record above 1 MiB. It must not be sent (the whole PutRecords would be rejected), the others
// must be delivered, and Send must report it.
func TestE1_KinesisDoesNotLetAnOversizedRecordSinkTheBatch(t *testing.T) {
	aws := fakeaws.Start(t)
	aws.Use(t)
	batch := items(5, 0)
	batch[2] = event{N: 2, Pad: strings.Repeat("x", fakeaws.KinesisMaxBytesPerRecord)}
	err := GetClient("stream", "us-east-1").Send("machine", KinesisEvents{Items: batch})
	if v := aws.Violations(); len(v) > 0 {
		t.Errorf("an oversized record was sent to Kinesis: %v", v)
	}
	got := delivered(t, aws.KinesisRecords())
	for _, n := range []int{0, 1, 3, 4} {
		if got[n] != 1 {
			t.Errorf("event %d delivered %d times, want 1", n, got[n])
		}
	}
	if err == nil {
		t.Error("the oversized record was silently lost; Send must report it")
	}
}
