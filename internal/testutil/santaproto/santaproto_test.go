package santaproto

import (
	"bytes"
	"compress/zlib"
	"io"
	"strings"
	"testing"
)

// These tests pin down the harness itself: if it stops rejecting what a Santa client rejects,
// every protocol contract test built on it would silently pass.

func TestClientJSON_UsesSantaWireNames(t *testing.T) {
	body := string(ClientJSON(t, "PreflightRequest", `{
		"serial_number": "C02XYZ",
		"client_mode": "LOCKDOWN",
		"sip_status": 127,
		"rules_hash": "abc",
		"request_clean_sync": false
	}`))

	for _, want := range []string{`"serial_num":"C02XYZ"`, `"client_mode":"LOCKDOWN"`, `"sipStatus":127`, `"rulesHash":"abc"`} {
		if !strings.Contains(strings.ReplaceAll(body, " ", ""), want) {
			t.Errorf("Santa-style JSON %s is missing %s", body, want)
		}
	}
	if strings.Contains(body, "request_clean_sync") {
		t.Errorf("default values must be omitted like Santa does, got %s", body)
	}
}

func TestClientJSON_RejectsFieldsNotInProtocol(t *testing.T) {
	ft := &fakeTB{TB: t}
	func() {
		defer func() { _ = recover() }()
		ClientJSON(ft, "PreflightRequest", `{"not_a_field": 1}`)
	}()
	if !ft.failed {
		t.Fatal("fixtures with unknown fields must fail")
	}
}

func TestParseServerResponse_MatchesSantaStrictness(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"unknown fields ignored", `{"client_mode":"MONITOR","upload_logs_url":"/x"}`, false},
		{"deprecated lowercase sync_type alias accepted", `{"sync_type":"clean"}`, false},
		{"remount_usb_mode must be a list", `{"remount_usb_mode":"rdonly"}`, true},
		{"batch_size must be a number", `{"batch_size":"fifty"}`, true},
		{"removable_media_policy oneof", `{"removable_media_policy":{"block":true}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseServerResponse(t, "PreflightResponse", []byte(tc.body))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestDeflate_IsZlibLikeSanta(t *testing.T) {
	r, err := zlib.NewReader(bytes.NewReader(Deflate(t, []byte(`{"a":1}`))))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(r)
	if string(got) != `{"a":1}` {
		t.Fatalf("round trip = %q", got)
	}
}

type fakeTB struct {
	testing.TB
	failed bool
}

func (f *fakeTB) Helper() {}
func (f *fakeTB) Fatalf(string, ...any) {
	f.failed = true
	panic("fatal")
}
