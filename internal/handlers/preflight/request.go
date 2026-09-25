package preflight

import (
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

// maxRulesHashLength bounds a client-controlled value that is stored on the machine's row. Santa reports a
// hex digest; anything longer is not one, and storing it could push the item past DynamoDB's 400 KB limit.
const maxRulesHashLength = 128

// ReportedRulesHash is the rules hash the client reported, if it is plausibly a digest.
func (r *PreflightRequest) ReportedRulesHash() string {
	if r == nil || len(r.RulesHash) > maxRulesHashLength {
		return ""
	}
	return r.RulesHash
}
