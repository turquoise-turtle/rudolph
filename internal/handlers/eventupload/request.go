package eventupload

import (
	"encoding/json"
	"log"
	"net/http"

	apirequest "github.com/airbnb/rudolph/pkg/request"
	"github.com/airbnb/rudolph/pkg/response"
	"github.com/aws/aws-lambda-go/events"
)

// Parses the HTTP Request into the appropriate request type, or returns a HTTP Response if something is wrong
func parseRequest(request events.APIGatewayProxyRequest) (machineID string, parsedRequest *EventUploadRequest, errorResponse *events.APIGatewayProxyResponse, err error) {
	if request.Resource != "/eventupload/{machine_id}" || request.HTTPMethod != "POST" {
		// This code is intended to be unreachable, as AWS Lambda will never route to this handler
		// with the wrong method, unless misconfigured.
		log.Printf("ASSERTION FAILED: Reached unreachable route code under /eventupload")
		errorResponse, err = response.APIResponse(http.StatusMethodNotAllowed, nil)
		return
	}

	machineID, errorResponse, err = apirequest.GetMachineID(request)
	if errorResponse != nil || err != nil {
		// Unreachable code; API Gateway should never encounter an error attempting to GetMachineID
		return
	}

	if !apirequest.HasJSONBody(request) {
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

	// Parse the request
	err = apirequest.UnmarshalSantaJSON(body, &parsedRequest)
	if err != nil {
		log.Printf("eventupload: request body could not be parsed")
		errorResponse, err = response.APIResponse(http.StatusBadRequest, response.ErrInvalidBodyResponse)
		return
	}

	return
}

// EventUploadRequest encapsulation of an /eventupload POST body sent by a Santa sensor
type EventUploadRequest struct {
	Events           []EventUploadEvent `json:"events"`
	FileAccessEvents []FileAccessEvent  `json:"file_access_events"`
	AuditEvents      []AuditEvent       `json:"audit_events"`
}

// EventUploadEvent is a single event entry; it mirrors santa.sync.v1.Event.
type EventUploadEvent struct {
	ParentName                   string           `json:"parent_name"`
	FilePath                     string           `json:"file_path"`
	QuarantineTimestamp          int              `json:"quarantine_timestamp"`
	QuarantineDataURL            string           `json:"quarantine_data_url,omitempty"`
	QuarantineRefererURL         string           `json:"quarantine_referer_url,omitempty"`
	QuarantineAgentBundleID      string           `json:"quarantine_agent_bundle_id,omitempty"`
	LoggedInUsers                []string         `json:"logged_in_users"`
	SigningChain                 []SigningEntry   `json:"signing_chain"`
	SigningIDs                   string           `json:"signing_id"`
	TeamID                       string           `json:"team_id"`
	BundleID                     string           `json:"bundle_id"`
	CDHash                       string           `json:"cdhash"`
	ParentProcessID              int              `json:"ppid"`
	ExecutingUser                string           `json:"executing_user"`
	FileName                     string           `json:"file_name"`
	ExecutionTime                float64          `json:"execution_time"`
	FileSHA256                   string           `json:"file_sha256"`
	Decision                     string           `json:"decision"`
	ProcessID                    int              `json:"pid"`
	CurrentSessions              []string         `json:"current_sessions"`
	FileBundleID                 string           `json:"file_bundle_id,omitempty"`
	FileBundlePath               string           `json:"file_bundle_path,omitempty"`
	FileBundleExecutableRelPath  string           `json:"file_bundle_executable_rel_path,omitempty"`
	FileBundleName               string           `json:"file_bundle_name,omitempty"`
	FileBundleVersion            string           `json:"file_bundle_version,omitempty"`
	FileBundleShortVersionString string           `json:"file_bundle_version_string,omitempty"`
	FileBundleHash               string           `json:"file_bundle_hash,omitempty"`
	FileBundleHashMilliseconds   float64          `json:"file_bundle_hash_millis,omitempty"`
	FileBundleBinaryCount        int64            `json:"file_bundle_binary_count,omitempty"`
	EntitlementInfo              *EntitlementInfo `json:"entitlement_info,omitempty"`
	CSFlags                      uint32           `json:"cs_flags,omitempty"`
	SigningStatus                string           `json:"signing_status,omitempty"`
	SecureSigningTime            uint32           `json:"secure_signing_time,omitempty"`
	SigningTime                  uint32           `json:"signing_time,omitempty"`
	StaticRule                   bool             `json:"static_rule,omitempty"`
}

// SigningEntry is optionally present when an event includes a binary that is signed
type SigningEntry struct {
	CertificateName    string `json:"cn"`
	ValidUntil         int    `json:"valid_until"`
	Organization       string `json:"org"`
	ValidFrom          int    `json:"valid_from"`
	OrganizationalUnit string `json:"ou"`
	SHA256             string `json:"sha256"`
}

// EntitlementInfo holds the entitlements of the executable an event is about.
type EntitlementInfo struct {
	EntitlementsFiltered bool          `json:"entitlements_filtered"`
	Entitlements         []Entitlement `json:"entitlements,omitempty"`
}

// Entitlement is a single entitlement key/value pair; the value is JSON serialized by the client.
type Entitlement struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Process describes one process of a file access event's process chain.
type Process struct {
	FilePath     string         `json:"file_path"`
	CDHash       string         `json:"cdhash,omitempty"`
	FileSHA256   string         `json:"file_sha256"`
	SigningID    string         `json:"signing_id,omitempty"`
	TeamID       string         `json:"team_id,omitempty"`
	ProcessID    int            `json:"pid"`
	SigningChain []SigningEntry `json:"signing_chain,omitempty"`
}

// FileAccessEvent is a File Access Authorization policy violation reported by a sensor.
type FileAccessEvent struct {
	RuleVersion  string    `json:"rule_version"`
	RuleName     string    `json:"rule_name"`
	Target       string    `json:"target"`
	ProcessChain []Process `json:"process_chain"`
	AccessTime   float64   `json:"access_time"`
	Decision     string    `json:"decision"`
}

// StandaloneModeRuleCreation records a rule a user approved locally in standalone mode.
type StandaloneModeRuleCreation struct {
	Decision   string `json:"decision"`
	Identifier string `json:"identifier"`
	Timestamp  uint32 `json:"timestamp"`
}

// AuditEvent is context a sensor sends to the sync service; exactly one of its fields is set.
type AuditEvent struct {
	StandaloneModeRuleCreation *StandaloneModeRuleCreation `json:"standalone_mode_rule_creation,omitempty"`
}

// EventPayload represents derived metadata for events uploaded with the UploadEvent endpoint.
type EventPayload struct {
	FileSHA  string          `json:"file_sha256"`
	UnixTime float64         `json:"execution_time"`
	Content  json.RawMessage `json:"-"`
}
