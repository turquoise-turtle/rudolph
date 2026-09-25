package eventupload

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/airbnb/rudolph/internal/testutil/santaproto"
	"github.com/aws/aws-lambda-go/events"
)

// Review finding M2: three record shapes now share one sink stream, and `decision` means different things on
// two of them. Every record must say what it is, so a consumer can tell them apart.
func TestEventSink_RecordsCarryTheirType_M2(t *testing.T) {
	body := santaproto.ClientJSON(t, "EventUploadRequest", `{
		"machine_id": "`+machineID+`",
		"events": [{"file_sha256":"`+strings.Repeat("11", 32)+`","file_name":"tool","decision":"BLOCK_UNKNOWN"}],
		"file_access_events": [{"rule_name":"SSHKeys","target":"/Users/carol/.ssh/id_ed25519","decision":"FILE_ACCESS_DECISION_DENIED"}],
		"audit_events": [{"standalone_mode_rule_creation":{"decision":"ALLOW_BINARY","identifier":"`+strings.Repeat("22", 32)+`","timestamp":1789000002}}]
	}`)

	sink := &capturingFirehose{}
	h := &PostEventuploadHandler{booted: true, firehoseClient: sink, enableFirehose: true}
	resp, err := h.Handle(events.APIGatewayProxyRequest{
		HTTPMethod: "POST", Resource: "/eventupload/{machine_id}",
		PathParameters: map[string]string{"machine_id": machineID},
		Headers:        map[string]string{"Content-Type": "application/json"},
		Body:           string(body),
	})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("eventupload failed: %v %+v", err, resp)
	}
	if len(sink.records) != 3 {
		t.Fatalf("expected 3 records, got %d: %v", len(sink.records), sink.records)
	}

	types := map[string]bool{}
	for _, record := range sink.records {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(record), &decoded); err != nil {
			t.Fatal(err)
		}
		eventType, _ := decoded["event_type"].(string)
		if eventType == "" {
			t.Fatalf("record has no event_type: %s", record)
		}
		types[eventType] = true
		if decoded["machine_id"] != machineID {
			t.Errorf("record %s is missing the machine ID: %s", eventType, record)
		}
	}
	if len(types) != 3 {
		t.Fatalf("the three record shapes must be distinguishable, got types %v", types)
	}
}

// Review finding 7: an audit event Rudolph does not recognise (a newer Santa) must not become an empty record.
func TestEventSink_UnrecognisedAuditEventIsNotForwarded(t *testing.T) {
	sink := &capturingFirehose{}
	h := &PostEventuploadHandler{booted: true, firehoseClient: sink, enableFirehose: true}
	resp, err := h.Handle(events.APIGatewayProxyRequest{
		HTTPMethod: "POST", Resource: "/eventupload/{machine_id}",
		PathParameters: map[string]string{"machine_id": machineID},
		Headers:        map[string]string{"Content-Type": "application/json"},
		Body:           `{"machine_id":"` + machineID + `","audit_events":[{"someFutureEvent":{"a":1}}]}`,
	})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("eventupload failed: %v %+v", err, resp)
	}
	for _, record := range sink.records {
		var decoded map[string]any
		if err := json.Unmarshal([]byte(record), &decoded); err != nil {
			t.Fatal(err)
		}
		// A record with nothing but the machine ID and its type carries no information.
		delete(decoded, "machine_id")
		delete(decoded, "event_type")
		if len(decoded) == 0 {
			t.Fatalf("an empty record was forwarded for an unrecognised audit event: %s", record)
		}
	}
}
