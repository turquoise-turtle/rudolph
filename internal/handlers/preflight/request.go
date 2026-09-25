package preflight

import (
	"fmt"
	"log"
	"net/http"

	apirequest "github.com/airbnb/rudolph/pkg/request"
	"github.com/airbnb/rudolph/pkg/response"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-lambda-go/events"
)

// Parses the HTTP Request into the appropriate request type, or returns a HTTP Response if something is wrong
func parseRequest(request events.APIGatewayProxyRequest) (parsedRequest *PreflightRequest, errorResponse *events.APIGatewayProxyResponse, err error) {
	if request.Headers["content-type"] != "application/json" && request.Headers["Content-Type"] != "application/json" {
		errorResponse, err = response.APIResponse(http.StatusUnsupportedMediaType, response.ErrInvalidMediaTypeResponse)
		return
	}

	body, errorResponse, err := apirequest.GetBody(request)
	if errorResponse != nil || err != nil {
		return
	}

	if len(body) <= 0 {
		errorResponse, err = response.APIResponse(http.StatusBadRequest, response.ErrInvalidBodyResponse)
		return
	}

	err = apirequest.UnmarshalSantaJSON(body, &parsedRequest)
	if err != nil {
		errorResponse, err = response.APIResponse(http.StatusBadRequest, response.ErrInvalidBodyResponse)
		return
	}

	if parsedRequest == nil {
		// A body of "null" parses without error but carries no preflight.
		errorResponse, err = response.APIResponse(http.StatusBadRequest, response.ErrInvalidBodyResponse)
		return
	}

	if validationErr := parsedRequest.validate(); validationErr != nil {
		log.Printf("preflight: refusing request: %v", validationErr)
		errorResponse, err = response.APIResponse(http.StatusBadRequest, response.ErrInvalidBodyResponse)
		return
	}

	return
}

// PreflightRequest represents sync payload sent to a sync server from a Santa client.
//
// Field names follow santa.sync.v1.PreflightRequest (testdata/santa-2026.8/sync_v1.proto); the fields without
// a `json_name` there (sip_status, rules_hash) are sent lowerCamelCased by Santa and normalized to the
// spelling below before unmarshalling. See pkg/request.UnmarshalSantaJSON.
type PreflightRequest struct {
	OSBuild              string           `json:"os_build"`
	SantaVersion         string           `json:"santa_version"`
	Hostname             string           `json:"hostname"`
	OSVersion            string           `json:"os_version"`
	CertificateRuleCount int              `json:"certificate_rule_count"`
	BinaryRuleCount      int              `json:"binary_rule_count"`
	ClientMode           types.ClientMode `json:"client_mode"`
	SerialNumber         string           `json:"serial_num"`
	PrimaryUser          string           `json:"primary_user"`
	PrimaryUserGroups    []string         `json:"primary_user_groups"`
	CompilerRuleCount    int              `json:"compiler_rule_count"`
	TransitiveRuleCount  int              `json:"transitive_rule_count"`
	CDHashRuleCount      int              `json:"cdhash_rule_count"`
	TeamIDRuleCount      int              `json:"teamid_rule_count"`
	SigningIDRuleCount   int              `json:"signingid_rule_count"`
	RequestCleanSync     bool             `json:"request_clean_sync"`
	ModelIdentifier      string           `json:"model_identifier"`
	SIPStatus            *uint32          `json:"sip_status"`
	RulesHash            string           `json:"rules_hash"`
	PushNotificationSync bool             `json:"push_notification_sync"`
}

// Limits, in bytes, on the client-reported strings preflight stores on the machine's sensor data row.
//
// Every one of them is client input written to DynamoDB. Without a bound a single host could push its item past
// DynamoDB's 400 KB limit, or - for SerialNum, which is a key of a secondary index and limited to 2 KB there -
// make every preflight fail with a 500, so that the host could never sync again. A preflight that exceeds a
// limit is refused with HTTP 400 before anything is written. The limits are far above what a Mac reports.
// Documented in docs/sync.md.
const (
	maxSerialNumberLength     = 64  // Apple serial numbers are 10-12 characters
	maxHostnameLength         = 255 // a fully qualified domain name is at most 253 characters
	maxModelIdentifierLength  = 64  // e.g. "Mac15,7"
	maxOSVersionLength        = 32  // e.g. "15.6.1"
	maxOSBuildLength          = 32  // e.g. "24G90"
	maxSantaVersionLength     = 64  // e.g. "2026.8.1"
	maxPrimaryUserLength      = 256 // a user name, or an e-mail address from MachineOwner
	maxPrimaryUserGroups      = 256
	maxPrimaryUserGroupLength = 256
	maxRulesHashLength        = 128 // Santa reports a hex digest
)

// validate refuses a preflight whose stored strings are larger than a Mac would ever report. The error names
// the field but never repeats its value.
func (r *PreflightRequest) validate() error {
	for _, field := range []struct {
		name  string
		value string
		limit int
	}{
		{"serial_num", r.SerialNumber, maxSerialNumberLength},
		{"hostname", r.Hostname, maxHostnameLength},
		{"model_identifier", r.ModelIdentifier, maxModelIdentifierLength},
		{"os_version", r.OSVersion, maxOSVersionLength},
		{"os_build", r.OSBuild, maxOSBuildLength},
		{"santa_version", r.SantaVersion, maxSantaVersionLength},
		{"primary_user", r.PrimaryUser, maxPrimaryUserLength},
		{"rules_hash", r.RulesHash, maxRulesHashLength},
	} {
		if len(field.value) > field.limit {
			return fmt.Errorf("%s is longer than %d bytes", field.name, field.limit)
		}
	}

	if len(r.PrimaryUserGroups) > maxPrimaryUserGroups {
		return fmt.Errorf("primary_user_groups has more than %d entries", maxPrimaryUserGroups)
	}
	for _, group := range r.PrimaryUserGroups {
		if len(group) > maxPrimaryUserGroupLength {
			return fmt.Errorf("a primary_user_groups entry is longer than %d bytes", maxPrimaryUserGroupLength)
		}
	}
	return nil
}
