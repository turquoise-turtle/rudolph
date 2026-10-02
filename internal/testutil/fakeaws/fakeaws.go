// Package fakeaws is an in-process fake of the AWS APIs Rudolph's event sinks call: Kinesis Data Firehose
// PutRecordBatch, Kinesis Data Streams PutRecords and Lambda Invoke.
//
// It enforces the service limits that matter to Rudolph (records and bytes per request, bytes per record, the async
// invocation payload limit), answers with the response shapes and error codes of the real services, and lets a test
// fail individual records or whole requests. Production code is pointed at it only through the SDKs' configuration:
// Use sets AWS_ENDPOINT_URL_<SERVICE> (honoured by the AWS SDK for Go v2) and redirects requests to *.amazonaws.com
// made through http.DefaultTransport (used by the AWS SDK for Go v1, which has no endpoint environment variable), so
// the sink clients are created with their normal constructors.
package fakeaws

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// Service limits (see the Firehose, Kinesis Data Streams and Lambda quotas pages).
const (
	FirehoseMaxRecordsPerRequest = 500
	FirehoseMaxBytesPerRequest   = 4 * 1024 * 1024
	FirehoseMaxBytesPerRecord    = 1000 * 1024
	KinesisMaxRecordsPerRequest  = 500
	KinesisMaxBytesPerRequest    = 5 * 1024 * 1024
	// KinesisMaxBytesPerRecord counts the data blob and the partition key.
	KinesisMaxBytesPerRecord = 1024 * 1024
	// LambdaMaxAsyncPayloadBytes is the payload limit of an asynchronous (Event) invocation. AWS raised it to 1 MB in
	// late 2025; the fake keeps the long-standing 256 KB, which every region and partition accepts.
	LambdaMaxAsyncPayloadBytes = 256 * 1024
	// LambdaMaxSyncPayloadBytes is the payload limit of a synchronous (RequestResponse) invocation.
	LambdaMaxSyncPayloadBytes = 6 * 1024 * 1024
)

// Fault is an error answer to a whole request.
type Fault struct {
	Status int
	// Code is the AWS error code, e.g. "ServiceUnavailableException".
	Code    string
	Message string
}

// Common faults.
var (
	// FirehoseThrottled is Firehose's answer when the delivery stream's throughput is exceeded (retried by the SDK).
	FirehoseThrottled = Fault{Status: 503, Code: "ServiceUnavailableException", Message: "Slow down."}
	// KinesisThrottled is Kinesis's answer when a shard's throughput is exceeded (retried by the SDK).
	KinesisThrottled = Fault{Status: 400, Code: "ProvisionedThroughputExceededException", Message: "Rate exceeded for shard shardId-000000000000"}
	// LambdaThrottled is Lambda's answer when the function's concurrency is exhausted (retried by the SDK).
	LambdaThrottled = Fault{Status: 429, Code: "TooManyRequestsException", Message: "Rate Exceeded."}
	// InternalFailure is a 5xx any service may answer.
	InternalFailure = Fault{Status: 500, Code: "InternalFailure", Message: "We encountered an internal error. Please try again."}
)

// Record is one record a stream accepted.
type Record struct {
	Stream       string
	PartitionKey string // Kinesis only
	Data         []byte
}

// Invocation is one accepted Lambda invocation.
type Invocation struct {
	FunctionName   string
	Qualifier      string
	InvocationType string
	Payload        []byte
}

// Server is a running fake.
type Server struct {
	URL string

	mu sync.Mutex
	// Accepted records and invocations.
	firehoseRecords []Record
	kinesisRecords  []Record
	invocations     []Invocation
	// Requests counts requests by API ("Firehose.PutRecordBatch", "Kinesis.PutRecords", "Lambda.Invoke").
	requests map[string]int
	// Violations are requests a real service would have rejected for exceeding a limit (they are rejected here too).
	violations []string

	// FirehoseRequestFault, when set, may answer a whole PutRecordBatch request with a fault. call counts from 1.
	FirehoseRequestFault func(call int) *Fault
	// FirehoseRecordFault, when set, may fail a single record of a PutRecordBatch with an error code (e.g.
	// "ServiceUnavailableException"). attempt counts how many times the same data was sent, from 1.
	FirehoseRecordFault func(data []byte, attempt int) string
	// KinesisRequestFault and KinesisRecordFault are the same for PutRecords (record error codes are e.g.
	// "ProvisionedThroughputExceededException" or "InternalFailure").
	KinesisRequestFault func(call int) *Fault
	KinesisRecordFault  func(data []byte, attempt int) string
	// LambdaFault, when set, may answer an Invoke with a fault.
	LambdaFault func(payload []byte, call int) *Fault

	attempts map[string]int
}

// Start starts a fake that is closed at the end of the test. Call Use to point the SDKs at it.
func Start(t testing.TB) *Server {
	t.Helper()
	s := &Server{requests: map[string]int{}, attempts: map[string]int{}}
	ts := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(ts.Close)
	s.URL = ts.URL
	return s
}

// Use points the AWS SDKs at the fake for the rest of the test, and sets the credentials and region they need.
func (s *Server) Use(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{
		"AWS_ENDPOINT_URL_FIREHOSE": s.URL,
		"AWS_ENDPOINT_URL_KINESIS":  s.URL,
		"AWS_ENDPOINT_URL_LAMBDA":   s.URL,
		"AWS_ACCESS_KEY_ID":         "fake",
		"AWS_SECRET_ACCESS_KEY":     "fake",
		"AWS_REGION":                "us-east-1",
		"AWS_EC2_METADATA_DISABLED": "true",
	} {
		t.Setenv(k, v)
	}
	target, _ := url.Parse(s.URL)
	original := http.DefaultTransport
	http.DefaultTransport = redirect{to: target, next: original}
	t.Cleanup(func() { http.DefaultTransport = original })
}

// redirect sends requests for AWS service endpoints to the fake and everything else to next.
type redirect struct {
	to   *url.URL
	next http.RoundTripper
}

func (r redirect) RoundTrip(req *http.Request) (*http.Response, error) {
	if !strings.HasSuffix(req.URL.Hostname(), ".amazonaws.com") {
		return r.next.RoundTrip(req)
	}
	clone := req.Clone(req.Context())
	clone.URL.Scheme, clone.URL.Host = r.to.Scheme, r.to.Host
	clone.Host = r.to.Host
	return r.next.RoundTrip(clone)
}

// FirehoseRecords returns the records Firehose accepted, in order.
func (s *Server) FirehoseRecords() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Record(nil), s.firehoseRecords...)
}

// KinesisRecords returns the records Kinesis accepted, in order.
func (s *Server) KinesisRecords() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Record(nil), s.kinesisRecords...)
}

// Invocations returns the Lambda invocations accepted, in order.
func (s *Server) Invocations() []Invocation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Invocation(nil), s.invocations...)
}

// Requests returns how many requests each API received ("Firehose.PutRecordBatch", "Kinesis.PutRecords",
// "Lambda.Invoke").
func (s *Server) Requests() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int{}
	for k, v := range s.requests {
		out[k] = v
	}
	return out
}

// Violations returns a description of every request that exceeded a service limit.
func (s *Server) Violations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.violations...)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	target := r.Header.Get("X-Amz-Target")
	switch {
	case target == "Firehose_20150804.PutRecordBatch":
		s.putRecordBatch(w, body)
	case target == "Kinesis_20131202.PutRecords":
		s.putRecords(w, body)
	case strings.HasPrefix(r.URL.Path, "/2015-03-31/functions/") && strings.HasSuffix(r.URL.Path, "/invocations"):
		s.invoke(w, r, body)
	default:
		writeJSONError(w, Fault{Status: 400, Code: "UnknownOperationException", Message: "fakeaws does not implement " + target + " " + r.URL.Path})
	}
}

func (s *Server) violation(format string, args ...any) string {
	msg := fmt.Sprintf(format, args...)
	s.violations = append(s.violations, msg)
	return msg
}

func (s *Server) putRecordBatch(w http.ResponseWriter, body []byte) {
	var input struct {
		DeliveryStreamName string
		Records            []struct{ Data []byte }
	}
	if err := json.Unmarshal(body, &input); err != nil {
		writeJSONError(w, Fault{Status: 400, Code: "SerializationException", Message: err.Error()})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests["Firehose.PutRecordBatch"]++
	if s.FirehoseRequestFault != nil {
		if f := s.FirehoseRequestFault(s.requests["Firehose.PutRecordBatch"]); f != nil {
			writeJSONError(w, *f)
			return
		}
	}
	total := 0
	for i, rec := range input.Records {
		total += len(rec.Data)
		if len(rec.Data) > FirehoseMaxBytesPerRecord {
			writeJSONError(w, Fault{Status: 400, Code: "InvalidArgumentException", Message: s.violation(
				"Firehose PutRecordBatch: record %d is %d bytes, above the %d byte record limit", i, len(rec.Data), FirehoseMaxBytesPerRecord)})
			return
		}
	}
	if len(input.Records) == 0 || len(input.Records) > FirehoseMaxRecordsPerRequest || total > FirehoseMaxBytesPerRequest {
		writeJSONError(w, Fault{Status: 400, Code: "InvalidArgumentException", Message: s.violation(
			"Firehose PutRecordBatch: %d records, %d bytes; limits are 1-%d records and %d bytes", len(input.Records), total, FirehoseMaxRecordsPerRequest, FirehoseMaxBytesPerRequest)})
		return
	}
	type entry struct {
		RecordId     string `json:",omitempty"`
		ErrorCode    string `json:",omitempty"`
		ErrorMessage string `json:",omitempty"`
	}
	var out struct {
		FailedPutCount   int
		Encrypted        bool
		RequestResponses []entry
	}
	for _, rec := range input.Records {
		key := "firehose\x00" + string(rec.Data)
		s.attempts[key]++
		code := ""
		if s.FirehoseRecordFault != nil {
			code = s.FirehoseRecordFault(rec.Data, s.attempts[key])
		}
		if code != "" {
			out.FailedPutCount++
			out.RequestResponses = append(out.RequestResponses, entry{ErrorCode: code, ErrorMessage: "fakeaws: " + code})
			continue
		}
		s.firehoseRecords = append(s.firehoseRecords, Record{Stream: input.DeliveryStreamName, Data: rec.Data})
		out.RequestResponses = append(out.RequestResponses, entry{RecordId: fmt.Sprintf("record-%d", len(s.firehoseRecords))})
	}
	writeJSON(w, out)
}

func (s *Server) putRecords(w http.ResponseWriter, body []byte) {
	var input struct {
		StreamName string
		Records    []struct {
			Data         []byte
			PartitionKey string
		}
	}
	if err := json.Unmarshal(body, &input); err != nil {
		writeJSONError(w, Fault{Status: 400, Code: "SerializationException", Message: err.Error()})
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests["Kinesis.PutRecords"]++
	if s.KinesisRequestFault != nil {
		if f := s.KinesisRequestFault(s.requests["Kinesis.PutRecords"]); f != nil {
			writeJSONError(w, *f)
			return
		}
	}
	total := 0
	for i, rec := range input.Records {
		size := len(rec.Data) + len(rec.PartitionKey)
		total += size
		if size > KinesisMaxBytesPerRecord {
			writeJSONError(w, Fault{Status: 400, Code: "ValidationException", Message: s.violation(
				"Kinesis PutRecords: record %d is %d bytes with its partition key, above the %d byte record limit", i, size, KinesisMaxBytesPerRecord)})
			return
		}
	}
	if len(input.Records) == 0 || len(input.Records) > KinesisMaxRecordsPerRequest || total > KinesisMaxBytesPerRequest {
		writeJSONError(w, Fault{Status: 400, Code: "ValidationException", Message: s.violation(
			"Kinesis PutRecords: %d records, %d bytes; limits are 1-%d records and %d bytes", len(input.Records), total, KinesisMaxRecordsPerRequest, KinesisMaxBytesPerRequest)})
		return
	}
	type entry struct {
		SequenceNumber string `json:",omitempty"`
		ShardId        string `json:",omitempty"`
		ErrorCode      string `json:",omitempty"`
		ErrorMessage   string `json:",omitempty"`
	}
	var out struct {
		FailedRecordCount int
		Records           []entry
	}
	for _, rec := range input.Records {
		key := "kinesis\x00" + string(rec.Data)
		s.attempts[key]++
		code := ""
		if s.KinesisRecordFault != nil {
			code = s.KinesisRecordFault(rec.Data, s.attempts[key])
		}
		if code != "" {
			out.FailedRecordCount++
			out.Records = append(out.Records, entry{ErrorCode: code, ErrorMessage: "fakeaws: " + code})
			continue
		}
		s.kinesisRecords = append(s.kinesisRecords, Record{Stream: input.StreamName, PartitionKey: rec.PartitionKey, Data: rec.Data})
		out.Records = append(out.Records, entry{SequenceNumber: fmt.Sprintf("%056d", len(s.kinesisRecords)), ShardId: "shardId-000000000000"})
	}
	writeJSON(w, out)
}

func (s *Server) invoke(w http.ResponseWriter, r *http.Request, payload []byte) {
	name, _ := url.PathUnescape(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/2015-03-31/functions/"), "/invocations"))
	invocationType := r.Header.Get("X-Amz-Invocation-Type")
	if invocationType == "" {
		invocationType = "RequestResponse"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests["Lambda.Invoke"]++
	if s.LambdaFault != nil {
		if f := s.LambdaFault(payload, s.requests["Lambda.Invoke"]); f != nil {
			writeRESTError(w, *f)
			return
		}
	}
	limit := LambdaMaxSyncPayloadBytes
	if invocationType == "Event" {
		limit = LambdaMaxAsyncPayloadBytes
	}
	if len(payload) > limit {
		writeRESTError(w, Fault{Status: 413, Code: "RequestEntityTooLargeException", Message: s.violation(
			"Lambda Invoke (%s): payload of %d bytes is above the %d byte limit", invocationType, len(payload), limit)})
		return
	}
	s.invocations = append(s.invocations, Invocation{
		FunctionName: name, Qualifier: r.URL.Query().Get("Qualifier"), InvocationType: invocationType, Payload: payload,
	})
	if invocationType == "Event" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("null"))
}

func writeJSON(w http.ResponseWriter, v any) {
	body, _ := json.Marshal(v)
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// writeJSONError answers in the awsJson1_1 error format (Firehose, Kinesis).
func writeJSONError(w http.ResponseWriter, f Fault) {
	body, _ := json.Marshal(map[string]string{"__type": f.Code, "message": f.Message})
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	w.WriteHeader(f.Status)
	_, _ = w.Write(body)
}

// writeRESTError answers in the restJson1 error format (Lambda).
func writeRESTError(w http.ResponseWriter, f Fault) {
	body, _ := json.Marshal(map[string]string{"Type": "User", "message": f.Message})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Amzn-Errortype", f.Code)
	w.WriteHeader(f.Status)
	_, _ = w.Write(body)
}
