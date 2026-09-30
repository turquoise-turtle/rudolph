package postflight

import (
	"log"
	"net/http"
	"os"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/response"
	"github.com/aws/aws-lambda-go/events"
)

// PostflightHandler is the entry point for the /postflight API call
type PostPostflightHandler struct {
	booted           bool
	syncStateUpdater syncStateUpdater
}

func (h *PostPostflightHandler) Boot() (err error) {
	if h.booted {
		return
	}

	dynamodbTableName := os.Getenv("DYNAMODB_NAME")
	awsRegion := os.Getenv("REGION")

	client := dynamodb.GetClient(dynamodbTableName, awsRegion)

	h.syncStateUpdater = concreteSyncStateUpdater{
		timeProvider: clock.ConcreteTimeProvider{},
		updater:      client,
	}

	h.booted = true
	return
}

func (h *PostPostflightHandler) Handles(request events.APIGatewayProxyRequest) bool {
	return request.Resource == "/postflight/{machine_id}" && request.HTTPMethod == "POST"
}

func (h *PostPostflightHandler) Handle(request events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, error) {
	machineID, postflightRequest, errResponse, err := parseRequest(request)
	if errResponse != nil {
		return errResponse, nil
	}
	if err != nil {
		// Unreachable code; API Gateway should never encounter an error attempting to GetMachineID
		return errResponse, err
	}

	// Mark the sync as completed. The next preflight records the completion, from which the feed cursor advances and
	// removals count as delivered once two completed syncs sent them (see syncstate.NextFeedSyncCursor and
	// syncstate.DeliveryConfirmed); a sync that never gets here advances neither.
	err = h.syncStateUpdater.updatePostflight(machineID, postflightRequest)
	if err != nil {
		log.Printf("Failed to set final PostflightAt")
		return response.APIResponse(http.StatusInternalServerError, err)
	}

	// Machine rules marked for deletion are deleted by ruledownload once delivery is tracked; whether their removal
	// still has to reach the Mac is decided by the sync state, not by the rows, so postflight reads nothing.

	// Optional: Archive the sync state
	// _ = archiveSyncState(h.dynamodbClient, machineID)

	return response.APIResponse(http.StatusOK, map[string]string{"status": "ok"})
}
