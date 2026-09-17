// Package fakesanta simulates a Santa client performing a full sync against Rudolph's API router.
//
// Every request is produced from, and every response parsed with, the vendored Santa sync protocol
// (see package santaproto), and rules are applied to a local rule database using the same semantics as
// Santa's santasyncservice (SNTSyncRuleDownload.mm / SNTSyncPreflight.mm in testdata):
//
//   - rules Santa cannot parse (no identifier, unknown policy or type) are skipped, not fatal
//   - the preflight sync_type decides cleanup: CLEAN / CLEAN_ALL delete existing rules before applying new ones
//   - downloaded rules are applied in order; REMOVE deletes, anything else replaces the rule for that identifier+type
//   - a requested clean sync keeps being requested until the server performs one (2026.8 SNTSyncPreflight.mm)
//   - allowed/blocked path regexes are only changed when the preflight response contains the field; an empty
//     string clears the regex, an absent field keeps the previously synced value (2026.8 SNTConfigurator.mm)
package fakesanta

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/airbnb/rudolph/internal/testutil/santaproto"
	"github.com/aws/aws-lambda-go/events"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Transport delivers a request the way API Gateway's Lambda proxy integration would, e.g. handlers.ApiRouter.
type Transport func(events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, error)

// Encoding is Santa's SyncClientContentEncoding setting.
type Encoding string

const (
	EncodingDeflate Encoding = "deflate" // Santa's default when unset
	EncodingGzip    Encoding = "gzip"
	EncodingNone    Encoding = "none"
)

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
	SerialNumber string
	Hostname     string
	ModelID      string
	PrimaryUser  string
	ClientMode   string // MONITOR, LOCKDOWN, STANDALONE
	Encoding     Encoding

	// RequestCleanSync makes the next preflight ask for a clean sync (santactl sync --clean).
	RequestCleanSync bool
	// PendingEvents are protojson Event objects uploaded during the next sync.
	PendingEvents []string
	// PendingFileAccessEvents are protojson FileAccessEvent objects uploaded during the next sync.
	PendingFileAccessEvents []string

	Rules map[RuleKey]Rule

	// AllowedPathRegex and BlockedPathRegex are the path regexes last applied from the sync server ("" = none).
	AllowedPathRegex string
	BlockedPathRegex string

	// BeforePostflight, if set, runs after rules are applied and before postflight is sent. Tests use it to
	// change server state while a sync is in flight.
	BeforePostflight func()

	transport Transport
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

// SyncResult records what happened during one sync.
type SyncResult struct {
	Preflight      *dynamicpb.Message
	SyncType       string // NORMAL, CLEAN, CLEAN_ALL as Santa resolved it
	RulesReceived  int
	RulesProcessed int
	RulePages      int
	// Downloaded holds every rule received, in order, as Santa parsed it.
	Downloaded []DownloadedRule
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
	res := &SyncResult{}

	// Preflight
	pre, err := h.call(t, "preflight", "PreflightRequest", h.preflightJSON(), "PreflightResponse")
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
	if len(h.PendingEvents) > 0 || len(h.PendingFileAccessEvents) > 0 {
		body := fmt.Sprintf(`{"machine_id":%q,"events":[%s],"file_access_events":[%s]}`,
			h.MachineID, strings.Join(h.PendingEvents, ","), strings.Join(h.PendingFileAccessEvents, ","))
		if _, err := h.call(t, "eventupload", "EventUploadRequest", body, "EventUploadResponse"); err != nil {
			return res, err
		}
	}

	// Rule download
	var downloaded []DownloadedRule
	cursor := ""
	for {
		req := fmt.Sprintf(`{"machine_id":%q}`, h.MachineID)
		if cursor != "" {
			req = fmt.Sprintf(`{"machine_id":%q,"cursor":%q}`, h.MachineID, cursor)
		}
		resp, err := h.call(t, "ruledownload", "RuleDownloadRequest", req, "RuleDownloadResponse")
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
	switch res.SyncType {
	case "CLEAN", "CLEAN_ALL":
		// Transitive rules are not modelled, so CLEAN and CLEAN_ALL both delete every rule.
		h.Rules = map[RuleKey]Rule{}
	}
	for _, d := range downloaded {
		if d.Skipped {
			continue
		}
		res.RulesProcessed++
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
	h.PendingEvents = nil
	h.PendingFileAccessEvents = nil

	if h.BeforePostflight != nil {
		h.BeforePostflight()
	}

	// Postflight
	post := fmt.Sprintf(`{"machine_id":%q,"rules_received":%d,"rules_processed":%d,"sync_type":%q}`,
		h.MachineID, res.RulesReceived, res.RulesProcessed, res.SyncType)
	if _, err := h.call(t, "postflight", "PostflightRequest", post, "PostflightResponse"); err != nil {
		return res, err
	}
	return res, nil
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

func (h *Host) preflightJSON() string {
	counts := map[string]int{}
	for k := range h.Rules {
		counts[k.Type]++
	}
	return fmt.Sprintf(`{
		"serial_number": %q, "hostname": %q, "os_version": "15.6", "os_build": "24G84",
		"model_identifier": %q, "santa_version": "2026.8.0", "primary_user": %q,
		"client_mode": %q, "request_clean_sync": %t, "machine_id": %q, "sip_status": 127,
		"binary_rule_count": %d, "certificate_rule_count": %d, "teamid_rule_count": %d,
		"signingid_rule_count": %d, "cdhash_rule_count": %d
	}`, h.SerialNumber, h.Hostname, h.ModelID, h.PrimaryUser, h.ClientMode, h.RequestCleanSync, h.MachineID,
		counts["BINARY"], counts["CERTIFICATE"], counts["TEAMID"], counts["SIGNINGID"], counts["CDHASH"])
}

func (h *Host) call(t testing.TB, stage, reqMsg, reqFields, respMsg string) (*dynamicpb.Message, error) {
	t.Helper()
	body := santaproto.ClientJSON(t, reqMsg, reqFields)
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

	resp, err := h.transport(req)
	if err != nil {
		// A Lambda error surfaces to the client as an HTTP 502 from API Gateway.
		return nil, &StageError{Stage: stage, Status: http.StatusBadGateway, Reason: err.Error()}
	}
	if resp == nil {
		return nil, &StageError{Stage: stage, Status: http.StatusBadGateway, Reason: "nil response"}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &StageError{Stage: stage, Status: resp.StatusCode, Body: resp.Body, Reason: "non-200 status"}
	}
	respBody := []byte(resp.Body)
	if resp.IsBase64Encoded {
		respBody, err = base64.StdEncoding.DecodeString(resp.Body)
		if err != nil {
			return nil, &StageError{Stage: stage, Status: resp.StatusCode, Reason: "bad base64 response"}
		}
	}
	// Santa strips an XSSI prefix before parsing.
	respBody = []byte(strings.TrimPrefix(string(respBody), ")]}'\n"))
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
