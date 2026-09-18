package request

import (
	"encoding/json"
	"strings"
	"testing"
)

// Review finding M1: Santa sends each field under one spelling, but a body that contains both the camelCase and
// the snake_case name must resolve deterministically, and identically however the duplicate is spelled.
// Randomised map iteration previously decided the winner, so the same body could put a host into MONITOR or
// LOCKDOWN at random.
func TestNormalisation_DuplicateSpellingsResolveDeterministically_M1(t *testing.T) {
	cases := map[string]struct {
		body string
		want map[string]any
	}{
		"camel first": {
			body: `{"syncType":"CLEAN","sync_type":"normal","rulesReceived":1,"rules_received":2}`,
			want: map[string]any{"sync_type": "normal", "rules_received": float64(2)},
		},
		"snake first": {
			body: `{"sync_type":"normal","syncType":"CLEAN","rules_received":2,"rulesReceived":1}`,
			want: map[string]any{"sync_type": "normal", "rules_received": float64(2)},
		},
		"repeated snake keys": {
			body: `{"sync_type":"CLEAN","sync_type":"normal"}`,
			want: map[string]any{"sync_type": "normal"},
		},
		"camel only": {
			body: `{"syncType":"CLEAN"}`,
			want: map[string]any{"sync_type": "CLEAN"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			// Repeat: a single run can pass by luck when the order is random.
			for i := 0; i < 200; i++ {
				var got map[string]any
				if err := UnmarshalSantaJSON([]byte(tc.body), &got); err != nil {
					t.Fatalf("run %d: %v", i, err)
				}
				for key, want := range tc.want {
					if got[key] != want {
						t.Fatalf("run %d: %s = %#v, want %#v (body %s)", i, key, got[key], want, tc.body)
					}
				}
			}
		})
	}
}

// Review finding M1, nested: the same rule applies inside nested objects and arrays.
func TestNormalisation_DuplicateSpellingsInNestedObjects_M1(t *testing.T) {
	body := `{"events":[{"csFlags":1,"cs_flags":2,"entitlement_info":{"entitlementsFiltered":true,"entitlements_filtered":false}}]}`
	for i := 0; i < 100; i++ {
		var got struct {
			Events []struct {
				CSFlags         int `json:"cs_flags"`
				EntitlementInfo struct {
					Filtered bool `json:"entitlements_filtered"`
				} `json:"entitlement_info"`
			} `json:"events"`
		}
		if err := UnmarshalSantaJSON([]byte(body), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Events) != 1 || got.Events[0].CSFlags != 2 || got.Events[0].EntitlementInfo.Filtered != false {
			t.Fatalf("run %d: got %+v, want the snake_case values (cs_flags 2, entitlements_filtered false)", i, got)
		}
	}
}

// Values must never be rewritten, only keys.
func TestNormalisation_LeavesValuesAlone_M1(t *testing.T) {
	body := `{"events":[{"file_path":"/Users/someUser/Library/Application Support","entitlement_info":{"entitlements":[{"key":"com.apple.security.cs.allow-jit","value":"{\"nestedKey\":true}"}]}}]}`
	out, err := json.Marshal(json.RawMessage(mustNormalise(t, body)))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`/Users/someUser/Library/Application Support`, `com.apple.security.cs.allow-jit`, `nestedKey`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("normalised body lost %q: %s", want, out)
		}
	}
}

func mustNormalise(t *testing.T, body string) []byte {
	t.Helper()
	var v any
	if err := UnmarshalSantaJSON([]byte(body), &v); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

