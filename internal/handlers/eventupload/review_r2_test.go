package eventupload

// Second review of the input handling: pins on the upload limits and the file_sha256 check, which a mutation could
// loosen without any other test failing.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// r2Status sends body to a handler with no events table and no sinks, so only parsing and the limits decide the
// status.
func r2Status(t *testing.T, body string) int {
	t.Helper()
	h := &PostEventuploadHandler{booted: true}
	resp, err := h.Handle(inputHandlingRequest(body))
	if resp == nil {
		t.Fatalf("eventupload gave no response: %v", err)
	}
	return resp.StatusCode
}

// emptyEvents is an upload body whose arrays hold counts[name] empty objects each.
func emptyEvents(counts map[string]int) string {
	var members []string
	for name, n := range counts {
		items := ""
		if n > 0 {
			items = strings.Repeat(`{},`, n-1) + `{}`
		}
		members = append(members, fmt.Sprintf(`%q:[%s]`, name, items))
	}
	return "{" + strings.Join(members, ",") + "}"
}

// maxUploadItems counts the elements of every array of the root object together, since each becomes a decoded item.
func TestReviewR2_UploadItemLimitIsExactly10000(t *testing.T) {
	for name, c := range map[string]struct {
		counts map[string]int
		want   int
	}{
		"10,000 events":                   {map[string]int{"events": 10_000}, 200},
		"10,001 events":                   {map[string]int{"events": 10_001}, 413},
		"10,001 items across three lists": {map[string]int{"events": 4_000, "file_access_events": 4_000, "audit_events": 2_001}, 413},
		"10,000 items across three lists": {map[string]int{"events": 4_000, "file_access_events": 4_000, "audit_events": 2_000}, 200},
	} {
		t.Run(name, func(t *testing.T) {
			if got := r2Status(t, emptyEvents(c.counts)); got != c.want {
				t.Errorf("an upload of %v was answered %d, want %d", c.counts, got, c.want)
			}
		})
	}
}

// maxUploadValues bounds what one event can make the decoder allocate through its nested arrays.
func TestReviewR2_UploadValueLimitCountsNestedArrays(t *testing.T) {
	// The root object, the event (as an element and as an object) and each user are values: 3 plus the users.
	oneEventWithUsers := func(users int) string {
		return `{"events":[{"file_sha256":"` + strings.Repeat("ab", 32) + `","logged_in_users":[` +
			strings.Repeat(`"u",`, users-1) + `"u"]}]}`
	}
	if got := r2Status(t, oneEventWithUsers(200_000)); got != 413 {
		t.Errorf("one event with 200,000 logged in users (200,003 JSON values) was answered %d, want 413", got)
	}
	if got := r2Status(t, oneEventWithUsers(199_997)); got != 200 {
		t.Errorf("one event with 199,997 logged in users (200,000 JSON values) was answered %d, want 200", got)
	}
	nested := `{"events":[{},{},{"signing_chain":[` + strings.Repeat(`{"cn":"x"},`, 100_000) + `{"cn":"x"}]}]}`
	if got := r2Status(t, nested); got != 413 {
		t.Errorf("three events, one with a 100,001 certificate signing chain, were answered %d, want 413", got)
	}
}

// The limits sit far above what Santa sends: an upload at Rudolph's largest batch size (500), each event with a
// three certificate chain, 64 entitlements and a few users, is about 75,000 values and must be accepted.
func TestReviewR2_RealisticLargestUploadIsAccepted(t *testing.T) {
	var upload EventUploadRequest
	for i := 0; i < 500; i++ {
		e := EventUploadEvent{
			FilePath:        "/Applications/Example.app/Contents/MacOS",
			FileName:        "Example",
			FileSHA256:      fmt.Sprintf("%064x", i),
			ExecutionTime:   1_790_000_000 + float64(i),
			Decision:        "BLOCK_UNKNOWN",
			ProcessID:       1000 + i,
			ParentProcessID: 1,
			ExecutingUser:   "alice",
			LoggedInUsers:   []string{"alice", "bob", "carol"},
			CurrentSessions: []string{"alice@console", "bob@ttys000", "carol@ttys001"},
			SigningIDs:      "ABCDE12345:com.example.app",
			TeamID:          "ABCDE12345",
			CDHash:          strings.Repeat("cd", 20),
			EntitlementInfo: &EntitlementInfo{},
		}
		for _, cn := range []string{"Developer ID Application: Example", "Developer ID Certification Authority", "Apple Root CA"} {
			e.SigningChain = append(e.SigningChain, SigningEntry{CertificateName: cn, Organization: "Example", OrganizationalUnit: "ABCDE12345", SHA256: strings.Repeat("ef", 32), ValidFrom: 1, ValidUntil: 2})
		}
		for j := 0; j < 64; j++ {
			e.EntitlementInfo.Entitlements = append(e.EntitlementInfo.Entitlements, Entitlement{Key: fmt.Sprintf("com.apple.security.entitlement-%d", j), Value: "true"})
		}
		upload.Events = append(upload.Events, e)
	}
	body, err := json.Marshal(upload)
	if err != nil {
		t.Fatal(err)
	}
	if got := r2Status(t, string(body)); got != 200 {
		t.Errorf("a realistic upload of 500 events (%d KiB) was answered %d, want 200", len(body)>>10, got)
	}
}

// Beside TestEventStore_FileSHA256ThatIsNotAHashIsNotStored: Santa sends lower-case hex, and a hash with a trailing
// newline would slip past a check anchored with a multi-line $. Neither may become a key: an upper-case hash would
// count the same binary under a second BinaryDay# counter and BinarySeen# marker.
func TestReviewR2_FileSHA256InUpperCaseOrWithANewlineIsNotStored(t *testing.T) {
	valid := strings.Repeat("ab", 32)
	forged := []string{strings.Repeat("AB", 32), valid + "\n"}
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
		t.Errorf("%d events were written, want only the one with a lower-case SHA-256", written)
	}
	for _, key := range api.keys {
		if strings.Contains(key, forged[0]) || strings.Contains(key, "\n") {
			t.Errorf("a forged file_sha256 was written into the key %.80q", key)
		}
	}
}
