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
	ruleDestroyer    staleRuleDestroyer
	syncStateUpdater syncStateUpdater
}

func (h *PostPostflightHandler) Boot() (err error) {
	if h.booted {
		return
	}

	dynamodbTableName := os.Getenv("DYNAMODB_NAME")
	awsRegion := os.Getenv("REGION")

	client := dynamodb.GetClient(dynamodbTableName, awsRegion)

	h.ruleDestroyer = concreteRuleDestroyer{
		queryer: client,
		deleter: client,
	}
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

	// Mark the sync as completed. The next preflight then advances the feed cursor to where this sync started, and
	// takes the machine-specific rules this sync served as delivered (see syncstate.NextFeedSyncCursor and
	// syncstate.NextMachineRulesDelivered); a sync that never gets here advances neither.
	err = h.syncStateUpdater.updatePostflight(machineID, postflightRequest)
	if err != nil {
		log.Printf("Failed to set final PostflightAt")
		return response.APIResponse(http.StatusInternalServerError, err)
	}

	// Delete rules marked for deletion that ruledownload has not deleted already (see staleRuleDestroyer)
	err = h.ruleDestroyer.destroyMachineRulesMarkedForDeletion(machineID)
	if err != nil {
		return response.APIResponse(http.StatusInternalServerError, err)
	}

	// Optional: Archive the sync state
	// _ = archiveSyncState(h.dynamodbClient, machineID)

	return response.APIResponse(http.StatusOK, map[string]string{"status": "ok"})
}
