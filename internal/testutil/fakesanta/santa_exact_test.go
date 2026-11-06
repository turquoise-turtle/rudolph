package fakesanta

// Self-tests for the Santa-exact options and the response size check.

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

func pendingEvents(n int, prefix string) []string {
	var out []string
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf(`{"file_sha256":"%s%02d","decision":"BLOCK_UNKNOWN"}`, prefix, i))
	}
	return out
}

// SNTSyncEventUpload.mm: batches of the preflight batch_size; the first failed batch stops the upload, it and the
// later events stay in the database, the batches before it are deleted; the sync goes on.
func TestSync_SantaExactEventUploadKeepsTheFailedBatchAndLaterEvents(t *testing.T) {
	srv := &scriptedServer{
		preflight: `{"sync_type":"NORMAL","batch_size":2}`,
		pages:     []string{`{"rules":[{"identifier":"aa","rule_type":"BINARY","policy":"BLOCKLIST"}]}`},
		statuses:  map[string][]int{"eventupload": {http.StatusOK, http.StatusInternalServerError}},
	}
	h := NewHost(id, srv.handle)
	h.SantaExactEventUpload = true
	h.PendingEvents = pendingEvents(5, "e")

	res, err := h.Sync(t)
	if err != nil {
		t.Fatalf("a failed event upload must not abort the sync: %v", err)
	}
	if res.EventUploadError == nil || res.EventBatches != 1 || res.EventsUploaded != 2 {
		t.Fatalf("expected one uploaded batch of 2 and a failed one, got batches %d events %d err %v", res.EventBatches, res.EventsUploaded, res.EventUploadError)
	}
	if got := len(srv.requests["eventupload"]); got != 2 {
		t.Fatalf("the upload must stop at the failed batch, sent %d requests", got)
	}
	if h.Policy("BINARY", "aa") != "BLOCKLIST" || len(srv.requests["postflight"]) != 1 {
		t.Fatal("rule download and postflight must still happen")
	}
	if len(h.PendingEvents) != 3 || !strings.Contains(h.PendingEvents[0], `"e02"`) {
		t.Fatalf("the failed batch and the later events must stay pending, have %v", h.PendingEvents)
	}

	res = h.MustSync(t)
	if res.EventBatches != 2 || res.EventsUploaded != 3 || len(h.PendingEvents) != 0 {
		t.Fatalf("the next sync must upload the remaining events in 2 batches, got %d batches, %d events, %d pending", res.EventBatches, res.EventsUploaded, len(h.PendingEvents))
	}
	if !strings.Contains(srv.requests["eventupload"][2], `e02`) || !strings.Contains(srv.requests["eventupload"][2], `e03`) {
		t.Fatalf("the failed batch must be re-sent first: %s", srv.requests["eventupload"][2])
	}
}

func TestSync_SantaExactEventUploadDefaultBatchSizeAndFileAccessEvents(t *testing.T) {
	srv := &scriptedServer{preflight: `{"sync_type":"NORMAL"}`, pages: []string{`{}`}}
	h := NewHost(id, srv.handle)
	h.SantaExactEventUpload = true
	h.PendingEvents = pendingEvents(DefaultEventBatchSize, "x")
	h.PendingFileAccessEvents = []string{`{"rule_name":"r","decision":"FILE_ACCESS_DECISION_DENIED"}`}
	res := h.MustSync(t)
	if res.EventBatches != 2 || len(srv.requests["eventupload"]) != 2 {
		t.Fatalf("51 events with no batch_size must take 2 batches of at most %d, got %d", DefaultEventBatchSize, res.EventBatches)
	}
	if !strings.Contains(srv.requests["eventupload"][1], `"rule_name":"r"`) {
		t.Fatalf("file access events are uploaded after execution events: %s", srv.requests["eventupload"][1])
	}
}

// PerformRequest in SNTSyncEventUpload.mm skips the upload during clean syncs (unless SyncEnableCleanSyncEventUpload), and
// the events are deleted anyway.
func TestSync_SantaExactEventUploadDropsEventsDuringCleanSyncs(t *testing.T) {
	srv := &scriptedServer{preflight: `{"sync_type":"CLEAN"}`, pages: []string{`{"rules":[{"identifier":"aa","rule_type":"BINARY","policy":"BLOCKLIST"}]}`}}
	h := NewHost(id, srv.handle)
	h.SantaExactEventUpload = true
	h.PendingEvents = pendingEvents(3, "c")
	h.MustSync(t)
	if len(srv.requests["eventupload"]) != 0 || len(h.PendingEvents) != 0 {
		t.Fatalf("clean sync: want no upload and no pending events, got %d requests and %d pending", len(srv.requests["eventupload"]), len(h.PendingEvents))
	}

	h.EnableCleanSyncEventUpload = true
	h.PendingEvents = pendingEvents(3, "d")
	h.MustSync(t)
	if len(srv.requests["eventupload"]) != 1 {
		t.Fatal("EnableCleanSyncEventUpload must upload events during clean syncs")
	}
}

// SNTSyncPostflight.mm ignores the postflight response; the sync still counts as successful.
func TestSync_PostflightFailureCanBeIgnoredLikeSanta(t *testing.T) {
	srv := &scriptedServer{
		preflight: `{"sync_type":"NORMAL"}`,
		pages:     []string{`{"rules":[{"identifier":"aa","rule_type":"BINARY","policy":"BLOCKLIST"}]}`},
		status:    map[string]int{"postflight": http.StatusInternalServerError},
	}
	h := NewHost(id, srv.handle)
	if _, err := h.Sync(t); err == nil {
		t.Fatal("by default a failed postflight fails the sync")
	}
	h.IgnorePostflightFailure = true
	res, err := h.Sync(t)
	if err != nil || res.PostflightError == nil {
		t.Fatalf("with IgnorePostflightFailure the sync succeeds and records the failure: err %v, postflight error %v", err, res.PostflightError)
	}
	if h.Policy("BINARY", "aa") != "BLOCKLIST" {
		t.Fatal("rules stay applied")
	}
}

// SNTSyncRuleDownload -sync returns before any cleanup when no rule was received.
func TestSync_CleanSyncWithoutRulesKeepsTheRuleDatabase(t *testing.T) {
	for name, page := range map[string]string{
		"no rules":           `{}`,
		"only unparseable":   `{"rules":[{"identifier":"bb","rule_type":"BINARY","policy":"ALLOWLIST_TRANSITIVE"}]}`,
		"empty rule entries": `{"rules":[{"rule_type":"BINARY","policy":"ALLOWLIST"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			srv := &scriptedServer{preflight: `{"sync_type":"CLEAN"}`, pages: []string{page}}
			h := NewHost(id, srv.handle)
			h.Rules[RuleKey{"BINARY", "stale"}] = Rule{Policy: "ALLOWLIST"}
			h.RequestCleanSync = true
			res := h.MustSync(t)
			if res.SyncType != "CLEAN" || h.Policy("BINARY", "stale") != "ALLOWLIST" {
				t.Fatalf("a clean sync without rules must not wipe the database: type %s, rules %v", res.SyncType, h.Rules)
			}
			if h.RequestCleanSync {
				t.Fatal("the clean sync request is still cleared: the server performed one")
			}
		})
	}
}

// SNTSyncStage.mm dataFromRequest: up to 5 attempts; 5xx, 408 and 429 are retried, other 4xx are not.
func TestSync_RetriesLikeSanta(t *testing.T) {
	ok := func(srv *scriptedServer) *Host {
		h := NewHost(id, srv.handle)
		h.Retries = true
		return h
	}

	srv := &scriptedServer{preflight: `{}`, pages: []string{`{}`}, statuses: map[string][]int{"ruledownload": {503, 429, 408}}}
	res := ok(srv).MustSync(t)
	if res.Requests["ruledownload"] != 4 {
		t.Fatalf("503, 429 and 408 must be retried: %d ruledownload requests", res.Requests["ruledownload"])
	}

	srv = &scriptedServer{preflight: `{}`, pages: []string{`{}`}, statuses: map[string][]int{"preflight": {400}}}
	if _, err := ok(srv).Sync(t); err == nil || len(srv.requests["preflight"]) != 1 {
		t.Fatalf("a 400 is final: err %v, %d preflight requests", err, len(srv.requests["preflight"]))
	}

	srv = &scriptedServer{preflight: `{}`, pages: []string{`{}`}, status: map[string]int{"postflight": 502}}
	if _, err := ok(srv).Sync(t); err == nil || len(srv.requests["postflight"]) != RetryAttempts {
		t.Fatalf("Santa gives up after %d attempts: err %v, %d postflight requests", RetryAttempts, err, len(srv.requests["postflight"]))
	}

	// A response that never arrives (the Lambda ran) is retried too.
	srv = &scriptedServer{preflight: `{}`, pages: []string{`{}`}}
	lost := 1
	h := NewHost(id, func(req events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, error) {
		resp, err := srv.handle(req)
		if strings.HasPrefix(req.Resource, "/ruledownload") && lost > 0 {
			lost--
			return nil, fmt.Errorf("connection lost")
		}
		return resp, err
	})
	h.Retries = true
	h.MustSync(t)
	if len(srv.requests["ruledownload"]) != 2 {
		t.Fatalf("a lost response must be retried, the server saw %d ruledownload requests", len(srv.requests["ruledownload"]))
	}

	// On a 403 Santa fetches an XSRF token (once per sync) and sends the request again with it.
	srv = &scriptedServer{preflight: `{}`, pages: []string{`{}`}, statuses: map[string][]int{"preflight": {403}}}
	ok(srv).MustSync(t)
	if len(srv.requests["xsrf"]) != 1 || srv.headers["preflight"][1]["X-XSRF-TOKEN"] != "token-1" {
		t.Fatalf("expected one XSRF fetch and the token on the retried request: %d fetches, headers %v", len(srv.requests["xsrf"]), srv.headers["preflight"])
	}

	// Without Retries, nothing is retried.
	srv = &scriptedServer{preflight: `{}`, pages: []string{`{}`}, statuses: map[string][]int{"ruledownload": {503}}}
	if _, err := NewHost(id, srv.handle).Sync(t); err == nil {
		t.Fatal("without Retries a 503 fails the sync")
	}
}

// recordingTB captures Errorf so that a self-test can check that the harness reports a failure.
type recordingTB struct {
	testing.TB
	errors []string
}

func (r *recordingTB) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func TestSync_ResponsesAboveTheLambdaLimitFailTheTestAndTheStage(t *testing.T) {
	var rules []string
	for i := 0; len(strings.Join(rules, ",")) < MaxLambdaResponseBytes; i++ {
		rules = append(rules, fmt.Sprintf(`{"identifier":"%064x","rule_type":"BINARY","policy":"BLOCKLIST","custom_msg":"%s"}`, i, strings.Repeat("m", 200)))
	}
	srv := &scriptedServer{preflight: `{}`, pages: []string{`{"rules":[` + strings.Join(rules, ",") + `]}`}}
	rec := &recordingTB{TB: t}
	_, err := NewHost(id, srv.handle).Sync(rec)
	if err == nil || !strings.Contains(err.Error(), "6 MB") {
		t.Fatalf("an oversized response must fail the stage, got %v", err)
	}
	if len(rec.errors) == 0 || !strings.Contains(rec.errors[0], "limit") {
		t.Fatalf("an oversized response must fail the test, got %v", rec.errors)
	}

	srv = &scriptedServer{preflight: `{}`, pages: []string{`{"rules":[` + strings.Join(rules[:len(rules)/2], ",") + `]}`}}
	res := NewHost(id, srv.handle).MustSync(t)
	if res.MaxResponseBytes < MaxLambdaResponseBytes/2-1000 || res.MaxResponseBytes > MaxLambdaResponseBytes {
		t.Fatalf("MaxResponseBytes = %d", res.MaxResponseBytes)
	}
}

func TestSync_SendsPrimaryUserGroupsAndRulesHashWhenSet(t *testing.T) {
	srv := &scriptedServer{preflight: `{}`, pages: []string{`{}`}}
	h := NewHost(id, srv.handle)
	h.MustSync(t)
	if strings.Contains(srv.requests["preflight"][0], "primary_user_groups") || strings.Contains(srv.requests["preflight"][0], "rulesHash") {
		t.Fatalf("unset fields must not be sent: %s", srv.requests["preflight"][0])
	}
	h.PrimaryUserGroups = []string{"staff", "admin"}
	h.RulesHash = "abc123"
	h.MustSync(t)
	if !strings.Contains(strings.ReplaceAll(srv.requests["preflight"][1], " ", ""), `"primary_user_groups":["staff","admin"]`) || !strings.Contains(srv.requests["preflight"][1], `"rulesHash":"abc123"`) {
		t.Fatalf("preflight must carry the groups and the rules hash: %s", srv.requests["preflight"][1])
	}
	if !strings.Contains(srv.requests["postflight"][1], `"rulesHash":"abc123"`) {
		t.Fatalf("postflight must carry the rules hash: %s", srv.requests["postflight"][1])
	}

	h.SerialNumber = ""
	h.MustSync(t)
	if strings.Contains(srv.requests["preflight"][2], "serial_num") {
		t.Fatalf("an empty serial number is not sent, as by Santa: %s", srv.requests["preflight"][2])
	}
}
