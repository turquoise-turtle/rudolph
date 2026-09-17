package types

import "testing"

// Values that exist in the Santa 2026.8 sync protocol (testdata/santa-2026.8/sync_v1.proto) must round-trip.
// Shortcomings P2 and P5.

func TestClientMode_Standalone_P2(t *testing.T) {
	var c ClientMode
	if err := c.UnmarshalText([]byte("STANDALONE")); err != nil {
		t.Fatalf("STANDALONE is a valid Santa client mode: %v", err)
	}
	out, err := c.MarshalText()
	if err != nil || string(out) != "STANDALONE" {
		t.Fatalf("round trip = %q, %v", out, err)
	}
}

func TestRuleType_CDHash_P5(t *testing.T) {
	var r RuleType
	if err := r.UnmarshalText([]byte("CDHASH")); err != nil {
		t.Fatalf("CDHASH is a valid Santa rule type: %v", err)
	}
	out, err := r.MarshalText()
	if err != nil || string(out) != "CDHASH" {
		t.Fatalf("round trip = %q, %v", out, err)
	}
}

func TestPolicy_NewerPolicies_P5(t *testing.T) {
	for _, name := range []string{"CEL", "SILENT_GUI_BLOCKLIST", "SILENT_TTY_BLOCKLIST"} {
		t.Run(name, func(t *testing.T) {
			var p Policy
			if err := p.UnmarshalText([]byte(name)); err != nil {
				t.Fatalf("%s is a valid Santa policy: %v", name, err)
			}
			out, err := p.MarshalText()
			if err != nil || string(out) != name {
				t.Fatalf("round trip = %q, %v", out, err)
			}
			av, err := p.MarshalDynamoDBAttributeValue()
			if err != nil {
				t.Fatal(err)
			}
			var back Policy
			if err := back.UnmarshalDynamoDBAttributeValue(av); err != nil || back != p {
				t.Fatalf("DynamoDB round trip = %v, %v", back, err)
			}
		})
	}
}
