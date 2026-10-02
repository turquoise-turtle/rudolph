package ddblocal

// The proxy between Rudolph's DynamoDB clients and DynamoDB Local. Production code only sees an endpoint URL; the
// proxy counts operations, measures consumed capacity and lets a test inject faults, without Rudolph knowing.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

var (
	stateMu  sync.Mutex
	counts   = map[string]int{}
	capacity = newCapacity()
	hooks    []*hookEntry
	nextHook int
)

// Call is one DynamoDB API call passing through the proxy.
type Call struct {
	// Op is the operation, e.g. "PutItem", "BatchWriteItem".
	Op string
	// Input is the request as decoded JSON (DynamoDB JSON). A Before hook may change it; the changed request is what
	// DynamoDB Local receives.
	Input map[string]any
	// Output is the decoded response of a forwarded call, for After hooks. An After hook may change it; the changed
	// response is what the client receives.
	Output map[string]any
	// Status is the HTTP status of the response, for After hooks.
	Status int
}

// Fault is an error response DynamoDB could send instead of executing a call.
type Fault struct {
	Status int
	// Type is the exception name, e.g. "ProvisionedThroughputExceededException".
	Type    string
	Message string
}

// Common faults.
var (
	// FaultThrottled is what DynamoDB answers when a request exceeds the table's or partition's throughput. The AWS
	// SDK retries it (by default up to 3 attempts in total).
	FaultThrottled = Fault{Status: 400, Type: "ProvisionedThroughputExceededException", Message: "The level of configured provisioned throughput for the table was exceeded."}
	// FaultThrottling is the error for control plane and on-demand throttling.
	FaultThrottling = Fault{Status: 400, Type: "ThrottlingException", Message: "Rate of requests exceeds the allowed throughput."}
	// FaultInternal is a server error. The AWS SDK retries it.
	FaultInternal = Fault{Status: 500, Type: "InternalServerError", Message: "Internal server error"}
	// FaultAccessDenied is what DynamoDB answers when IAM does not allow the call. The AWS SDK does not retry it.
	FaultAccessDenied = Fault{Status: 400, Type: "AccessDeniedException", Message: "User is not authorized to perform this action"}
)

// Hook observes or changes calls passing through the proxy.
type Hook struct {
	// Before runs before a call is forwarded. Returning a non-nil Fault answers the call with it instead of
	// forwarding it. It may block, e.g. to interleave two requests deterministically.
	Before func(*Call) *Fault
	// After runs on the response of a forwarded call.
	After func(*Call)
}

type hookEntry struct {
	id   int
	hook Hook
}

// AddHook installs a hook until the end of the test. Hooks run in the order they were added.
func AddHook(t testing.TB, hook Hook) {
	t.Helper()
	stateMu.Lock()
	nextHook++
	entry := &hookEntry{id: nextHook, hook: hook}
	hooks = append(hooks, entry)
	stateMu.Unlock()
	t.Cleanup(func() {
		stateMu.Lock()
		defer stateMu.Unlock()
		for i, h := range hooks {
			if h.id == entry.id {
				hooks = append(hooks[:i], hooks[i+1:]...)
				return
			}
		}
	})
}

// FailNext answers the next n calls matching op ("" for any operation) with fault.
func FailNext(t testing.TB, op string, n int, fault Fault) {
	t.Helper()
	var mu sync.Mutex
	remaining := n
	AddHook(t, Hook{Before: func(c *Call) *Fault {
		if op != "" && c.Op != op {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		if remaining <= 0 {
			return nil
		}
		remaining--
		f := fault
		return &f
	}})
}

// LeaveUnprocessed makes the next n BatchWriteItem and BatchGetItem calls process only the first request of each
// table and return the others as UnprocessedItems / UnprocessedKeys, as DynamoDB does when it throttles part of a
// batch.
func LeaveUnprocessed(t testing.TB, n int) {
	t.Helper()
	var mu sync.Mutex
	remaining := n
	stash := map[*Call]map[string]any{}
	AddHook(t, Hook{
		Before: func(c *Call) *Fault {
			if c.Op != "BatchWriteItem" && c.Op != "BatchGetItem" {
				return nil
			}
			mu.Lock()
			defer mu.Unlock()
			if remaining <= 0 {
				return nil
			}
			requestItems, _ := c.Input["RequestItems"].(map[string]any)
			unprocessed := map[string]any{}
			for table, value := range requestItems {
				switch c.Op {
				case "BatchWriteItem":
					writes, _ := value.([]any)
					if len(writes) > 1 {
						requestItems[table] = writes[:1]
						unprocessed[table] = writes[1:]
					}
				case "BatchGetItem":
					keysAndAttributes, _ := value.(map[string]any)
					keys, _ := keysAndAttributes["Keys"].([]any)
					if len(keys) > 1 {
						rest := map[string]any{}
						for k, v := range keysAndAttributes {
							rest[k] = v
						}
						rest["Keys"] = keys[1:]
						keysAndAttributes["Keys"] = keys[:1]
						unprocessed[table] = rest
					}
				}
			}
			if len(unprocessed) > 0 {
				remaining--
				stash[c] = unprocessed
			}
			return nil
		},
		After: func(c *Call) {
			mu.Lock()
			defer mu.Unlock()
			unprocessed, ok := stash[c]
			if !ok || c.Output == nil {
				return
			}
			delete(stash, c)
			if c.Op == "BatchWriteItem" {
				c.Output["UnprocessedItems"] = unprocessed
			} else {
				c.Output["UnprocessedKeys"] = unprocessed
			}
		},
	})
}

// Pause holds the first call for which match returns true before it is forwarded. Reached is closed when that call
// arrives; the call proceeds once Release is called (Release is also called at the end of the test).
type Pause struct {
	Reached <-chan struct{}
	release chan struct{}
	once    sync.Once
}

// Release lets the paused call proceed.
func (p *Pause) Release() { p.once.Do(func() { close(p.release) }) }

// PauseFirst installs a Pause for the first call that matches.
func PauseFirst(t testing.TB, match func(*Call) bool) *Pause {
	t.Helper()
	reached := make(chan struct{})
	p := &Pause{Reached: reached, release: make(chan struct{})}
	var mu sync.Mutex
	done := false
	AddHook(t, Hook{Before: func(c *Call) *Fault {
		mu.Lock()
		if done || !match(c) {
			mu.Unlock()
			return nil
		}
		done = true
		mu.Unlock()
		close(reached)
		<-p.release
		return nil
	}})
	t.Cleanup(p.Release)
	return p
}

// InputJSON returns the call's request as JSON, for matching on its contents.
func (c *Call) InputJSON() string {
	out, _ := json.Marshal(c.Input)
	return string(out)
}

// ---------------------------------------------------------------------------------------------------------------
// Consumed capacity
// ---------------------------------------------------------------------------------------------------------------

// Capacity is the capacity DynamoDB Local reported as consumed, with ReturnConsumedCapacity=INDEXES set on every call
// that supports it. On-demand tables bill one read request unit per RCU and one write request unit per WCU.
//
// Limitation: DynamoDB Local computes these units itself. It rounds item sizes like DynamoDB (4 KB per RCU, half for
// eventually consistent reads; 1 KB per WCU), but whether it charges a global secondary index for a write that leaves
// the index's keys and projected attributes unchanged may differ from DynamoDB; see the ddblocal self-test, which pins
// down what it reports.
type Capacity struct {
	ReadUnits  float64 // total, table and indexes
	WriteUnits float64 // total, table and indexes
	// TableReadUnits and TableWriteUnits are the base table's share.
	TableReadUnits, TableWriteUnits float64
	// IndexReadUnits and IndexWriteUnits are by index name.
	IndexReadUnits, IndexWriteUnits map[string]float64
	// ByOp is the total capacity units (read + write) by operation.
	ByOp map[string]float64
}

func newCapacity() Capacity {
	return Capacity{IndexReadUnits: map[string]float64{}, IndexWriteUnits: map[string]float64{}, ByOp: map[string]float64{}}
}

func (c Capacity) clone() Capacity {
	out := c
	out.IndexReadUnits, out.IndexWriteUnits, out.ByOp = map[string]float64{}, map[string]float64{}, map[string]float64{}
	for k, v := range c.IndexReadUnits {
		out.IndexReadUnits[k] = v
	}
	for k, v := range c.IndexWriteUnits {
		out.IndexWriteUnits[k] = v
	}
	for k, v := range c.ByOp {
		out.ByOp[k] = v
	}
	return out
}

// String summarizes the capacity for test failure messages.
func (c Capacity) String() string {
	var parts []string
	for name, units := range c.IndexWriteUnits {
		parts = append(parts, fmt.Sprintf("%s write %.1f", name, units))
	}
	for name, units := range c.IndexReadUnits {
		parts = append(parts, fmt.Sprintf("%s read %.1f", name, units))
	}
	for op, units := range c.ByOp {
		parts = append(parts, fmt.Sprintf("%s %.1f", op, units))
	}
	sort.Strings(parts)
	return fmt.Sprintf("RCU %.1f (table %.1f), WCU %.1f (table %.1f); %s",
		c.ReadUnits, c.TableReadUnits, c.WriteUnits, c.TableWriteUnits, strings.Join(parts, ", "))
}

// ConsumedCapacity returns the capacity consumed since the last ResetCounts.
func ConsumedCapacity() Capacity {
	stateMu.Lock()
	defer stateMu.Unlock()
	return capacity.clone()
}

// operations that accept ReturnConsumedCapacity
var capacityOps = map[string]bool{
	"GetItem": true, "PutItem": true, "UpdateItem": true, "DeleteItem": true, "Query": true, "Scan": true,
	"BatchGetItem": true, "BatchWriteItem": true, "TransactWriteItems": true, "TransactGetItems": true,
}

var writeOps = map[string]bool{
	"PutItem": true, "UpdateItem": true, "DeleteItem": true, "BatchWriteItem": true, "TransactWriteItems": true,
}

func recordCapacity(op string, consumed any) {
	var list []any
	switch v := consumed.(type) {
	case []any:
		list = v
	case map[string]any:
		list = []any{v}
	default:
		return
	}
	for _, entry := range list {
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		total := number(m["CapacityUnits"])
		read, write := number(m["ReadCapacityUnits"]), number(m["WriteCapacityUnits"])
		if read == 0 && write == 0 {
			// Only CapacityUnits is reported: attribute it by operation.
			if writeOps[op] {
				write = total
			} else {
				read = total
			}
		}
		capacity.ReadUnits += read
		capacity.WriteUnits += write
		capacity.ByOp[op] += read + write
		if table, ok := m["Table"].(map[string]any); ok {
			tr, tw, tc := number(table["ReadCapacityUnits"]), number(table["WriteCapacityUnits"]), number(table["CapacityUnits"])
			if tr == 0 && tw == 0 {
				if writeOps[op] {
					tw = tc
				} else {
					tr = tc
				}
			}
			capacity.TableReadUnits += tr
			capacity.TableWriteUnits += tw
		} else {
			capacity.TableReadUnits += read
			capacity.TableWriteUnits += write
		}
		if indexes, ok := m["GlobalSecondaryIndexes"].(map[string]any); ok {
			for name, value := range indexes {
				index, _ := value.(map[string]any)
				ir, iw, ic := number(index["ReadCapacityUnits"]), number(index["WriteCapacityUnits"]), number(index["CapacityUnits"])
				if ir == 0 && iw == 0 {
					if writeOps[op] {
						iw = ic
					} else {
						ir = ic
					}
				}
				capacity.IndexReadUnits[name] += ir
				capacity.IndexWriteUnits[name] += iw
			}
		}
	}
}

func number(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}

// ---------------------------------------------------------------------------------------------------------------
// The proxy
// ---------------------------------------------------------------------------------------------------------------

func startProxy(backend string) (string, func(), error) {
	target, err := url.Parse(backend)
	if err != nil {
		return "", nil, err
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	reverse := httputil.NewSingleHostReverseProxy(target)
	reverse.Transport = &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 64}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveCall(w, r, reverse)
	})}
	go func() { _ = srv.Serve(l) }()
	return "http://" + l.Addr().String(), func() { _ = srv.Close() }, nil
}

func serveCall(w http.ResponseWriter, r *http.Request, reverse *httputil.ReverseProxy) {
	op := r.Header.Get("X-Amz-Target")
	if i := strings.LastIndex(op, "."); i >= 0 {
		op = op[i+1:]
	}
	stateMu.Lock()
	counts[op]++
	active := append([]*hookEntry(nil), hooks...)
	stateMu.Unlock()

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	call := &Call{Op: op}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&call.Input); err != nil || call.Input == nil {
		// Not a JSON API call (e.g. a health check): pass it through untouched.
		r.Body = io.NopCloser(bytes.NewReader(raw))
		reverse.ServeHTTP(w, r)
		return
	}

	for _, h := range active {
		if h.hook.Before == nil {
			continue
		}
		if fault := h.hook.Before(call); fault != nil {
			writeFault(w, *fault)
			return
		}
	}

	clientAskedForCapacity := false
	if capacityOps[op] {
		if v, ok := call.Input["ReturnConsumedCapacity"].(string); ok && v != "" && v != "NONE" {
			clientAskedForCapacity = true
		}
		call.Input["ReturnConsumedCapacity"] = "INDEXES"
	}
	body, err := json.Marshal(call.Input)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))

	recorder := &responseRecorder{header: http.Header{}, status: http.StatusOK}
	reverse.ServeHTTP(recorder, r)

	call.Status = recorder.status
	respBody := recorder.body.Bytes()
	if len(respBody) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(respBody))
		decoder.UseNumber()
		if err := decoder.Decode(&call.Output); err == nil && call.Output != nil {
			if consumed, ok := call.Output["ConsumedCapacity"]; ok && recorder.status == http.StatusOK {
				stateMu.Lock()
				recordCapacity(op, consumed)
				stateMu.Unlock()
				if !clientAskedForCapacity {
					delete(call.Output, "ConsumedCapacity")
				}
			}
			for _, h := range active {
				if h.hook.After != nil {
					h.hook.After(call)
				}
			}
			if rewritten, err := json.Marshal(call.Output); err == nil {
				respBody = rewritten
			}
		}
	}

	for k, v := range recorder.header {
		w.Header()[k] = v
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(respBody)))
	// The AWS SDK validates the CRC32 of DynamoDB response bodies.
	w.Header().Set("X-Amz-Crc32", strconv.FormatUint(uint64(crc32.ChecksumIEEE(respBody)), 10))
	w.WriteHeader(recorder.status)
	_, _ = w.Write(respBody)
}

func writeFault(w http.ResponseWriter, fault Fault) {
	body, _ := json.Marshal(map[string]string{
		"__type":  "com.amazonaws.dynamodb.v20120810#" + fault.Type,
		"message": fault.Message,
	})
	w.Header().Set("Content-Type", "application/x-amz-json-1.0")
	w.Header().Set("X-Amzn-Errortype", fault.Type)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.Header().Set("X-Amz-Crc32", strconv.FormatUint(uint64(crc32.ChecksumIEEE(body)), 10))
	status := fault.Status
	if status == 0 {
		status = http.StatusBadRequest
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

type responseRecorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *responseRecorder) Header() http.Header         { return r.header }
func (r *responseRecorder) WriteHeader(status int)      { r.status = status }
func (r *responseRecorder) Write(b []byte) (int, error) { return r.body.Write(b) }
