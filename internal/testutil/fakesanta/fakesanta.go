// Package fakesanta simulates a Santa client performing a full sync against Rudolph's API router.
//
// Every request is produced from, and every response parsed with, the vendored Santa sync protocol
// (see package santaproto), and rules are applied to a local rule database using the same semantics as
// Santa's santasyncservice (SNTSyncRuleDownload.mm / SNTSyncPreflight.mm in testdata):
//
//   - rules Santa cannot parse (no identifier, unknown policy or type) are skipped, not fatal
//   - the preflight sync_type decides cleanup: CLEAN / CLEAN_ALL delete existing rules before applying new ones, but
//     only when the download produced at least one rule Santa could parse: SNTSyncRuleDownload -sync returns early,
//     without any cleanup, when no rules were received (so a clean sync with zero rules leaves the Mac's rules alone)
//   - downloaded rules are applied in order; REMOVE deletes, anything else replaces the rule for that identifier+type
//   - a requested clean sync keeps being requested until the server performs one (2026.8 SNTSyncPreflight.mm)
//   - allowed/blocked path regexes are only changed when the preflight response contains the field; an empty
//     string clears the regex, an absent field keeps the previously synced value (2026.8 SNTConfigurator.mm)
//
// By default the simulation is stricter than Santa where that helps tests notice broken stages: an event upload or a
// postflight that fails fails the sync, and no request is retried. Options make it behave exactly like Santa 2026.8:
// SantaExactEventUpload, IgnorePostflightFailure and Retries (see SNTSyncEventUpload.mm, SNTSyncManager.mm and
// SNTSyncStage.mm). Every response is checked against the 6 MB limit of a Lambda proxy response.
package fakesanta

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/airbnb/rudolph/internal/testutil/santaproto"
	"github.com/aws/aws-lambda-go/events"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Transport delivers a request the way API Gateway's Lambda proxy integration would, e.g. handlers.ApiRouter.
//
// A Transport that returns an error simulates a request whose response never arrived intact (a Lambda error, which
// API Gateway turns into a 502, or a connection dropped mid-response); Santa treats both as a failed attempt.
type Transport func(events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, error)

// Encoding is Santa's SyncClientContentEncoding setting.
type Encoding string

const (
	EncodingDeflate Encoding = "deflate" // Santa's default when unset
	EncodingGzip    Encoding = "gzip"
	EncodingNone    Encoding = "none"
)

// MaxLambdaResponseBytes is the largest response a synchronously invoked Lambda can return (6 MB), measured on the
// proxy response object as Lambda serializes it. API Gateway answers 502 when a Lambda returns more.
const MaxLambdaResponseBytes = 6 * 1024 * 1024

// DefaultEventBatchSize is the event upload batch size Santa uses when the preflight response has none
// (kDefaultEventBatchSize in SNTSyncConstants.mm).
const DefaultEventBatchSize = 50

// RetryAttempts is how many times Santa sends a request before giving up (SNTSyncStage.mm dataFromRequest).
const RetryAttempts = 5

// RuleKey identifies a rule in Santa's rule database.
type RuleKey struct {
	Type       string // BINARY, CERTIFICATE, TEAMID, SIGNINGID, CDHASH
	Identifier string
}

// Rule is a rule as stored by Santa.
type Rule struct {
	Policy    string // ALLOWLIST, BLOCKLIST, ...
	CustomMsg string
	CustomURL string
	CELExpr   string
}

// Host is a simulated Mac running Santa.
type Host struct {
	MachineID    string
	SerialNumber string // "" is not sent (protobuf JSON omits empty strings), as on a Mac that reports none
	Hostname     string
	ModelID      string
	PrimaryUser  string
	ClientMode   string // MONITOR, LOCKDOWN, STANDALONE
	Encoding     Encoding

	// PrimaryUserGroups is sent in preflight when not empty.
	PrimaryUserGroups []string
	// RulesHash is the hash of the rule database Santa reports in preflight and postflight, when not empty.
	RulesHash string
	// HashRules makes the host report a hash of its current rules instead of RulesHash, as Santa 2026.8 does: the
	// hash changes whenever applying a sync changes the rule database, and only then.
	HashRules bool

	// RequestCleanSync makes the next preflight ask for a clean sync (santactl sync --clean).
	RequestCleanSync bool
	// PendingEvents are protojson Event objects in Santa's event database, uploaded during the next sync.
	PendingEvents []string
	// PendingFileAccessEvents are protojson FileAccessEvent objects uploaded during the next sync.
	PendingFileAccessEvents []string

	Rules map[RuleKey]Rule

	// AllowedPathRegex and BlockedPathRegex are the path regexes last applied from the sync server ("" = none).
	AllowedPathRegex string
	BlockedPathRegex string

	// SantaExactEventUpload uploads events exactly like SNTSyncEventUpload.mm: in batches of the preflight's
	// batch_size (DefaultEventBatchSize when absent), stopping at the first batch that fails. Batches that were
	// uploaded are deleted from PendingEvents / PendingFileAccessEvents; the failed batch and every later event stay
	// for the next sync. A failed upload does not abort the sync. During a CLEAN or CLEAN_ALL sync, events are deleted
	// without being uploaded unless EnableCleanSyncEventUpload is set (as in Santa). Execution events are uploaded
	// before file access events.
	//
	// Without it, all pending events are uploaded in one request, and a failure fails the sync.
	SantaExactEventUpload bool
	// EnableCleanSyncEventUpload is Santa's EnableCleanSyncEventUpload setting (only with SantaExactEventUpload).
	EnableCleanSyncEventUpload bool

	// IgnorePostflightFailure makes a failed postflight not fail the sync, as in Santa 2026.8 (SNTSyncPostflight.mm
	// ignores the response and the rules and settings stay applied). SyncResult.PostflightError records the failure.
	IgnorePostflightFailure bool

	// Retries makes every request behave like SNTSyncStage.mm dataFromRequest: up to RetryAttempts attempts; a
	// failed attempt (no response, a 5xx, 408 or 429) is sent again; on a 403 Santa fetches an XSRF token once per
	// sync and sends the request again; any other 4xx is final. Backoff delays are not simulated.
	Retries bool

	// BeforePostflight, if set, runs after rules are applied and before postflight is sent. Tests use it to
	// change server state while a sync is in flight.
	BeforePostflight func()

	transport Transport
	xsrfToken string
}

// NewHost returns a Santa 2026.8 host in MONITOR mode with default (deflate) request compression.
func NewHost(machineID string, transport Transport) *Host {
	return &Host{
		MachineID:    machineID,
		SerialNumber: "C02" + strings.ReplaceAll(machineID, "-", "")[:9],
		Hostname:     "mac-" + strings.ToLower(machineID[:8]) + ".example.com",
		ModelID:      "Mac15,7",
		PrimaryUser:  "alice",
		ClientMode:   "MONITOR",
		Encoding:     EncodingDeflate,
		Rules:        map[RuleKey]Rule{},
		transport:    transport,
	}
}

// Clone returns a copy of the host (rules and pending events included) that syncs through transport, e.g. to
// simulate an earlier sync of the same Mac that is still running.
func (h *Host) Clone(transport Transport) *Host {
	c := *h
	c.Rules = make(map[RuleKey]Rule, len(h.Rules))
	for k, v := range h.Rules {
		c.Rules[k] = v
	}
	c.PendingEvents = append([]string(nil), h.PendingEvents...)
	c.PendingFileAccessEvents = append([]string(nil), h.PendingFileAccessEvents...)
	c.PrimaryUserGroups = append([]string(nil), h.PrimaryUserGroups...)
	c.transport = transport
	return &c
}

// SyncResult records what happened during one sync.
type SyncResult struct {
	Preflight      *dynamicpb.Message
	SyncType       string // NORMAL, CLEAN, CLEAN_ALL as Santa resolved it
	RulesReceived  int
	RulesProcessed int
	RulePages      int
	// Downloaded holds every rule received, in order, as Santa parsed it.
	Downloaded []DownloadedRule

	// Requests counts the HTTP requests sent per stage ("preflight", "eventupload", "ruledownload", "postflight",
	// "xsrf"), retries included.
	Requests map[string]int
	// MaxResponseBytes is the size of the largest Lambda proxy response of the sync, as Lambda serializes it.
	MaxResponseBytes int

	// EventBatches is the number of event upload batches that succeeded; EventsUploaded the events in them.
	EventBatches   int
	EventsUploaded int
	// EventUploadError is the error of the batch that failed (SantaExactEventUpload only; the sync goes on).
	EventUploadError error
	// PostflightError is the postflight failure (IgnorePostflightFailure only; the sync counts as successful).
	PostflightError error
}

type DownloadedRule struct {
	Key  RuleKey
	Rule Rule
	// Skipped is true when Santa would ignore the rule (unparseable).
	Skipped bool
}

// StageError is returned when Santa would abort the sync.
type StageError struct {
	Stage  string
	Status int
	Body   string
	Reason string
}

func (e *StageError) Error() string {
	return fmt.Sprintf("santa sync aborted at %s: HTTP %d: %s (body: %s)", e.Stage, e.Status, e.Reason, e.Body)
}

// Sync performs preflight, eventupload, ruledownload and postflight. It returns a *StageError when a real
// Santa client would abort the sync; in that case no rules are applied (Santa applies rules transactionally).
func (h *Host) Sync(t testing.TB) (*SyncResult, error) {
	t.Helper()
	res := &SyncResult{Requests: map[string]int{}}
	h.xsrfToken = "" // Santa fetches an XSRF token at most once per sync

	// Preflight
	pre, err := h.call(t, res, "preflight", "PreflightRequest", h.preflightJSON(), "PreflightResponse")
	if err != nil {
		return res, err
	}
	res.Preflight = pre
	res.SyncType = resolveSyncType(pre, h.RequestCleanSync)
	allowedRegex, blockedRegex := h.AllowedPathRegex, h.BlockedPathRegex
	if pre.Has(field(pre, "allowed_path_regex")) {
		allowedRegex = pre.Get(field(pre, "allowed_path_regex")).String()
	}
	if pre.Has(field(pre, "blocked_path_regex")) {
		blockedRegex = pre.Get(field(pre, "blocked_path_regex")).String()
	}

	// Event upload
	if h.SantaExactEventUpload {
		batchSize := int(pre.Get(field(pre, "batch_size")).Uint())
		if batchSize <= 0 {
			batchSize = DefaultEventBatchSize
		}
		h.uploadEventsLikeSanta(t, res, batchSize)
	} else if len(h.PendingEvents) > 0 || len(h.PendingFileAccessEvents) > 0 {
		body := fmt.Sprintf(`{"machine_id":%q,"events":[%s],"file_access_events":[%s]}`,
			h.MachineID, strings.Join(h.PendingEvents, ","), strings.Join(h.PendingFileAccessEvents, ","))
		if _, err := h.call(t, res, "eventupload", "EventUploadRequest", body, "EventUploadResponse"); err != nil {
			return res, err
		}
		res.EventBatches = 1
		res.EventsUploaded = len(h.PendingEvents) + len(h.PendingFileAccessEvents)
	}

	// Rule download
	var downloaded []DownloadedRule
	cursor := ""
	for {
		req := fmt.Sprintf(`{"machine_id":%q}`, h.MachineID)
		if cursor != "" {
			req = fmt.Sprintf(`{"machine_id":%q,"cursor":%q}`, h.MachineID, cursor)
		}
		resp, err := h.call(t, res, "ruledownload", "RuleDownloadRequest", req, "RuleDownloadResponse")
		if err != nil {
			return res, err
		}
		res.RulePages++
		rules := resp.Get(field(resp, "rules")).List()
		for i := 0; i < rules.Len(); i++ {
			downloaded = append(downloaded, parseRule(rules.Get(i).Message()))
		}
		res.RulesReceived += rules.Len()
		cursor = resp.Get(field(resp, "cursor")).String()
		if cursor == "" {
			break
		}
		if res.RulePages > 10000 {
			return res, &StageError{Stage: "ruledownload", Reason: "cursor never ended (pagination loop)"}
		}
	}
	res.Downloaded = downloaded

	// Apply rules (Santa does this in one database transaction).
	for _, d := range downloaded {
		if !d.Skipped {
			res.RulesProcessed++
		}
	}
	// SNTSyncRuleDownload -sync returns early when no rule could be parsed: no cleanup, even for a clean sync.
	if res.RulesProcessed > 0 {
		switch res.SyncType {
		case "CLEAN", "CLEAN_ALL":
			// Transitive rules are not modelled, so CLEAN and CLEAN_ALL both delete every rule.
			h.Rules = map[RuleKey]Rule{}
		}
	}
	for _, d := range downloaded {
		if d.Skipped {
			continue
		}
		if d.Rule.Policy == "REMOVE" {
			delete(h.Rules, d.Key)
			continue
		}
		h.Rules[d.Key] = d.Rule
	}
	h.AllowedPathRegex, h.BlockedPathRegex = allowedRegex, blockedRegex
	if res.SyncType == "CLEAN" || res.SyncType == "CLEAN_ALL" {
		h.RequestCleanSync = false
	}
	if !h.SantaExactEventUpload {
		h.PendingEvents = nil
		h.PendingFileAccessEvents = nil
	}

	if h.BeforePostflight != nil {
		h.BeforePostflight()
	}

	// Postflight
	post := fmt.Sprintf(`{"machine_id":%q,"rules_received":%d,"rules_processed":%d,"sync_type":%q,"rules_hash":%q}`,
		h.MachineID, res.RulesReceived, res.RulesProcessed, res.SyncType, h.rulesHash())
	if _, err := h.call(t, res, "postflight", "PostflightRequest", post, "PostflightResponse"); err != nil {
		if h.IgnorePostflightFailure {
			res.PostflightError = err
			return res, nil
		}
		return res, err
	}
	return res, nil
}

// uploadEventsLikeSanta is EventUpload in SNTSyncEventUpload.mm.
func (h *Host) uploadEventsLikeSanta(t testing.TB, res *SyncResult, batchSize int) {
	t.Helper()
	type stored struct {
		json       string
		fileAccess bool
	}
	var queue []stored
	for _, e := range h.PendingEvents {
		queue = append(queue, stored{json: e})
	}
	for _, e := range h.PendingFileAccessEvents {
		queue = append(queue, stored{json: e, fileAccess: true})
	}
	upload := res.SyncType == "NORMAL" || h.EnableCleanSyncEventUpload

	sent := 0 // events removed from the database
	for start := 0; start < len(queue); start += batchSize {
		batch := queue[start:min(start+batchSize, len(queue))]
		if upload {
			var execution, fileAccess []string
			for _, e := range batch {
				if e.fileAccess {
					fileAccess = append(fileAccess, e.json)
				} else {
					execution = append(execution, e.json)
				}
			}
			body := fmt.Sprintf(`{"machine_id":%q,"events":[%s],"file_access_events":[%s]}`,
				h.MachineID, strings.Join(execution, ","), strings.Join(fileAccess, ","))
			if _, err := h.call(t, res, "eventupload", "EventUploadRequest", body, "EventUploadResponse"); err != nil {
				res.EventUploadError = err
				break
			}
			res.EventBatches++
			res.EventsUploaded += len(batch)
		}
		sent = start + len(batch)
	}

	h.PendingEvents, h.PendingFileAccessEvents = nil, nil
	for _, e := range queue[sent:] {
		if e.fileAccess {
			h.PendingFileAccessEvents = append(h.PendingFileAccessEvents, e.json)
		} else {
			h.PendingEvents = append(h.PendingEvents, e.json)
		}
	}
}

// MustSync is Sync that fails the test if Santa would abort.
func (h *Host) MustSync(t testing.TB) *SyncResult {
	t.Helper()
	res, err := h.Sync(t)
	if err != nil {
		t.Fatalf("sync for %s failed: %v", h.MachineID, err)
	}
	return res
}

// Policy returns the policy Santa has stored for a rule, or "" when there is none.
func (h *Host) Policy(ruleType, identifier string) string {
	return h.Rules[RuleKey{Type: ruleType, Identifier: identifier}].Policy
}

// rulesHash is the rules_hash the host reports. The real hash covers Santa's rule table; this one covers the same
// fields this simulation stores, which is all a server can observe.
func (h *Host) rulesHash() string {
	if !h.HashRules {
		return h.RulesHash
	}
	lines := make([]string, 0, len(h.Rules))
	for k, r := range h.Rules {
		lines = append(lines, strings.Join([]string{k.Type, k.Identifier, r.Policy, r.CustomMsg, r.CustomURL, r.CELExpr}, "\x00"))
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

func (h *Host) preflightJSON() string {
	counts := map[string]int{}
	for k := range h.Rules {
		counts[k.Type]++
	}
	groups, _ := json.Marshal(h.PrimaryUserGroups)
	if len(h.PrimaryUserGroups) == 0 {
		groups = []byte("[]")
	}
	return fmt.Sprintf(`{
		"serial_number": %q, "hostname": %q, "os_version": "15.6", "os_build": "24G84",
		"model_identifier": %q, "santa_version": "2026.8.0", "primary_user": %q, "primary_user_groups": %s,
		"client_mode": %q, "request_clean_sync": %t, "machine_id": %q, "sip_status": 127, "rules_hash": %q,
		"binary_rule_count": %d, "certificate_rule_count": %d, "teamid_rule_count": %d,
		"signingid_rule_count": %d, "cdhash_rule_count": %d
	}`, h.SerialNumber, h.Hostname, h.ModelID, h.PrimaryUser, groups, h.ClientMode, h.RequestCleanSync, h.MachineID,
		h.rulesHash(), counts["BINARY"], counts["CERTIFICATE"], counts["TEAMID"], counts["SIGNINGID"], counts["CDHASH"])
}

func (h *Host) request(t testing.TB, stage string, body []byte) events.APIGatewayProxyRequest {
	t.Helper()
	req := events.APIGatewayProxyRequest{
		HTTPMethod:     http.MethodPost,
		Resource:       "/" + stage + "/{machine_id}",
		Path:           "/" + stage + "/" + h.MachineID,
		PathParameters: map[string]string{"machine_id": h.MachineID},
		Headers: map[string]string{
			"Content-Type": "application/json",
			"User-Agent":   "santasyncservice/2026.8.0",
		},
	}
	if h.xsrfToken != "" {
		req.Headers["X-XSRF-TOKEN"] = h.xsrfToken
	}
	switch h.Encoding {
	case EncodingDeflate, "":
		// API Gateway passes non-text payloads to Lambda base64 encoded (binary media types).
		req.Headers["Content-Encoding"] = "deflate"
		req.Body = base64.StdEncoding.EncodeToString(santaproto.Deflate(t, body))
		req.IsBase64Encoded = true
	case EncodingGzip:
		req.Headers["Content-Encoding"] = "gzip"
		req.Body = base64.StdEncoding.EncodeToString(santaproto.Gzip(t, body))
		req.IsBase64Encoded = true
	case EncodingNone:
		req.Body = string(body)
	}
	return req
}

// send performs one attempt and checks the response against the Lambda response size limit.
func (h *Host) send(t testing.TB, res *SyncResult, stage string, req events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, *StageError) {
	t.Helper()
	res.Requests[stage]++
	resp, err := h.transport(req)
	if err != nil {
		// A Lambda error surfaces to the client as an HTTP 502 from API Gateway; a lost response as no response.
		return nil, &StageError{Stage: stage, Status: http.StatusBadGateway, Reason: err.Error()}
	}
	if resp == nil {
		return nil, &StageError{Stage: stage, Status: http.StatusBadGateway, Reason: "nil response"}
	}
	serialized, err := json.Marshal(resp)
	if err != nil {
		t.Errorf("%s: the Lambda proxy response cannot be serialized: %v", stage, err)
		return nil, &StageError{Stage: stage, Status: http.StatusBadGateway, Reason: "unserializable Lambda response"}
	}
	if len(serialized) > res.MaxResponseBytes {
		res.MaxResponseBytes = len(serialized)
	}
	if len(serialized) > MaxLambdaResponseBytes {
		t.Errorf("%s: Lambda proxy response is %d bytes, above the %d byte limit; API Gateway would answer 502", stage, len(serialized), MaxLambdaResponseBytes)
		return nil, &StageError{Stage: stage, Status: http.StatusBadGateway, Reason: fmt.Sprintf("Lambda response of %d bytes exceeds the 6 MB limit", len(serialized))}
	}
	return resp, nil
}

// fetchXSRFToken is SNTSyncStage -fetchXSRFToken: only once per sync.
func (h *Host) fetchXSRFToken(t testing.TB, res *SyncResult, fetched *bool) bool {
	t.Helper()
	if *fetched {
		return false
	}
	*fetched = true
	req := events.APIGatewayProxyRequest{
		HTTPMethod:     http.MethodPost,
		Resource:       "/xsrf/{machine_id}",
		Path:           "/xsrf/" + h.MachineID,
		PathParameters: map[string]string{"machine_id": h.MachineID},
		Headers:        map[string]string{},
	}
	resp, stageErr := h.send(t, res, "xsrf", req)
	if stageErr != nil || resp.StatusCode != http.StatusOK {
		return false
	}
	for name, value := range resp.Headers {
		if strings.EqualFold(name, "X-XSRF-TOKEN") {
			h.xsrfToken = value
		}
	}
	return true
}

func (h *Host) call(t testing.TB, res *SyncResult, stage, reqMsg, reqFields, respMsg string) (*dynamicpb.Message, error) {
	t.Helper()
	body := santaproto.ClientJSON(t, reqMsg, reqFields)

	attempts := 1
	if h.Retries {
		attempts = RetryAttempts
	}
	xsrfFetched := h.xsrfToken != ""
	var resp *events.APIGatewayProxyResponse
	var stageErr *StageError
	for attempt := 1; attempt <= attempts; attempt++ {
		resp, stageErr = h.send(t, res, stage, h.request(t, stage, body))
		if stageErr == nil && resp.StatusCode == http.StatusOK {
			break
		}
		if !h.Retries {
			break
		}
		status := http.StatusBadGateway
		if stageErr == nil {
			status = resp.StatusCode
		}
		if status == http.StatusForbidden && h.fetchXSRFToken(t, res, &xsrfFetched) {
			continue
		}
		if status >= 400 && status < 500 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests {
			break
		}
	}
	if stageErr != nil {
		return nil, stageErr
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &StageError{Stage: stage, Status: resp.StatusCode, Body: resp.Body, Reason: "non-200 status"}
	}
	respBody := []byte(resp.Body)
	if resp.IsBase64Encoded {
		var err error
		respBody, err = base64.StdEncoding.DecodeString(resp.Body)
		if err != nil {
			return nil, &StageError{Stage: stage, Status: resp.StatusCode, Reason: "bad base64 response"}
		}
	}
	// Santa strips an XSSI prefix before parsing.
	respBody = []byte(strings.TrimPrefix(string(respBody), ")]}'\n"))
	if len(respBody) == 0 {
		// SNTSyncStage performRequest: an empty body is a success with nothing to parse.
		msg, _ := santaproto.ParseServerResponse(t, respMsg, []byte("{}"))
		return msg, nil
	}
	msg, err := santaproto.ParseServerResponse(t, respMsg, respBody)
	if err != nil {
		return nil, &StageError{Stage: stage, Status: resp.StatusCode, Body: resp.Body, Reason: "Santa cannot parse response: " + err.Error()}
	}
	return msg, nil
}

// resolveSyncType mirrors SNTSyncPreflight.mm: sync_type wins over the deprecated clean_sync flag.
func resolveSyncType(pre *dynamicpb.Message, requestedClean bool) string {
	if pre.Has(field(pre, "sync_type")) {
		switch enumName(pre, "sync_type") {
		case "CLEAN", "clean":
			return "CLEAN"
		case "CLEAN_ALL", "clean_all":
			return "CLEAN_ALL"
		default:
			return "NORMAL"
		}
	}
	if pre.Get(field(pre, "deprecated_clean_sync")).Bool() {
		return "CLEAN"
	}
	return "NORMAL"
}

// parseRule mirrors RuleFromProtoRule in SNTSyncRuleDownload.mm.
func parseRule(m protoreflect.Message) DownloadedRule {
	identifier := m.Get(field(m, "identifier")).String()
	if identifier == "" {
		identifier = m.Get(field(m, "deprecated_sha256")).String()
	}
	d := DownloadedRule{
		Key: RuleKey{Type: enumName(m, "rule_type"), Identifier: identifier},
		Rule: Rule{
			Policy:    enumName(m, "policy"),
			CustomMsg: m.Get(field(m, "custom_msg")).String(),
			CustomURL: m.Get(field(m, "custom_url")).String(),
			CELExpr:   m.Get(field(m, "cel_expr")).String(),
		},
	}
	switch d.Rule.Policy {
	case "WHITELIST":
		d.Rule.Policy = "ALLOWLIST"
	case "BLACKLIST":
		d.Rule.Policy = "BLOCKLIST"
	case "SILENT_BLACKLIST":
		d.Rule.Policy = "SILENT_BLOCKLIST"
	case "WHITELIST_COMPILER":
		d.Rule.Policy = "ALLOWLIST_COMPILER"
	}
	validPolicy := map[string]bool{"ALLOWLIST": true, "ALLOWLIST_COMPILER": true, "BLOCKLIST": true,
		"SILENT_BLOCKLIST": true, "SILENT_GUI_BLOCKLIST": true, "SILENT_TTY_BLOCKLIST": true, "REMOVE": true, "CEL": true}
	validType := map[string]bool{"BINARY": true, "CERTIFICATE": true, "TEAMID": true, "SIGNINGID": true, "CDHASH": true}
	d.Skipped = identifier == "" || !validPolicy[d.Rule.Policy] || !validType[d.Key.Type]
	return d
}

func field(m protoreflect.Message, name string) protoreflect.FieldDescriptor {
	fd := m.Descriptor().Fields().ByName(protoreflect.Name(name))
	if fd == nil {
		panic(fmt.Sprintf("field %s not in %s", name, m.Descriptor().FullName()))
	}
	return fd
}

func enumName(m protoreflect.Message, name string) string {
	fd := field(m, name)
	v := fd.Enum().Values().ByNumber(m.Get(fd).Enum())
	if v == nil {
		return fmt.Sprintf("UNKNOWN(%d)", m.Get(fd).Enum())
	}
	return string(v.Name())
}
