package eventsink

// Found by the batch 4 events re-review: with a short Lambda timeout, a fixed DeadlineMargin left the sinks an
// already-expired context, so every event upload answered 500 and Santa re-sent the same batch on every sync.

import (
	"context"
	"testing"
	"time"
)

func TestReview_ShortLambdaTimeoutStillLeavesTheSinksTimeToSend(t *testing.T) {
	for _, timeout := range []time.Duration{time.Second, 1500 * time.Millisecond, 2 * time.Second, 3 * time.Second} {
		t.Run(timeout.String(), func(t *testing.T) {
			parent, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			parentDeadline, _ := parent.Deadline()

			ctx, cancelSend := WithSendDeadline(parent)
			defer cancelSend()
			if err := ctx.Err(); err != nil {
				t.Fatalf("the sink context has already ended: %v", err)
			}
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Fatal("the sink context has no deadline")
			}
			if left := time.Until(deadline); left < timeout/3 {
				t.Errorf("the sinks get %v of a %v invocation, want at least a third of it", left, timeout)
			}
			if !deadline.Before(parentDeadline) {
				t.Errorf("the sink deadline %v leaves the handler no time to answer before %v", deadline, parentDeadline)
			}
		})
	}
}

func TestReview_LongLambdaTimeoutKeepsTheFullMargin(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	parentDeadline, _ := parent.Deadline()

	ctx, cancelSend := WithSendDeadline(parent)
	defer cancelSend()
	deadline, _ := ctx.Deadline()
	if margin := parentDeadline.Sub(deadline); margin != DeadlineMargin {
		t.Errorf("margin before a 10s deadline is %v, want %v", margin, DeadlineMargin)
	}
}
