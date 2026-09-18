package eventupload

// ForwardedEventUploadEvent is a single event entry that appends the MachineID with the EventUploadEvent details
// and is send to Firehose
type ForwardedEventUploadEvent struct {
	MachineID string `json:"machine_id"`
	EventUploadEvent
}

// ForwardedFileAccessEvent is a file access event as forwarded to the event sink.
type ForwardedFileAccessEvent struct {
	MachineID string `json:"machine_id"`
	FileAccessEvent
}

// ForwardedAuditEvent is an audit event as forwarded to the event sink.
type ForwardedAuditEvent struct {
	MachineID string `json:"machine_id"`
	AuditEvent
}

// convertRequestEventsToUploadEvents turns everything a sensor uploaded into the records that are written to
// the event sinks. Every record carries the machine ID of the host that reported it.
func convertRequestEventsToUploadEvents(machineID string, request *EventUploadRequest) []interface{} {
	var forwardedEvents []interface{}

	for _, event := range request.Events {
		forwardedEvents = append(forwardedEvents, ForwardedEventUploadEvent{
			MachineID:        machineID,
			EventUploadEvent: event,
		})
	}

	for _, event := range request.FileAccessEvents {
		forwardedEvents = append(forwardedEvents, ForwardedFileAccessEvent{
			MachineID:       machineID,
			FileAccessEvent: event,
		})
	}

	for _, event := range request.AuditEvents {
		forwardedEvents = append(forwardedEvents, ForwardedAuditEvent{
			MachineID:  machineID,
			AuditEvent: event,
		})
	}

	return forwardedEvents
}
