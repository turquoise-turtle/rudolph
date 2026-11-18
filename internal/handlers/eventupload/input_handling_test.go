package eventupload

// Input handling review: bodies a holder of the fleet's sync token can send to /eventupload that no Santa sends.

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	dynamotypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func inputHandlingRequest(body string) events.APIGatewayProxyRequest {
	return events.APIGatewayProxyRequest{
		HTTPMethod: "POST", Resource: "/eventupload/{machine_id}",
		PathParameters: map[string]string{"machine_id": machineID},
		Headers:        map[string]string{"Content-Type": "application/json"},
		Body:           body,
	}
}

// A body of JSON `null` unmarshals without error and leaves the request pointer nil. The handler must answer 400, as
// preflight does, instead of dereferencing it: a panic ends the Lambda's process, API Gateway answers 502, the
// lambda_errors alarm counts it and the next request pays a cold start.
func TestEventuploadHandler_NullBodyIsABadRequestNotAPanic(t *testing.T) {
	for _, body := range []string{`null`, ` null `} {
		t.Run(body, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("the handler panicked on a body of %q: %v", body, r)
				}
			}()
			h := &PostEventuploadHandler{booted: true}
			resp, err := h.Handle(inputHandlingRequest(body))
			if err != nil || resp == nil || resp.StatusCode != 400 {
				t.Fatalf("a body of %q was answered %+v, %v; want 400", body, resp, err)
			}
		})
	}
}

// The number of events in an upload is not bounded before they are decoded. An empty event is three bytes of body
// ("{},") and becomes a whole EventUploadEvent in memory, so a body well under the 6 MiB cap needs many times the
// eventupload Lambda's 256 MB: the runtime is killed, API Gateway answers 502 and the next upload pays a cold start.
// Repeated, the body also compresses to almost nothing, and API Gateway decompresses it before the Lambda.
//
// The test measures the bytes the handler allocates for a 512 KiB body of empty events, with no sink and no events
// table, so nothing but decoding is measured. runtime.MemStats.TotalAlloc only grows and does not depend on when the
// garbage collector runs, so the measurement is repeatable. The bound of 64 times the body (32 MiB here) is far above
// what a handler that caps the event count, or that decodes events one at a time, allocates (a few times the body),
// and far below what decoding every event at once allocates (several hundred times the body).
func TestEventuploadHandler_MemoryIsBoundedByTheBodySize(t *testing.T) {
	const bodySize = 512 << 10
	const maxAllocPerBodyByte = 64
	count := (bodySize - len(`{"events":[]}`)) / len(`{},`)
	body := `{"events":[` + strings.Repeat(`{},`, count-1) + `{}]}`

	h := &PostEventuploadHandler{booted: true}
	request := inputHandlingRequest(body)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	resp, err := h.Handle(request)
	runtime.ReadMemStats(&after)

	if err != nil || resp == nil {
		t.Fatalf("eventupload failed: %v %+v", err, resp)
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	if allocated > maxAllocPerBodyByte*uint64(len(body)) {
		t.Errorf("a %d KiB body of %d empty events made the handler allocate %d MiB (%d bytes per byte of body), want at most %d: a 6 MiB body would need far more than the Lambda's memory (answered %d)",
			len(body)>>10, count, allocated>>20, allocated/uint64(len(body)), maxAllocPerBodyByte, resp.StatusCode)
	}
}

// storeRecorder is an eventStoreAPI that accepts every write and keeps the keys it was asked to write.
type storeRecorder struct {
	mu   sync.Mutex
	keys []string
}

func (r *storeRecorder) record(items ...map[string]dynamotypes.AttributeValue) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, item := range items {
		for _, name := range []string{"PK", "SK"} {
			if v, ok := item[name].(*dynamotypes.AttributeValueMemberS); ok {
				r.keys = append(r.keys, v.Value)
			}
		}
	}
}

func (r *storeRecorder) PutItem(_ context.Context, in *dynamodb.PutItemInput, _ ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error) {
	r.record(in.Item)
	return &dynamodb.PutItemOutput{}, nil
}

func (r *storeRecorder) TransactWriteItems(_ context.Context, in *dynamodb.TransactWriteItemsInput, _ ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error) {
	for _, item := range in.TransactItems {
		switch {
		case item.Put != nil:
			r.record(item.Put.Item)
		case item.Update != nil:
			r.record(item.Update.Key)
		case item.ConditionCheck != nil:
			r.record(item.ConditionCheck.Key)
		case item.Delete != nil:
			r.record(item.Delete.Key)
		}
	}
	return &dynamodb.TransactWriteItemsOutput{}, nil
}

// file_sha256 goes into the event's sort key, the BinaryDay# counter and the BinarySeen# marker, which the ranker and
// rudolph-web read back as a binary's hash. Santa always sends 64 hex characters; anything else comes from a forged
// upload and must be dropped before any write, not stored as a counter row (strings up to DynamoDB's 1 KB sort key
// limit are accepted by DynamoDB itself, so its ValidationException is no guard).
func TestEventStore_FileSHA256ThatIsNotAHashIsNotStored(t *testing.T) {
	valid := strings.Repeat("ab", 32)
	forged := []string{
		"not-a-hash",
		strings.Repeat("Z", 64),
		strings.Repeat("ab", 32) + "#extra",
		strings.Repeat("a", 1000),
	}
	now := time.Unix(1_790_000_000, 0)
	events := []EventUploadEvent{{FileSHA256: valid, ExecutionTime: float64(now.Unix() - 60), Decision: "BLOCK_UNKNOWN", ProcessID: 1}}
	for i, sha := range forged {
		events = append(events, EventUploadEvent{FileSHA256: sha, ExecutionTime: float64(now.Unix() - 60), Decision: "BLOCK_UNKNOWN", ProcessID: 2 + i})
	}

	api := &storeRecorder{}
	store := newEventStore(api, "events", nil)
	store.now = func() time.Time { return now }
	written, _, err := store.Store(context.Background(), machineID, events, nil)
	if err != nil {
		t.Fatalf("storing failed: %v", err)
	}

	if written != 1 {
		t.Errorf("%d events were written, want only the one whose file_sha256 is a SHA-256", written)
	}
	for _, key := range api.keys {
		for _, sha := range forged {
			if strings.Contains(key, sha) {
				t.Errorf("a forged file_sha256 %.20q was written into the key %.60q", sha, key)
			}
		}
	}
	found := false
	for _, key := range api.keys {
		found = found || strings.Contains(key, valid)
	}
	if !found {
		t.Errorf("the valid event was not written (keys %q)", api.keys)
	}
}
