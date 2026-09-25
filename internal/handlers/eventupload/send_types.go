package eventupload

// Event sinks receive three different record shapes on one stream, and a field such as `decision` means
// something different on each. Every record therefore carries an event_type saying which shape it is, so a
// consumer (an Athena table, a Lambda, a search index) can tell them apart instead of guessing.
const (
	EventTypeExecution  = "execution"
	EventTypeFileAccess = "file_access"
	EventTypeAudit      = "audit"
)

// ForwardedEventUploadEvent is a single event entry that appends the MachineID with the EventUploadEvent details
// and is send to Firehose
type ForwardedEventUploadEvent struct {
	MachineID string `json:"machine_id"`
	EventType string `json:"event_type"`
	EventUploadEvent
}

// ForwardedFileAccessEvent is a file access event as forwarded to the event sink.
type ForwardedFileAccessEvent struct {
	MachineID string `json:"machine_id"`
	EventType string `json:"event_type"`
	FileAccessEvent
}

// ForwardedAuditEvent is an audit event as forwarded to the event sink.
type ForwardedAuditEvent struct {
	MachineID string `json:"machine_id"`
	EventType string `json:"event_type"`
	AuditEvent
}

// convertRequestEventsToUploadEvents turns everything a sensor uploaded into the records that are written to
// the event sinks. Every record carries the machine ID of the host that reported it and its event type.
func convertRequestEventsToUploadEvents(machineID string, request *EventUploadRequest) []interface{} {
	var forwardedEvents []interface{}

	for _, event := range request.Events {
		forwardedEvents = append(forwardedEvents, ForwardedEventUploadEvent{
			MachineID:        machineID,
			EventType:        EventTypeExecution,
			EventUploadEvent: event,
		})
	}

	for _, event := range request.FileAccessEvents {
		forwardedEvents = append(forwardedEvents, ForwardedFileAccessEvent{
			MachineID:       machineID,
			EventType:       EventTypeFileAccess,
			FileAccessEvent: event,
		})
	}

	for _, event := range request.AuditEvents {
		if event.StandaloneModeRuleCreation == nil {
			// An audit event of a kind this Rudolph does not know (a newer Santa); forwarding it would write
			// a record with nothing in it.
			continue
		}
		forwardedEvents = append(forwardedEvents, ForwardedAuditEvent{
			MachineID:  machineID,
			EventType:  EventTypeAudit,
			AuditEvent: event,
		})
	}

	return forwardedEvents
}
