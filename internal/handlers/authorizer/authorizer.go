package authorizer

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/aws/aws-lambda-go/events"
	"github.com/google/uuid"
)

// Santa authenticates to Rudolph with a bearer token that is configured on hosts through the SyncExtraHeaders
// setting, e.g. {"Authorization": "Bearer <token>"}.
//
// The authorizer Lambda never sees plain tokens in its configuration. SYNC_TOKEN_SHA256 holds a comma separated list
// of the hex encoded SHA-256 digests of all currently valid tokens, so that two tokens can be valid while rotating.
//
// API Gateway caches the authorizer result per Authorization header value. The policy returned for a valid token must
// therefore cover every sync route, not only the route of the request that happened to populate the cache.

const (
	// SyncTokenDigestsEnv is the environment variable holding the comma separated SHA-256 hex digests of valid tokens
	SyncTokenDigestsEnv = "SYNC_TOKEN_SHA256"

	authorizationHeader = "Authorization"
	bearerScheme        = "Bearer"
	principalID         = "SantaSyncClient"
)

// errUnauthorized is the exact error message API Gateway turns into an HTTP 401 response
var errUnauthorized = errors.New("Unauthorized")

// syncRoutes are the POST routes Santa calls, all of which take a machine ID path parameter
var syncRoutes = []string{"preflight", "eventupload", "ruledownload", "postflight", "xsrf"}

type authorizerEnvironment struct {
	Region    string
	GatewayID string
	AccountID string
}

// HandleAuthorizerRequest is the handler to be used by the authorizer function
func HandleAuthorizerRequest(request events.APIGatewayCustomAuthorizerRequestTypeRequest) (*events.APIGatewayCustomAuthorizerResponse, error) {
	// Never log the request: its headers contain the bearer token.
	if !hasValidToken(request, loadTokenDigests()) {
		return nil, errUnauthorized
	}

	env, ok := resolveEnvironment(request.MethodArn)
	if !ok {
		log.Printf("authorizer: cannot determine the API ARN; denying request")
		return nil, errUnauthorized
	}

	if request.HTTPMethod == "GET" && request.Resource == "/health" {
		return allowResponse(env), nil
	}

	if request.HTTPMethod != "POST" {
		return denyResponse(env, request.MethodArn, "Incorrect Method")
	}

	machineID, ok := request.PathParameters["machine_id"]
	if !ok {
		return denyResponse(env, request.MethodArn, "Incorrect Request URI")
	}
	if _, err := uuid.Parse(machineID); err != nil {
		return denyResponse(env, request.MethodArn, "Invalid Machine ID")
	}

	return allowResponse(env), nil
}

// loadTokenDigests reads the valid token digests at invocation time. Malformed entries are ignored; an empty or
// missing configuration yields no digests, which denies every request.
func loadTokenDigests() [][]byte {
	var digests [][]byte
	invalid := false
	for _, entry := range strings.Split(os.Getenv(SyncTokenDigestsEnv), ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		digest, err := hex.DecodeString(strings.ToLower(entry))
		if err != nil || len(digest) != sha256.Size {
			invalid = true
			continue
		}
		digests = append(digests, digest)
	}
	if invalid {
		log.Printf("authorizer: %s contains malformed entries, which are ignored", SyncTokenDigestsEnv)
	}
	if len(digests) == 0 {
		log.Printf("authorizer: no valid token digests configured in %s; denying all requests", SyncTokenDigestsEnv)
	}
	return digests
}

func hasValidToken(request events.APIGatewayCustomAuthorizerRequestTypeRequest, digests [][]byte) bool {
	if len(digests) == 0 {
		return false
	}
	token, ok := bearerToken(request)
	if !ok {
		return false
	}

	presented := sha256.Sum256([]byte(token))
	match := 0
	for _, digest := range digests {
		// Compare against every digest so timing does not depend on which one matched
		match |= subtle.ConstantTimeCompare(presented[:], digest)
	}
	return match == 1
}

// bearerToken extracts the token from the Authorization header. The header name is matched case-insensitively; a
// request that carries more than one Authorization value is rejected as ambiguous.
func bearerToken(request events.APIGatewayCustomAuthorizerRequestTypeRequest) (string, bool) {
	var values []string
	for name, value := range request.Headers {
		if strings.EqualFold(name, authorizationHeader) {
			values = append(values, value)
		}
	}
	for name, multi := range request.MultiValueHeaders {
		if strings.EqualFold(name, authorizationHeader) && len(multi) > 1 {
			return "", false
		}
	}
	if len(values) != 1 {
		return "", false
	}

	scheme, token, found := strings.Cut(strings.TrimSpace(values[0]), " ")
	if !found || !strings.EqualFold(scheme, bearerScheme) {
		return "", false
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", false
	}
	return token, true
}

// resolveEnvironment reads the API identity from the environment at invocation time, falling back to the method ARN
// that API Gateway supplies for any value that is not configured.
func resolveEnvironment(methodArn string) (authorizerEnvironment, bool) {
	env := authorizerEnvironment{
		Region:    os.Getenv("REGION"),
		GatewayID: os.Getenv("GATEWAY_ID"),
		AccountID: os.Getenv("ACCOUNT_ID"),
	}

	// arn:aws:execute-api:<region>:<account>:<api id>/<stage>/<method>/<path>
	parts := strings.SplitN(methodArn, ":", 6)
	if len(parts) == 6 && parts[0] == "arn" && parts[2] == "execute-api" {
		if env.Region == "" {
			env.Region = parts[3]
		}
		if env.AccountID == "" {
			env.AccountID = parts[4]
		}
		if env.GatewayID == "" {
			env.GatewayID, _, _ = strings.Cut(parts[5], "/")
		}
	}

	for _, v := range []string{env.Region, env.AccountID, env.GatewayID} {
		if v == "" || strings.ContainsAny(v, "*?/:") {
			return env, false
		}
	}
	return env, true
}

func (env authorizerEnvironment) apiArn() string {
	return fmt.Sprintf("arn:aws:execute-api:%s:%s:%s", env.Region, env.AccountID, env.GatewayID)
}

// allowedResources lists every route a Santa client may call, in any stage of this API.
// Format: <API ARN>/<STAGE>/<HTTP_METHOD>/<URLPATH>
func (env authorizerEnvironment) allowedResources() []string {
	resources := []string{fmt.Sprintf("%s/*/GET/health", env.apiArn())}
	for _, route := range syncRoutes {
		resources = append(resources, fmt.Sprintf("%s/*/POST/%s/*", env.apiArn(), route))
	}
	return resources
}

func allowStatement(env authorizerEnvironment) events.IAMPolicyStatement {
	return events.IAMPolicyStatement{
		Action:   []string{"execute-api:Invoke"},
		Effect:   "Allow",
		Resource: env.allowedResources(),
	}
}

// denyResponse denies the current request to a client that holds a valid token.
//
// The result is cached per token, and the token is typically shared by the whole fleet, so the policy must not lock
// out other requests: it keeps allowing the sync routes and denies only this exact method ARN. A method ARN that
// contains IAM wildcard characters cannot be denied precisely, so such a request is rejected as Unauthorized instead.
func denyResponse(env authorizerEnvironment, methodArn string, denyReason string) (*events.APIGatewayCustomAuthorizerResponse, error) {
	if !strings.HasPrefix(methodArn, env.apiArn()+"/") || strings.ContainsAny(methodArn, "*?") {
		return nil, errUnauthorized
	}

	return &events.APIGatewayCustomAuthorizerResponse{
		PrincipalID: principalID,
		PolicyDocument: events.APIGatewayCustomAuthorizerPolicy{
			Version: "2012-10-17",
			Statement: []events.IAMPolicyStatement{
				allowStatement(env),
				{
					Action:   []string{"execute-api:Invoke"},
					Effect:   "Deny",
					Resource: []string{methodArn},
				},
			},
		},
		Context: map[string]interface{}{
			"DenyReason": denyReason,
		},
	}, nil
}

func allowResponse(env authorizerEnvironment) *events.APIGatewayCustomAuthorizerResponse {
	return &events.APIGatewayCustomAuthorizerResponse{
		PrincipalID: principalID,
		PolicyDocument: events.APIGatewayCustomAuthorizerPolicy{
			Version:   "2012-10-17",
			Statement: []events.IAMPolicyStatement{allowStatement(env)},
		},
	}
}
