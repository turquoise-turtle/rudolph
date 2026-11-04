//go:build integration

package handlers

// Regression tests for the independent review of batch 4's OPS and CLI changes.

import (
	"bytes"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

// Review finding 5: CloudWatch metric filters count the router's "status=NNN" log lines. A client must not be able
// to fire those alarms (or forge any other log line) by putting text in the machine ID of the request path.
func TestReview_RequestPathCannotForgeStatusLogLines(t *testing.T) {
	freshServer(t)
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	forged := "x status=413 status=500\nPOST /preflight/{machine_id} machine=y status=500"
	resp, err := ApiRouter(events.APIGatewayProxyRequest{
		HTTPMethod: "POST", Resource: "/preflight/{machine_id}", Path: "/preflight/" + forged,
		PathParameters: map[string]string{"machine_id": forged},
		Headers:        map[string]string{"Content-Type": "application/json"},
		Body:           `{"serial_num":"C02TEST","hostname":"h","os_version":"15.0","os_build":"x","santa_version":"2026.8","primary_user":"u","client_mode":"MONITOR"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	logged := buf.String()
	if resp.StatusCode != 413 && strings.Contains(logged, "status=413") {
		t.Errorf("a machine ID in the request path forged a status=413 log line:\n%s", logged)
	}
	if resp.StatusCode != 500 && strings.Contains(logged, "status=500") {
		t.Errorf("a machine ID in the request path forged a status=500 log line:\n%s", logged)
	}
	if strings.Count(logged, "\n") > 2 {
		t.Errorf("a machine ID in the request path added log lines:\n%s", logged)
	}
}
