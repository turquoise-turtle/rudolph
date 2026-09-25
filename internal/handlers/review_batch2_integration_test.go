//go:build integration

package handlers

// Regression tests for the independent review of batch 2 (P1, P6, P7, P8, S2, S3).

import (
	"strings"
	"testing"

	"github.com/airbnb/rudolph/internal/testutil/fakesanta"
	"github.com/airbnb/rudolph/internal/testutil/santaproto"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-lambda-go/events"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func postPostflight(t *testing.T, transport fakesanta.Transport, machineID, fields string) *events.APIGatewayProxyResponse {
	t.Helper()
	resp, err := transport(events.APIGatewayProxyRequest{
		HTTPMethod: "POST", Resource: "/postflight/{machine_id}", Path: "/postflight/" + machineID,
		PathParameters: map[string]string{"machine_id": machineID},
		Headers:        map[string]string{"Content-Type": "application/json"},
		Body:           fields,
	})
	if err != nil {
		t.Fatalf("postflight transport error: %v", err)
	}
	return resp
}

func syncStateAttr(t *testing.T, machineID, attr string) (string, bool) {
	t.Helper()
	value, ok := getItem(t, "Machine#"+machineID, "SyncState")[attr]
	if !ok {
		return "", false
	}
	s, ok := value.(*awstypes.AttributeValueMemberS)
	if !ok {
		t.Fatalf("%s is not a string attribute: %#v", attr, value)
	}
	return s.Value, true
}

// Review finding M4: the sync type a client reports is stored in Rudolph's own canonical spelling, whichever
// spelling the client used (Santa 2026.8 sends the enum name, older clients the deprecated lowercase alias).
func TestReview_PostflightStoresCanonicalSyncType_M4(t *testing.T) {
	for _, reported := range []string{"CLEAN", "clean"} {
		t.Run(reported, func(t *testing.T) {
			transport := freshServer(t)
			host := newHost(t, machineA, transport)
			host.MustSync(t)

			resp := postPostflight(t, transport, machineA, `{"machine_id":"`+machineA+`","rules_received":3,"rules_processed":3,"sync_type":"`+reported+`"}`)
			if resp.StatusCode != 200 {
				t.Fatalf("postflight returned %d: %s", resp.StatusCode, resp.Body)
			}
			got, ok := syncStateAttr(t, machineA, "LastSyncType")
			if !ok {
				t.Fatal("LastSyncType was not stored")
			}
			if got != string(types.SyncTypeClean) {
				t.Fatalf("client reported %q, stored %q, want the canonical %q", reported, got, types.SyncTypeClean)
			}
		})
	}
}

// Review finding M4: a client's sync_type is unvalidated input; an unknown or oversized value must be rejected
// rather than written to the machine's row (a large value can push the item past DynamoDB's size limit and
// break every later sync for that host).
func TestReview_PostflightRejectsBogusSyncType_M4(t *testing.T) {
	transport := freshServer(t)
	host := newHost(t, machineA, transport)
	host.MustSync(t)
	before, _ := syncStateAttr(t, machineA, "LastSyncType")

	for name, value := range map[string]string{
		"unknown":  "TOTALLY_UNKNOWN",
		"oversize": strings.Repeat("A", 300000),
	} {
		t.Run(name, func(t *testing.T) {
			resp := postPostflight(t, transport, machineA, `{"machine_id":"`+machineA+`","rules_received":1,"rules_processed":1,"sync_type":"`+value+`"}`)
			if resp.StatusCode != 400 && resp.StatusCode != 413 {
				t.Errorf("postflight accepted a bogus sync type with status %d", resp.StatusCode)
			}
			if got, _ := syncStateAttr(t, machineA, "LastSyncType"); got != before {
				t.Errorf("LastSyncType changed to %q on a rejected postflight", got)
			}
		})
	}
}

// Review finding 5: a postflight that reports no sync type must not erase the last recorded one.
func TestReview_PostflightWithoutSyncTypeKeepsTheLastOne(t *testing.T) {
	transport := freshServer(t)
	host := newHost(t, machineA, transport)
	host.MustSync(t)
	if got, ok := syncStateAttr(t, machineA, "LastSyncType"); !ok || got == "" {
		t.Fatalf("precondition: a sync should record a sync type, got %q (present: %v)", got, ok)
	}

	resp := postPostflight(t, transport, machineA, `{"machine_id":"`+machineA+`","rules_received":0,"rules_processed":0}`)
	if resp.StatusCode != 200 {
		t.Fatalf("postflight returned %d: %s", resp.StatusCode, resp.Body)
	}
	if got, _ := syncStateAttr(t, machineA, "LastSyncType"); got == "" {
		t.Fatal("a postflight without a sync type erased the previously recorded one")
	}
}

// Review finding 6: the rules hash a client reports after applying rules is what reveals drift, so it is stored
// separately from the one reported at preflight.
func TestReview_PostflightStoresReportedRulesHash(t *testing.T) {
	transport := freshServer(t)
	host := newHost(t, machineA, transport)
	host.MustSync(t)

	const hash = "b8a1f0c2d3e4f5061728394a5b6c7d8e9f001122334455667788990011223344"
	resp := postPostflight(t, transport, machineA, `{"machine_id":"`+machineA+`","rules_received":2,"rules_processed":2,"sync_type":"NORMAL","rules_hash":"`+hash+`"}`)
	if resp.StatusCode != 200 {
		t.Fatalf("postflight returned %d: %s", resp.StatusCode, resp.Body)
	}
	item := getItem(t, "Machine#"+machineA, "SyncState")
	found := false
	for name, value := range item {
		s, ok := value.(*awstypes.AttributeValueMemberS)
		if ok && s.Value == hash && strings.Contains(strings.ToLower(name), "hash") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the rules hash reported at postflight was not stored: %v", item)
	}
}

// Guard for the same review: a host that reports a sync type keeps working through a normal sync, and the
// response Santa receives is still parseable.
func TestReview_PostflightResponseStaysValid(t *testing.T) {
	transport := freshServer(t)
	host := newHost(t, machineA, transport)
	res := host.MustSync(t)
	if got := santaproto.Canonical(t, res.Preflight); got == "" {
		t.Fatal("preflight response could not be canonicalised")
	}
}

// Raised by the batch 2 implementer: every string a client reports at preflight is stored on the machine's
// row, so an oversized value (e.g. a 300 KB hostname) could push the row past DynamoDB's 400 KB item limit and
// break every later sync for that host. Oversized values are rejected before anything is written; normal-sized
// values keep working.
func TestReview_PreflightRejectsOversizedClientStrings(t *testing.T) {
	huge := strings.Repeat("h", 300000)
	for name, apply := range map[string]func(*fakesanta.Host){
		"hostname":     func(h *fakesanta.Host) { h.Hostname = huge },
		"primary user": func(h *fakesanta.Host) { h.PrimaryUser = huge },
		"serial":       func(h *fakesanta.Host) { h.SerialNumber = huge },
		"model":        func(h *fakesanta.Host) { h.ModelID = huge },
	} {
		t.Run(name, func(t *testing.T) {
			transport := freshServer(t)
			host := newHost(t, machineA, transport)
			apply(host)

			_, err := host.Sync(t)
			stageErr, ok := err.(*fakesanta.StageError)
			if !ok || stageErr.Stage != "preflight" || (stageErr.Status != 400 && stageErr.Status != 413) {
				t.Fatalf("expected preflight to reject the oversized %s with 400 or 413, got %v", name, err)
			}
			if item := getItem(t, "Machine#"+machineA, "Current"); len(item) != 0 {
				t.Fatalf("sensor data was written despite the rejected preflight")
			}
		})
	}

	transport := freshServer(t)
	host := newHost(t, machineA, transport)
	host.Hostname = strings.Repeat("a", 60) + ".corp.example.com"
	host.MustSync(t)
}
