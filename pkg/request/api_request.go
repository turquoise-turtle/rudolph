package request

import (
	"log"
	"net/http"

	"github.com/airbnb/rudolph/pkg/response"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-lambda-go/events"
)

// IsValidUUID reports whether a machineID is a canonical UUID.
//
// Only the 36 character 8-4-4-4-12 form is accepted, in either case. The other spellings that UUID parsers
// usually allow (braces, a `urn:uuid:` prefix, 32 hex digits without dashes) are refused, because they all
// address the same host under a different partition key, which would scatter one Mac's rules, configuration
// and sync state over several rows. The case of the ID is never rewritten, for the same reason: an existing
// host keeps syncing under exactly the ID it has always used.
func IsValidUUID(machineID string) bool {
	return types.ValidateMachineID(machineID) == nil
}

func GetMachineID(req events.APIGatewayProxyRequest) (machineID string, errorResponse *events.APIGatewayProxyResponse, err error) {
	machineID, found := req.PathParameters["machine_id"]

	if !found || len(machineID) == 0 {
		// Unreachable code; API Gateway will never allow {machine_id} to be blank
		log.Printf("ASSERTION FAILED: Received blank {machine_id}")
		errorResponse, err = response.APIResponse(http.StatusBadRequest, response.ErrBlankPathParameterResponse)
		return
	}

	// Check if UUID is valid
	if !(IsValidUUID(machineID)) {
		// Refuse any non-valid UUID as a path parameter
		machineID = ""
		errorResponse, err = response.APIResponse(http.StatusBadRequest, response.ErrInvalidPathParameterResponse)
		return
	}

	return
}
