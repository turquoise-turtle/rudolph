package postflight

import (
	"log"
	"net/http"

	apirequest "github.com/airbnb/rudolph/pkg/request"
	"github.com/airbnb/rudolph/pkg/response"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-lambda-go/events"
)

// Parses the HTTP Request into the appropriate request type, or returns a HTTP Response if something is wrong
func parseRequest(request events.APIGatewayProxyRequest) (machineID string, parsedRequest *PostflightRequest, errorResponse *events.APIGatewayProxyResponse, err error) {
	if request.Headers["content-type"] != "application/json" && request.Headers["Content-Type"] != "application/json" {
		errorResponse, err = response.APIResponse(http.StatusUnsupportedMediaType, response.ErrInvalidMediaTypeResponse)
		return
	}

	machineID, errorResponse, err = apirequest.GetMachineID(request)
	if errorResponse != nil || err != nil {
		// Unreachable code; API Gateway should never encounter an error attempting to GetMachineID
		return
	}

	body, errorResponse, err := apirequest.GetBody(request)
	if errorResponse != nil || err != nil {
		return
	}

	if len(body) > 0 {
		// Parse the request
		err = apirequest.UnmarshalSantaJSON(body, &parsedRequest)
		if err != nil {
			log.Printf("postflight: request body could not be parsed")
			errorResponse, err = response.APIResponse(http.StatusBadRequest, response.ErrInvalidBodyResponse)
			return
		}
		if parsedRequest != nil && len(parsedRequest.RulesHash) > maxRulesHashLength {
			log.Printf("postflight: refusing request: rules_hash is longer than %d bytes", maxRulesHashLength)
			errorResponse, err = response.APIResponse(http.StatusBadRequest, response.ErrInvalidBodyResponse)
			return
		}
	}

	return
}

// PostflightRequest is what a Santa client reports about the sync it has just finished.
//
// Santa sends sync_type lowerCamelCased ("syncType"), as it has no `json_name` in sync_v1.proto; the body is
// normalized to the spelling below before unmarshalling. See pkg/request.UnmarshalSantaJSON.
//
// SyncType is typed, so that a value Rudolph does not know is refused at the door rather than stored: it is
// client-controlled input that goes into the machine's row, and an item that grows past DynamoDB's 400 KB
// limit would break every later sync for that host.
type PostflightRequest struct {
	RulesReceived  int `json:"rules_received"`
	RulesProcessed int `json:"rules_processed"`
	// Santa reports the sync type Rudolph sent it, which is only ever NORMAL or CLEAN. If Rudolph starts sending
	// CLEAN_STANDALONE, CLEAN_RULES or CLEAN_FILE_ACCESS_RULES (Santa 2026.8), types.SyncType must learn them.
	SyncType  types.SyncType `json:"sync_type"`
	RulesHash string         `json:"rules_hash"`
}

// maxRulesHashLength bounds the other client-controlled string stored on the sync state row, in bytes. Santa
// reports a hex digest; a postflight with anything longer is refused, like one with an unknown sync type.
const maxRulesHashLength = 128
