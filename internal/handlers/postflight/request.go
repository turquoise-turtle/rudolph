package postflight

import (
	"log"
	"net/http"

	apirequest "github.com/airbnb/rudolph/pkg/request"
	"github.com/airbnb/rudolph/pkg/response"
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
	}

	return
}

// PostflightRequest is what a Santa client reports about the sync it has just finished.
//
// Santa sends sync_type lowerCamelCased ("syncType"), as it has no `json_name` in sync_v1.proto; the body is
// normalized to the spelling below before unmarshalling. See pkg/request.UnmarshalSantaJSON.
type PostflightRequest struct {
	RulesReceived  int    `json:"rules_received"`
	RulesProcessed int    `json:"rules_processed"`
	SyncType       string `json:"sync_type"`
	RulesHash      string `json:"rules_hash"`
}
