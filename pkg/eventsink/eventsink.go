// Package eventsink holds what the event sinks (Firehose, Kinesis, Lambda) share: splitting records into requests
// that stay within a service's limits, a bounded retry schedule, and the error that tells a caller some records can
// never be stored by the sink.
package eventsink

import (
	"errors"
	"fmt"
	"math/rand"
	"time"
)

// MaxAttempts is how often a sink sends a record before it gives up on it. Whole-request errors (throttling, 5xx)
// are additionally retried by the AWS SDK itself.
const MaxAttempts = 4

// Backoff returns how long to wait before the given attempt (2, 3, ...): exponential from 100ms with jitter, capped
// at 2s, so a handler invocation stays well within its timeout.
func Backoff(attempt int) time.Duration {
	if attempt < 2 {
		return 0
	}
	d := 100 * time.Millisecond << (attempt - 2)
	if d > 2*time.Second {
		d = 2 * time.Second
	}
	return d/2 + time.Duration(rand.Int63n(int64(d/2)+1))
}

// Wait sleeps for Backoff(attempt).
func Wait(attempt int) { time.Sleep(Backoff(attempt)) }

// UnstorableError reports records that the sink can never store, whatever the number of retries, for example
// because a single record is larger than the service accepts. Those records were not sent; every other record was
// delivered. It never carries record contents.
type UnstorableError struct {
	Sink    string
	Dropped int
	Reason  string
}

func (e *UnstorableError) Error() string {
	return fmt.Sprintf("%s: %d record(s) can never be stored: %s", e.Sink, e.Dropped, e.Reason)
}

// OnlyUnstorable returns the UnstorableError when err is exactly that, meaning every storable record was delivered
// and nothing failed transiently. It returns false for nil and for any other error.
func OnlyUnstorable(err error) (*UnstorableError, bool) {
	var u *UnstorableError
	if err == nil || !errors.As(err, &u) {
		return nil, false
	}
	return u, err == error(u)
}

// Limits describes how a service bounds one request.
type Limits struct {
	MaxRecords        int // records per request
	MaxRequestBytes   int // bytes per request
	MaxRecordBytes    int // bytes per record (0: only bounded by MaxRequestBytes)
	RequestOverhead   int // bytes a request costs besides its records
	PerRecordOverhead int // bytes each record costs besides its size (separators, partition keys)
}

// Split groups records, given by their sizes in bytes, into consecutive requests within limits. Records that cannot
// be sent even alone are returned in tooLarge and are in no request. Records keep their order.
func Split(sizes []int, limits Limits) (requests [][]int, tooLarge []int) {
	var cur []int
	curBytes := limits.RequestOverhead
	for i, size := range sizes {
		cost := size + limits.PerRecordOverhead
		if (limits.MaxRecordBytes > 0 && cost > limits.MaxRecordBytes) || limits.RequestOverhead+cost > limits.MaxRequestBytes {
			tooLarge = append(tooLarge, i)
			continue
		}
		if len(cur) > 0 && (len(cur) >= limits.MaxRecords || curBytes+cost > limits.MaxRequestBytes) {
			requests = append(requests, cur)
			cur, curBytes = nil, limits.RequestOverhead
		}
		cur = append(cur, i)
		curBytes += cost
	}
	if len(cur) > 0 {
		requests = append(requests, cur)
	}
	return
}

// Outcome combines the transient failures of a send with the records it could never store. It returns nil when
// both are empty, the UnstorableError alone when nothing failed transiently, and otherwise every error joined.
func Outcome(failures []error, unstorable *UnstorableError) error {
	if unstorable == nil {
		return errors.Join(failures...)
	}
	if len(failures) == 0 {
		return unstorable
	}
	return errors.Join(append(failures, unstorable)...)
}
