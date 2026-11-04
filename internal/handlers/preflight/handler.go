package preflight

import (
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
	"github.com/airbnb/rudolph/pkg/model/syncstate"
	apiRequest "github.com/airbnb/rudolph/pkg/request"
	"github.com/airbnb/rudolph/pkg/response"
	"github.com/aws/aws-lambda-go/events"
)

type PostPreflightHandler struct {
	booted                      bool
	rudolphDynamoDBClient       dynamodb.DynamoDBClient
	machineConfigurationService machineconfiguration.MachineConfigurationService
	stateTrackingService        stateTrackingService
	cleanSyncService            cleanSyncService
	timeProvider                clock.TimeProvider
}

func (h *PostPreflightHandler) Boot() (err error) {
	if h.booted {
		return
	}

	dynamodbTableName := os.Getenv("DYNAMODB_NAME")
	awsRegion := os.Getenv("REGION")
	h.rudolphDynamoDBClient = dynamodb.GetClient(dynamodbTableName, awsRegion)
	h.timeProvider = clock.ConcreteTimeProvider{}

	h.stateTrackingService = getStateTrackingService(h.rudolphDynamoDBClient, h.timeProvider)

	h.cleanSyncService = getCleanSyncService(h.timeProvider)

	h.machineConfigurationService = machineconfiguration.GetMachineConfigurationService(h.rudolphDynamoDBClient, h.timeProvider)

	h.booted = true
	return
}

func (h *PostPreflightHandler) Handles(request events.APIGatewayProxyRequest) bool {
	return request.Resource == "/preflight/{machine_id}" && request.HTTPMethod == "POST"
}

func (h *PostPreflightHandler) Handle(request events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, error) {
	// Get machineID from req.PathParameter of "machine_id" and it must be a valid UUID
	machineID, errResponse, err := apiRequest.GetMachineID(request)
	if errResponse != nil {
		// API Gateway will never allow {machine_id} to be blank,
		// this will fail if the path parameter of "machine_id" does not match a valid UUID
		return errResponse, nil
	}
	if err != nil {
		// Unreachable code; API Gateway should never encounter an error attempting to GetMachineID
		return errResponse, err
	}

	preflightRequest, errorResponse, err := parseRequest(request)
	if errorResponse != nil {
		return errorResponse, nil
	}
	if err != nil {
		return response.APIResponse(http.StatusBadRequest, response.ErrInvalidBodyResponse)
	}
	return h.handlePreflight(machineID, preflightRequest)
}

// The main controller function for API calls to /preflight
func (h *PostPreflightHandler) handlePreflight(machineID string, preflightRequest *PreflightRequest) (*events.APIGatewayProxyResponse, error) {
	// Here we figure out where to set the "feedSync" cursor in the newly created sync state
	var feedSyncCursor string
	var performCleanSync bool = false

	// Save the state of the preflight request - this is the sensor state
	err := h.stateTrackingService.saveSensorDataFromPreflightRequest(machineID, preflightRequest)
	if err != nil {
		return response.InternalServerError(fmt.Errorf("saving the sensor data: %w", err), response.ErrInternalServerErrorResponse)
	}

	// Retrieve the intended configuration for the machine
	machineConfiguration, err := h.machineConfigurationService.GetIntendedConfig(machineID)
	if err != nil {
		return response.InternalServerError(fmt.Errorf("reading the machine configuration: %w", err), response.ErrInternalServerErrorResponse)
	}
	// A stored value Santa cannot use is replaced by a safe default instead of failing every machine using it
	machineConfiguration = machineconfiguration.SanitizeForSanta(machineConfiguration)

	// Get the previous sync state
	prevSyncState, err := h.stateTrackingService.getSyncState(machineID)
	if err != nil {
		return response.InternalServerError(fmt.Errorf("reading the sync state: %w", err), response.ErrInternalServerErrorResponse)
	}
	if prevSyncState != nil {
		// Whether the previous sync's served last page counts as its completion depends on the rules hash the Mac
		// reports now; everything below reads whether it completed
		prevSyncState.SettleCompletion(preflightRequest.RulesHash)
	}

	// The clean sync flag of a machine-specific config asks for one clean sync of that machine, until one is confirmed
	configCleanSync, configCleanSyncConfirmed := configCleanSyncDue(machineConfiguration, prevSyncState)

	// Determine if a Clean sync should be performed based on the preflight request
	// if the machine needs a periodic refresh
	// or if the machine is new
	switch preflightRequest.RequestCleanSync || configCleanSync != nil {
	case true:
		performCleanSync = true
	case false:
		// Retrieve the current feed sync cursor
		feedSyncCursor, performCleanSync = h.stateTrackingService.getFeedSyncStateCursor(prevSyncState)
		// If a clean sync should be forced, break out and do it now
		if performCleanSync {
			break
		}
		// Determine if a refresh clean sync should be performed
		performCleanSync, err = h.cleanSyncService.determineCleanSync(
			machineID,
			preflightRequest,
			prevSyncState,
		)
		if err != nil {
			return response.InternalServerError(fmt.Errorf("deciding whether to clean sync: %w", err), struct{}{})
		}
	default:
		performCleanSync = true
	}

	// Set up a syncState object which will track the progress of the currently requested sync
	// Here we use dynamodb:PutItem to restart the whole process, wipe out any previous sync
	var lastCleanSyncTime string
	switch performCleanSync {
	case true:
		lastCleanSyncTime = clock.RFC3339(h.timeProvider.Now())
	case false:
		// Not prevSyncState.LastCleanSync: the previous sync may have been a clean sync that never completed
		lastCleanSyncTime = prevSyncState.LastCompletedCleanSync()
	}

	err = h.stateTrackingService.saveSyncState(
		machineID,
		// If the CleanSync is going to be forced, log this request in the SensorState, so we can indicate
		// in the /ruledownload step which strategy to use
		performCleanSync,
		lastCleanSyncTime,
		machineConfiguration.BatchSize,
		feedSyncCursor,
		preflightRequest.RulesHash,
		prevSyncState,
		configCleanSync,
	)

	if err != nil {
		err = fmt.Errorf("encountered error trying to save new sync state: %w", err)
		return response.InternalServerError(err, struct{}{})
	}

	// The clean sync the flag asked for was confirmed without a postflight clearing the flag. If clearing fails, the
	// flag makes the next sync clean again, which is harmless.
	if configCleanSyncConfirmed {
		if err := h.machineConfigurationService.ClearMachineCleanSync(machineID, machineConfiguration.CleanSyncRequest); err != nil {
			log.Printf("%v", err)
		}
	}

	// Construct the response
	preflightResponse := ConstructPreflightResponse(machineConfiguration, performCleanSync)

	return response.APIResponse(http.StatusOK, preflightResponse)
}

// configCleanSyncDue decides what the clean sync flag of the machine's own config (CleanSync, never taken from the
// global config) asks of this sync. The flag is cleared once a clean sync performing the request is confirmed: by
// its postflight, which clears the flag itself, or at the next preflight (see syncstate.ConfigCleanSyncOutcome),
// which reports it as confirmed. Until then every sync is a clean sync for it: progress describes it (nil when the
// flag asks for nothing). A clean sync that does not complete is therefore performed again, and one whose last page
// was served but that nothing confirms is repeated at most syncstate.ConfigCleanSyncServingsToConfirm times.
func configCleanSyncDue(config machineconfiguration.MachineConfiguration, prevSyncState *syncstate.SyncStateRow) (progress *syncstate.ConfigCleanSyncProgress, confirmed bool) {
	if !config.CleanSync {
		return nil, false
	}
	var prev *syncstate.SyncState
	if prevSyncState != nil {
		prev = &prevSyncState.SyncState
	}
	confirmed, served := prev.ConfigCleanSyncOutcome(config.CleanSyncRequest)
	if confirmed {
		return nil, true
	}
	return &syncstate.ConfigCleanSyncProgress{Request: config.CleanSyncRequest, Served: served}, false
}
