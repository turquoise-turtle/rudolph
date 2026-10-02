package lambda

// E1 (batch 4): the Lambda sink invokes the output function asynchronously, whose payload is limited (fakeaws uses the
// long-standing 256 KB limit, which every region accepts). A normal batch of events larger than that must still be
// delivered, split over several invocations; a single event too large for any invocation must be reported, not sent.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/airbnb/rudolph/internal/testutil/fakeaws"
)

type event struct {
	N   int    `json:"n"`
	Pad string `json:"pad,omitempty"`
}

func lambdaItems(n, pad int) []interface{} {
	out := make([]interface{}, n)
	for i := range out {
		out[i] = event{N: i, Pad: strings.Repeat("p", pad)}
	}
	return out
}

// deliveredInOrder decodes every invocation payload and returns the event numbers in delivery order.
func deliveredInOrder(t *testing.T, invocations []fakeaws.Invocation) []int {
	t.Helper()
	var got []int
	for _, inv := range invocations {
		if inv.InvocationType != "Event" || inv.FunctionName != "sink" {
			t.Fatalf("unexpected invocation %s %s", inv.FunctionName, inv.InvocationType)
		}
		var payload struct {
			Source string  `json:"source"`
			Items  []event `json:"items"`
		}
		if err := json.Unmarshal(inv.Payload, &payload); err != nil {
			t.Fatalf("payload is not a LambdaEvents object: %v", err)
		}
		if payload.Source != "test" {
			t.Fatalf("every invocation must carry the source, got %q", payload.Source)
		}
		for _, e := range payload.Items {
			got = append(got, e.N)
		}
	}
	return got
}

func TestE1_LambdaSplitsABatchAboveTheAsyncPayloadLimit(t *testing.T) {
	aws := fakeaws.Start(t)
	aws.Use(t)
	// 50 events (Santa's default batch) of about 8 KB: 400 KB in one payload.
	err := GetClient("sink", "$LATEST", "us-east-1").Send(context.Background(), "machine", LambdaEvents{Source: "test", Items: lambdaItems(50, 8000)})
	if err != nil {
		t.Fatalf("Send failed for a normal batch: %v", err)
	}
	if v := aws.Violations(); len(v) > 0 {
		t.Fatalf("an invocation exceeded the async payload limit: %v", v)
	}
	got := deliveredInOrder(t, aws.Invocations())
	if len(got) != 50 {
		t.Fatalf("delivered %d events, want 50", len(got))
	}
	for i, n := range got {
		if n != i {
			t.Fatalf("events delivered out of order or twice: %v", got)
		}
	}
}

func TestE1_LambdaReportsAnEventTooLargeForAnyInvocation(t *testing.T) {
	aws := fakeaws.Start(t)
	aws.Use(t)
	batch := lambdaItems(3, 0)
	batch[1] = event{N: 1, Pad: strings.Repeat("x", fakeaws.LambdaMaxAsyncPayloadBytes)}
	err := GetClient("sink", "$LATEST", "us-east-1").Send(context.Background(), "machine", LambdaEvents{Source: "test", Items: batch})
	if v := aws.Violations(); len(v) > 0 {
		t.Errorf("an oversized payload was sent to Lambda: %v", v)
	}
	if err == nil {
		t.Error("an event that cannot be delivered was reported as delivered")
	}
}
