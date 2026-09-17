package fakesanta

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

// scriptedServer answers each stage with canned bodies and records decoded request bodies.
type scriptedServer struct {
	preflight string
	pages     []string
	status    map[string]int
	requests  map[string][]string
}

func (s *scriptedServer) handle(req events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, error) {
	stage := strings.Split(strings.TrimPrefix(req.Resource, "/"), "/")[0]
	body := req.Body
	if req.IsBase64Encoded {
		raw, _ := base64.StdEncoding.DecodeString(req.Body)
		r, err := zlib.NewReader(bytes.NewReader(raw))
		if err == nil {
			b, _ := io.ReadAll(r)
			body = string(b)
		}
	}
	if s.requests == nil {
		s.requests = map[string][]string{}
	}
	s.requests[stage] = append(s.requests[stage], body)
	if code, ok := s.status[stage]; ok {
		return &events.APIGatewayProxyResponse{StatusCode: code, Body: `{}`}, nil
	}
	switch stage {
	case "preflight":
		return &events.APIGatewayProxyResponse{StatusCode: http.StatusOK, Body: s.preflight}, nil
	case "ruledownload":
		page := len(s.requests[stage]) - 1
		return &events.APIGatewayProxyResponse{StatusCode: http.StatusOK, Body: s.pages[page]}, nil
	default:
		return &events.APIGatewayProxyResponse{StatusCode: http.StatusOK, Body: `{}`}, nil
	}
}

const id = "11111111-2222-3333-4444-555555555555"

func TestSync_AppliesPagesInOrderAndHonoursRemoveAndSkipsBadRules(t *testing.T) {
	srv := &scriptedServer{
		preflight: `{"client_mode":"MONITOR","sync_type":"normal"}`,
		pages: []string{
			`{"rules":[{"identifier":"aa","rule_type":"BINARY","policy":"BLOCKLIST"},{"identifier":"old","rule_type":"TEAMID","policy":"REMOVE"}],"cursor":"next"}`,
			`{"rules":[{"identifier":"aa","rule_type":"BINARY","policy":"ALLOWLIST"},{"identifier":"bb","rule_type":"BINARY","policy":"ALLOWLIST_TRANSITIVE"},{"rule_type":"BINARY","policy":"ALLOWLIST"}]}`,
		},
	}
	h := NewHost(id, srv.handle)
	h.Rules[RuleKey{"TEAMID", "old"}] = Rule{Policy: "ALLOWLIST"}
	h.Rules[RuleKey{"CDHASH", "keep"}] = Rule{Policy: "BLOCKLIST"}

	res := h.MustSync(t)

	if got := h.Policy("BINARY", "aa"); got != "ALLOWLIST" {
		t.Errorf("later rule must replace earlier one, got %q", got)
	}
	if got := h.Policy("TEAMID", "old"); got != "" {
		t.Errorf("REMOVE must delete the rule, got %q", got)
	}
	if got := h.Policy("CDHASH", "keep"); got != "BLOCKLIST" {
		t.Errorf("normal sync must keep existing rules, got %q", got)
	}
	if got := h.Policy("BINARY", "bb"); got != "" {
		t.Errorf("non-protocol policy must be skipped like Santa does, got %q", got)
	}
	if res.RulesReceived != 5 || res.RulesProcessed != 3 || res.RulePages != 2 {
		t.Errorf("received/processed/pages = %d/%d/%d, want 5/3/2", res.RulesReceived, res.RulesProcessed, res.RulePages)
	}
	if !strings.Contains(srv.requests["ruledownload"][1], `"cursor":"next"`) {
		t.Errorf("second page must echo the cursor, sent %s", srv.requests["ruledownload"][1])
	}
	if !strings.Contains(srv.requests["preflight"][0], `"cdhash_rule_count":1`) {
		t.Errorf("preflight must report rule counts, sent %s", srv.requests["preflight"][0])
	}
}

func TestSync_CleanSyncWipesExistingRules(t *testing.T) {
	srv := &scriptedServer{
		preflight: `{"sync_type":"CLEAN"}`,
		pages:     []string{`{"rules":[{"identifier":"new","rule_type":"SIGNINGID","policy":"ALLOWLIST"}]}`},
	}
	h := NewHost(id, srv.handle)
	h.Rules[RuleKey{"BINARY", "stale"}] = Rule{Policy: "ALLOWLIST"}

	res := h.MustSync(t)
	if res.SyncType != "CLEAN" {
		t.Fatalf("sync type = %s", res.SyncType)
	}
	if len(h.Rules) != 1 || h.Policy("SIGNINGID", "new") != "ALLOWLIST" {
		t.Fatalf("clean sync must replace all rules, got %v", h.Rules)
	}
}

func TestSync_AbortsWithoutApplyingRules(t *testing.T) {
	srv := &scriptedServer{
		preflight: `{"sync_type":"CLEAN"}`,
		pages:     []string{`{"rules":[]}`},
		status:    map[string]int{"ruledownload": http.StatusInternalServerError},
	}
	h := NewHost(id, srv.handle)
	h.Rules[RuleKey{"BINARY", "stale"}] = Rule{Policy: "ALLOWLIST"}

	_, err := h.Sync(t)
	if err == nil || !strings.Contains(err.Error(), "ruledownload") {
		t.Fatalf("expected ruledownload abort, got %v", err)
	}
	if h.Policy("BINARY", "stale") != "ALLOWLIST" {
		t.Fatal("an aborted sync must not touch the rule database")
	}
}

func TestSync_UnparseableResponseAborts(t *testing.T) {
	srv := &scriptedServer{preflight: `{"remount_usb_mode":"rdonly"}`}
	h := NewHost(id, srv.handle)
	_, err := h.Sync(t)
	if err == nil || !strings.Contains(err.Error(), "cannot parse") {
		t.Fatalf("expected parse abort at preflight, got %v", err)
	}
}

func TestSync_RequestedCleanSyncPersistsUntilServerPerformsIt(t *testing.T) {
	srv := &scriptedServer{
		preflight: `{"sync_type":"NORMAL"}`,
		pages:     []string{`{"rules":[]}`, `{"rules":[]}`},
	}
	h := NewHost(id, srv.handle)
	h.RequestCleanSync = true
	h.MustSync(t)
	if !h.RequestCleanSync {
		t.Fatal("a server that ignored the clean sync request must see it again next sync")
	}
	srv.preflight = `{"sync_type":"CLEAN"}`
	h.MustSync(t)
	if h.RequestCleanSync {
		t.Fatal("request must be cleared once a clean sync happened")
	}
	if !strings.Contains(srv.requests["preflight"][1], `"request_clean_sync":true`) {
		t.Fatalf("second preflight should still request a clean sync: %s", srv.requests["preflight"][1])
	}
}

func TestSync_PathRegexSemantics(t *testing.T) {
	srv := &scriptedServer{pages: []string{`{}`, `{}`, `{}`}}
	h := NewHost(id, srv.handle)

	srv.preflight = `{"allowed_path_regex":"^/opt/a/","blocked_path_regex":"^/tmp/b/"}`
	h.MustSync(t)
	if h.AllowedPathRegex != "^/opt/a/" || h.BlockedPathRegex != "^/tmp/b/" {
		t.Fatalf("regexes not applied: %q %q", h.AllowedPathRegex, h.BlockedPathRegex)
	}

	srv.preflight = `{}`
	h.MustSync(t)
	if h.AllowedPathRegex != "^/opt/a/" || h.BlockedPathRegex != "^/tmp/b/" {
		t.Fatalf("absent fields must keep previous regexes: %q %q", h.AllowedPathRegex, h.BlockedPathRegex)
	}

	srv.preflight = `{"allowed_path_regex":"","blocked_path_regex":""}`
	h.MustSync(t)
	if h.AllowedPathRegex != "" || h.BlockedPathRegex != "" {
		t.Fatalf("empty strings must clear regexes: %q %q", h.AllowedPathRegex, h.BlockedPathRegex)
	}
}
