package eventupload

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/airbnb/rudolph/internal/testutil/santaproto"
	"github.com/airbnb/rudolph/pkg/firehose"
	"github.com/aws/aws-lambda-go/events"
)

type capturingFirehose struct{ records []string }

func (c *capturingFirehose) Send(_ string, e firehose.FirehoseEvents) error {
	for _, item := range e.Items {
		b, err := json.Marshal(item)
		if err != nil {
			return err
		}
		c.records = append(c.records, string(b))
	}
	return nil
}

const machineID = "AAAAAAAA-1111-4111-8111-AAAAAAAAAAAA"

// Shortcoming P6: everything Santa 2026.8 uploads must reach the event sink so it can be searched later.
func TestEventUpload_ForwardsAllSanta2026_8EventData_P6(t *testing.T) {
	body := santaproto.ClientJSON(t, "EventUploadRequest", `{
		"machine_id": "`+machineID+`",
		"events": [{
			"file_sha256": "`+strings.Repeat("ab", 32)+`",
			"file_path": "/Applications/Tool.app/Contents/MacOS",
			"file_name": "Tool",
			"executing_user": "alice",
			"execution_time": 1789000000.5,
			"decision": "BLOCK_UNKNOWN",
			"team_id": "EQHXZ8M8AV",
			"signing_id": "EQHXZ8M8AV:com.example.tool",
			"cdhash": "a1b2c3d4e5f60718293a4b5c6d7e8f9001122334",
			"entitlement_info": {"entitlements_filtered": false, "entitlements": [{"key": "com.apple.security.get-task-allow", "value": "true"}]},
			"cs_flags": 570522385,
			"signing_status": "SIGNING_STATUS_PRODUCTION",
			"secure_signing_time": 1780000000,
			"signing_time": 1780000001,
			"static_rule": true
		}, {
			"file_sha256": "`+strings.Repeat("9f", 32)+`",
			"file_path": "/usr/local/bin",
			"file_name": "swapped",
			"decision": "BLOCK_BINARY_MISMATCH"
		}],
		"file_access_events": [{
			"rule_version": "v3",
			"rule_name": "ProtectBrowserCookies",
			"target": "/Users/alice/Library/Cookies/Cookies.binarycookies",
			"access_time": 1789000001.25,
			"decision": "FILE_ACCESS_DECISION_DENIED",
			"process_chain": [{"file_path": "/tmp/stealer", "file_sha256": "`+strings.Repeat("cd", 32)+`", "pid": 4242}]
		}],
		"audit_events": [{"standalone_mode_rule_creation": {"decision": "ALLOW_BINARY", "identifier": "`+strings.Repeat("ef", 32)+`", "timestamp": 1789000002}}]
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

	all := strings.Join(sink.records, "\n")
	for _, want := range []string{
		`"entitlements"`, `com.apple.security.get-task-allow`,
		`"cs_flags":570522385`, `SIGNING_STATUS_PRODUCTION`,
		`"secure_signing_time":1780000000`, `"signing_time":1780000001`, `"static_rule":true`,
		// decision newly reported by Santa 2026.8 clients
		`BLOCK_BINARY_MISMATCH`, `swapped`,
		// file access events
		`ProtectBrowserCookies`, `/Users/alice/Library/Cookies/Cookies.binarycookies`, `FILE_ACCESS_DECISION_DENIED`, `/tmp/stealer`,
		// audit events
		`standalone_mode_rule_creation`, strings.Repeat("ef", 32),
	} {
		if !strings.Contains(all, want) {
			t.Errorf("forwarded records are missing %s", want)
		}
	}
	if len(sink.records) > 0 && !strings.Contains(sink.records[0], `"machine_id":"`+machineID+`"`) {
		t.Errorf("records must carry the machine ID: %s", sink.records[0])
	}
}
