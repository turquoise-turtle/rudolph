package eventupload

import (
	"log"

	"github.com/airbnb/rudolph/pkg/eventsink"
)

// dropUnstorable applies the policy for events a sink can never store (for example an event larger than the sink's
// record or payload limit): they are dropped and logged, and the upload is answered as successful. Santa stops at
// the first batch the server fails and sends it again on every sync, so failing the upload for an event that can
// never be stored would block every later event from that Mac. The log line carries counts only, never the events.
//
// Any other error (a throttled or failing sink, records that kept failing after retries) is returned unchanged so
// the upload is answered with a 5xx and Santa keeps the batch and sends it again.
func dropUnstorable(machineID string, err error) error {
	u, ok := eventsink.OnlyUnstorable(err)
	if !ok {
		return err
	}
	log.Printf("eventupload: dropped %d event(s) from machine %s that the %s sink can never store (%s); the other events were delivered",
		u.Dropped, machineID, u.Sink, u.Reason)
	return nil
}
