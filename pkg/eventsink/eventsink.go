// Package eventsink holds what the event sinks (Firehose, Kinesis, Lambda) share: splitting records into requests
// that stay within a service's limits, a retry schedule bounded by attempts and by the caller's deadline, and the
// error that tells a caller some records can never be stored by the sink.
package eventsink

import (
	"errors"
	"fmt"
)

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
