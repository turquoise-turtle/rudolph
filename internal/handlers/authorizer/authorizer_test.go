package authorizer

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

// Shortcoming S1. Contract:
//   - Santa sends a bearer token configured through SyncExtraHeaders (e.g. {"Authorization": "Bearer <token>"}).
//   - The authorizer Lambda gets the SHA-256 hex digests of valid tokens from SYNC_TOKEN_SHA256 (comma separated,
//     so two tokens can be valid during rotation). Plain tokens are never deployed to AWS.
//   - Anything without a valid token is denied, including when no token hashes are configured.
//   - Sync routes require a UUID machine ID.

const (
	validToken   = "rudolph-sync-token-0123456789abcdef0123456789abcdef"
	rotatedToken = "rudolph-sync-token-fedcba9876543210fedcba9876543210"
	machineID    = "AAAAAAAA-1111-4111-8111-AAAAAAAAAAAA"
)

func hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func syncRequest(headers map[string]string, id string) events.APIGatewayCustomAuthorizerRequestTypeRequest {
	return events.APIGatewayCustomAuthorizerRequestTypeRequest{
		Type:           "REQUEST",
		MethodArn:      "arn:aws:execute-api:us-east-1:123456789012:abcdef1234/prod/POST/preflight/" + id,
		Resource:       "/preflight/{machine_id}",
		Path:           "/preflight/" + id,
		HTTPMethod:     "POST",
		Headers:        headers,
		PathParameters: map[string]string{"machine_id": id},
	}
}

func allowed(t *testing.T, req events.APIGatewayCustomAuthorizerRequestTypeRequest) bool {
	t.Helper()
	resp, err := HandleAuthorizerRequest(req)
	if err != nil {
		// Returning "Unauthorized" is how a Lambda authorizer produces a 401.
		return false
	}
	for _, s := range resp.PolicyDocument.Statement {
		if s.Effect != "Allow" {
			return false
		}
	}
	return len(resp.PolicyDocument.Statement) > 0
}

func TestAuthorizer_AllowsValidBearerToken_S1(t *testing.T) {
	t.Setenv("SYNC_TOKEN_SHA256", hash(validToken))
	if !allowed(t, syncRequest(map[string]string{"Authorization": "Bearer " + validToken}, machineID)) {
		t.Fatal("a valid token must be allowed")
	}
	// Header names are case-insensitive in HTTP; API Gateway passes them through as sent.
	if !allowed(t, syncRequest(map[string]string{"authorization": "Bearer " + validToken}, machineID)) {
		t.Fatal("header name matching must be case-insensitive")
	}
}

func TestAuthorizer_SupportsTokenRotation_S1(t *testing.T) {
	t.Setenv("SYNC_TOKEN_SHA256", hash(validToken)+" , "+hash(rotatedToken))
	for _, token := range []string{validToken, rotatedToken} {
		if !allowed(t, syncRequest(map[string]string{"Authorization": "Bearer " + token}, machineID)) {
			t.Errorf("token %q should be valid during rotation", token[:24])
		}
	}
}

func TestAuthorizer_DeniesMissingOrWrongToken_S1(t *testing.T) {
	t.Setenv("SYNC_TOKEN_SHA256", hash(validToken))
	cases := map[string]map[string]string{
		"no header":         {},
		"wrong token":       {"Authorization": "Bearer nope"},
		"hash as token":     {"Authorization": "Bearer " + hash(validToken)},
		"missing scheme":    {"Authorization": validToken},
		"token with suffix": {"Authorization": "Bearer " + validToken + "x"},
	}
	for name, headers := range cases {
		t.Run(name, func(t *testing.T) {
			if allowed(t, syncRequest(headers, machineID)) {
				t.Fatal("request must be denied")
			}
		})
	}
}

func TestAuthorizer_DeniesEverythingWhenNoTokensConfigured_S1(t *testing.T) {
	t.Setenv("SYNC_TOKEN_SHA256", "")
	if allowed(t, syncRequest(map[string]string{"Authorization": "Bearer " + validToken}, machineID)) {
		t.Fatal("an unconfigured authorizer must fail closed")
	}
	if allowed(t, syncRequest(map[string]string{"Authorization": "Bearer "}, machineID)) {
		t.Fatal("empty bearer token must be denied")
	}
}

func TestAuthorizer_DeniesInvalidMachineIDs_S1(t *testing.T) {
	t.Setenv("SYNC_TOKEN_SHA256", hash(validToken))
	for _, id := range []string{"", "not-a-uuid", "GlobalRules", strings.Repeat("A", 36)} {
		if allowed(t, syncRequest(map[string]string{"Authorization": "Bearer " + validToken}, id)) {
			t.Errorf("machine ID %q must be denied", id)
		}
	}
}

func TestAuthorizer_AllowPolicyIsScopedToThisAPI_S1(t *testing.T) {
	t.Setenv("SYNC_TOKEN_SHA256", hash(validToken))
	t.Setenv("REGION", "us-east-1")
	t.Setenv("ACCOUNT_ID", "123456789012")
	t.Setenv("GATEWAY_ID", "abcdef1234")
	resp, err := HandleAuthorizerRequest(syncRequest(map[string]string{"Authorization": "Bearer " + validToken}, machineID))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range resp.PolicyDocument.Statement {
		for _, r := range s.Resource {
			if !strings.HasPrefix(r, "arn:aws:execute-api:us-east-1:123456789012:abcdef1234/") {
				t.Errorf("allow policy resource %q is not scoped to this API", r)
			}
		}
	}
	if strings.Contains(resp.PrincipalID, validToken) || strings.Contains(resp.UsageIdentifierKey, validToken) {
		t.Error("the token must not be echoed into the principal or usage key")
	}
}
