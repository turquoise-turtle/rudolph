package authorizer

import (
	"testing"
)

// Review finding: authorizer results are cached per token, so a single request must never produce a response
// that, once cached, denies the whole fleet sharing that token. A malformed machine ID containing IAM wildcard
// characters must not turn into an Unauthorized error for a valid token; the handlers reject the bad ID instead.
func TestReview_WildcardMachineIDWithValidTokenCannotLockOutFleet(t *testing.T) {
	t.Setenv("SYNC_TOKEN_SHA256", hash(validToken))
	t.Setenv("REGION", "us-east-1")
	t.Setenv("ACCOUNT_ID", "123456789012")
	t.Setenv("GATEWAY_ID", "abcdef1234")

	for _, id := range []string{"*", "AAAAAAAA-1111-4111-8111-AAAAAAAAAAA?", "a*b"} {
		resp, err := HandleAuthorizerRequest(syncRequest(map[string]string{"Authorization": "Bearer " + validToken}, id))
		if err != nil {
			t.Fatalf("machine ID %q: valid token got an error response (%v), which API Gateway may cache for every host using this token", id, err)
		}
		allowsOtherHosts := false
		for _, s := range resp.PolicyDocument.Statement {
			if s.Effect != "Allow" {
				continue
			}
			for _, r := range s.Resource {
				if r == "arn:aws:execute-api:us-east-1:123456789012:abcdef1234/*/POST/preflight/*" {
					allowsOtherHosts = true
				}
			}
		}
		if !allowsOtherHosts {
			t.Errorf("machine ID %q: cached policy would not allow other hosts' preflight: %+v", id, resp.PolicyDocument)
		}
		for _, s := range resp.PolicyDocument.Statement {
			if s.Effect == "Deny" {
				for _, r := range s.Resource {
					if r == "arn:aws:execute-api:us-east-1:123456789012:abcdef1234/*/POST/preflight/*" || r == "*" {
						t.Errorf("machine ID %q: Deny statement %q would block other hosts once cached", id, r)
					}
				}
			}
		}
	}
}
