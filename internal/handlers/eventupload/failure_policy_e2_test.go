package eventupload

// E2 (batch 4, decided): an event that can never be stored by the configured sink (e.g. larger than the sink's
// per-record limit, or rejected with a permanent error) is dropped with a log line that carries no payload, and the
// upload answers 200 so Santa deletes it; a transient sink failure (throttling, 5xx) answers a 5xx so Santa sends the
// batch again. Santa stops at the first failed batch and re-sends it forever (SNTSyncEventUpload.mm), so a single
// unstorable event answered with a 5xx would block every later event from that Mac.
//
// The handler is booted from its environment, like in Lambda, and talks to fakeaws through the real sink clients.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/airbnb/rudolph/internal/testutil/fakeaws"
	"github.com/airbnb/rudolph/internal/testutil/santaproto"
	"github.com/aws/aws-lambda-go/events"
)

var poisonPath = "/Users/victim/" + strings.Repeat("deeply-nested-dir/", 70_000) // about 1.2 MB, above every sink's record limit

func e2Sha(i int) string { return fmt.Sprintf("%064x", 0xe2000+i) }

// e2Request is an upload of n events; event poison (if >= 0) has a path too large for any sink.
func e2Request(t *testing.T, n, poison int) events.APIGatewayProxyRequest {
	t.Helper()
	var evs []string
	for i := 0; i < n; i++ {
		path := "/Applications/Tool.app/Contents/MacOS"
		if i == poison {
			path = poisonPath
		}
		evs = append(evs, fmt.Sprintf(`{"file_sha256":%q,"file_path":%q,"file_name":"Tool","decision":"BLOCK_UNKNOWN","executing_user":"victim"}`, e2Sha(i), path))
	}
	body := santaproto.ClientJSON(t, "EventUploadRequest", `{"machine_id":"`+machineID+`","events":[`+strings.Join(evs, ",")+`]}`)
	return events.APIGatewayProxyRequest{
		HTTPMethod:     "POST",
		Resource:       "/eventupload/{machine_id}",
		PathParameters: map[string]string{"machine_id": machineID},
		Headers:        map[string]string{"Content-Type": "application/json"},
		Body:           string(body),
	}
}

type sinkCase struct {
	name string
	env  map[string]string
	// delivered returns the file_sha256 of every event the sink accepted.
	delivered func(*fakeaws.Server) []string
}

func shasOf(t *testing.T, payloads [][]byte) []string {
	var out []string
	for _, p := range payloads {
		var e struct {
			FileSHA256 string `json:"file_sha256"`
		}
		if err := json.Unmarshal(p, &e); err != nil {
			t.Fatalf("sink record is not an event: %v", err)
		}
		out = append(out, e.FileSHA256)
	}
	return out
}

func sinkCases(t *testing.T) []sinkCase {
	return []sinkCase{
		{"firehose", map[string]string{"HANDLER": "FIREHOSE", "FIREHOSE_NAME": "events"}, func(s *fakeaws.Server) []string {
			var data [][]byte
			for _, r := range s.FirehoseRecords() {
				data = append(data, r.Data)
			}
			return shasOf(t, data)
		}},
		{"kinesis", map[string]string{"HANDLER": "KINESIS", "KINESIS_NAME": "events"}, func(s *fakeaws.Server) []string {
			var data [][]byte
			for _, r := range s.KinesisRecords() {
				data = append(data, r.Data)
			}
			return shasOf(t, data)
		}},
		{"lambda", map[string]string{"HANDLER": "NONE", "LAMBDA_NAME": "sink"}, func(s *fakeaws.Server) []string {
			var data [][]byte
			for _, inv := range s.Invocations() {
				var payload struct {
					Items []json.RawMessage `json:"items"`
				}
				if err := json.Unmarshal(inv.Payload, &payload); err != nil {
					t.Fatalf("lambda payload: %v", err)
				}
				for _, item := range payload.Items {
					data = append(data, item)
				}
			}
			return shasOf(t, data)
		}},
	}
}

func bootE2Handler(t *testing.T, aws *fakeaws.Server, env map[string]string) *PostEventuploadHandler {
	t.Helper()
	aws.Use(t)
	t.Setenv("REGION", "us-east-1")
	for _, k := range []string{"HANDLER", "FIREHOSE_NAME", "KINESIS_NAME", "LAMBDA_NAME"} {
		t.Setenv(k, env[k])
	}
	h := &PostEventuploadHandler{}
	if err := h.Boot(); err != nil {
		t.Fatal(err)
	}
	return h
}

func captureLogs(t *testing.T) *bytes.Buffer {
	var logs bytes.Buffer
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &logs
}

func TestE2_UnstorableEventIsDroppedAndTheOthersDelivered(t *testing.T) {
	for _, sink := range sinkCases(t) {
		t.Run(sink.name, func(t *testing.T) {
			aws := fakeaws.Start(t)
			h := bootE2Handler(t, aws, sink.env)
			logs := captureLogs(t)

			resp, err := h.Handle(e2Request(t, 5, 2))
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("an upload with one unstorable event must answer 200 so Santa deletes it, got %d", resp.StatusCode)
			}
			got := map[string]int{}
			for _, sha := range sink.delivered(aws) {
				got[sha]++
			}
			for _, i := range []int{0, 1, 3, 4} {
				if got[e2Sha(i)] == 0 {
					t.Errorf("event %d was not delivered to the sink", i)
				}
			}
			if got[e2Sha(2)] != 0 {
				t.Errorf("the unstorable event reached the sink")
			}
			if v := aws.Violations(); len(v) > 0 {
				t.Errorf("the handler sent requests the sink rejects: %v", v)
			}
			if !strings.Contains(strings.ToLower(logs.String()), "drop") {
				t.Errorf("dropping an event must be logged, logs: %q", logs.String())
			}
			for _, secret := range []string{"victim", e2Sha(2), "deeply-nested-dir"} {
				if strings.Contains(logs.String(), secret) {
					t.Errorf("the log line about a dropped event must not carry its payload (%q)", secret)
				}
			}
		})
	}
}

func TestE2_TransientSinkFailureAsksSantaToRetry(t *testing.T) {
	throttle := map[string]func(*fakeaws.Server){
		"firehose": func(s *fakeaws.Server) { s.FirehoseRequestFault = func(int) *fakeaws.Fault { return &fakeaws.FirehoseThrottled } },
		"kinesis":  func(s *fakeaws.Server) { s.KinesisRequestFault = func(int) *fakeaws.Fault { return &fakeaws.KinesisThrottled } },
		"lambda":   func(s *fakeaws.Server) { s.LambdaFault = func([]byte, int) *fakeaws.Fault { return &fakeaws.LambdaThrottled } },
	}
	for _, sink := range sinkCases(t) {
		t.Run(sink.name, func(t *testing.T) {
			aws := fakeaws.Start(t)
			throttle[sink.name](aws)
			h := bootE2Handler(t, aws, sink.env)
			resp, err := h.Handle(e2Request(t, 3, -1))
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode < 500 {
				t.Fatalf("a throttled sink must answer 5xx so Santa keeps and re-sends the batch, got %d", resp.StatusCode)
			}
		})
	}
}

// A record the sink keeps failing with a transient per-record error is not dropped: the batch is retried by Santa.
func TestE2_RecordsThatKeepFailingTransientlyAreNotDropped(t *testing.T) {
	aws := fakeaws.Start(t)
	aws.FirehoseRecordFault = func(data []byte, attempt int) string {
		if strings.Contains(string(data), e2Sha(1)) {
			return "ServiceUnavailableException"
		}
		return ""
	}
	h := bootE2Handler(t, aws, map[string]string{"HANDLER": "FIREHOSE", "FIREHOSE_NAME": "events"})
	resp, err := h.Handle(e2Request(t, 3, -1))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode < 500 {
		t.Fatalf("a record Firehose keeps throttling must make the upload answer 5xx, got %d", resp.StatusCode)
	}
}
