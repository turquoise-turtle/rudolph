package eventsink

import (
	"context"
	"errors"
	"math/rand"
	"time"
)

// MaxAttempts is how often a sink sends a record before it gives up on it, unless the deadline comes first.
// Whole-request errors (throttling, 5xx) are additionally retried by the AWS SDK itself, within the same deadline.
const MaxAttempts = 6

// throughputWindow is the window over which Kinesis shards and Firehose streams measure their throughput limits.
// A record refused for throughput is only worth re-sending once the window has moved on.
const throughputWindow = time.Second

// minRequestTime is the least time a retry needs to be worth starting before the deadline.
const minRequestTime = 250 * time.Millisecond

// Backoff returns how long to wait before the given attempt (2, 3, ...).
//
// After a throughput error (throttled is true) it waits one throughput window plus up to half a window of jitter
// (1s to 1.5s), so the retry lands in a new window; five such waits span more than five seconds. After any other
// error it backs off exponentially from 100ms, capped at one second, with jitter.
func Backoff(attempt int, throttled bool) time.Duration {
	if attempt < 2 {
		return 0
	}
	if throttled {
		return throughputWindow + jitter(throughputWindow/2)
	}
	d := 100 * time.Millisecond << (attempt - 2)
	if d > time.Second || d <= 0 {
		d = time.Second
	}
	return d/2 + jitter(d/2)
}

func jitter(max time.Duration) time.Duration {
	return time.Duration(rand.Int63n(int64(max) + 1))
}

// ErrNoTimeLeft is returned by Wait when the caller's deadline does not leave room for another attempt.
var ErrNoTimeLeft = errors.New("deadline too close for another attempt")

// Wait sleeps for Backoff(attempt, throttled). It returns ErrNoTimeLeft without sleeping when the context's deadline
// would not leave time for a request after the wait, and the context's error if it ends during the wait.
func Wait(ctx context.Context, attempt int, throttled bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d := Backoff(attempt, throttled)
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < d+minRequestTime {
		return ErrNoTimeLeft
	}
	if d == 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// DeadlineMargin is the time kept free before the caller's deadline (the Lambda invocation's) so a handler can
// still answer when its sinks give up, instead of being killed by the Lambda timeout.
const DeadlineMargin = 1500 * time.Millisecond

// DefaultBudget bounds sink calls when the caller has no deadline (outside Lambda).
const DefaultBudget = 20 * time.Second

// WithSendDeadline returns the context sink calls should use: the caller's deadline minus DeadlineMargin, or
// DefaultBudget from now if the caller has no deadline.
func WithSendDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok {
		return context.WithDeadline(ctx, deadline.Add(-DeadlineMargin))
	}
	return context.WithTimeout(ctx, DefaultBudget)
}

// EnsureDeadline returns ctx unchanged if it has a deadline, and otherwise ctx bounded by DefaultBudget.
func EnsureDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, DefaultBudget)
}
