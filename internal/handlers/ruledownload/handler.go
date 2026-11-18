package ruledownload

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/groups"
	"github.com/airbnb/rudolph/pkg/model/syncstate"
	apirequest "github.com/airbnb/rudolph/pkg/request"
	"github.com/airbnb/rudolph/pkg/response"
	"github.com/aws/aws-lambda-go/events"
)

// RuleDownloadHandler handles requests to the /ruledownload and /ruledownload/{machine_id} API endpoints
//
//	During every sync, Santa sensors make successive POST requests to the /ruledownload endpoint to paginate
//	through all rules.
//	When given a blank postbody (e.g. {}), it indicates the very first request in a sequence. If the
//	API returns a "cursor" in the response body, this cursor will be sent back verbatim in a subsequent postbody.
//	When a response does not return a "cursor" in the body, it signals that there are no more items to page
//	through, and the sensor will stop sending requests.
type PostRuledownloadHandler struct {
	booted        bool
	cursorService ruledownloadCursorService
	ghandler      globalRuleDownloader
	fhandler      feedRuleDownloader
	mhandler      machineRuleDownloder
}

func (h *PostRuledownloadHandler) Boot() (err error) {
	if h.booted {
		return
	}

	dynamodbTableName := os.Getenv("DYNAMODB_NAME")
	awsRegion := os.Getenv("REGION")

	client, err := dynamodb.GetClient(dynamodbTableName, awsRegion)
	if err != nil {
		return err
	}
	h.cursorService = concreteRuledownloadCursorService{
		timer:   clock.ConcreteTimeProvider{},
		updater: client,
		getter:  client,
	}
	machineRules := machineRulePage{
		queryer:     client,
		getter:      client,
		batchGetter: client,
		batchWriter: client,
		updater:     client,
		deleter:     client,
		timer:       clock.ConcreteTimeProvider{},
		groups:      groups.NewCache(clock.ConcreteTimeProvider{}),
	}
	h.ghandler = concreteGlobalRuleDownloader{
		queryer:      client,
		machineRules: machineRules,
	}
	h.fhandler = concreteFeedRuleDownloader{
		queryer:      client,
		machineRules: machineRules,
	}
	h.mhandler = concreteMachineRuleDownloader{
		machineRules: machineRules,
	}

	h.booted = true
	return
}

func (h *PostRuledownloadHandler) Handles(request events.APIGatewayProxyRequest) bool {
	return request.Resource == "/ruledownload/{machine_id}" && request.HTTPMethod == "POST"
}

func (h *PostRuledownloadHandler) Handle(request events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, error) {
	// Like every other sync stage, refuse a machine ID that is not a canonical UUID before touching storage
	machineID, errorResponse, err := apirequest.GetMachineID(request)
	if errorResponse != nil || err != nil {
		return errorResponse, err
	}

	body, errorResponse, err := apirequest.GetBody(request)
	if errorResponse != nil || err != nil {
		return errorResponse, err
	}

	// Parse the request
	var ruledownloadRequest *RuledownloadRequest
	err = apirequest.UnmarshalSantaJSON(body, &ruledownloadRequest)
	if err != nil {
		log.Printf("  Failed to unmarshall ruledownload request")
		return response.APIResponse(http.StatusBadRequest, response.ErrInvalidBodyResponse)
	}

	if ruledownloadRequest == nil {
		// A body of "null" parses without error but carries no request.
		return response.APIResponse(http.StatusBadRequest, response.ErrInvalidBodyResponse)
	}

	if ruledownloadRequest.RawCursor != "" {
		ruledownloadRequest.Cursor = &ruledownloadCursor{}
		err = json.Unmarshal([]byte(ruledownloadRequest.RawCursor), ruledownloadRequest.Cursor)
		if err != nil {
			log.Printf("  Failed to unmarshall cursor")
			return response.APIResponse(http.StatusBadRequest, response.ErrInvalidBodyResponse)
		}
		if err = ruledownloadRequest.Cursor.validate(); err != nil {
			log.Printf("ruledownload: refusing cursor: %v", err)
			return response.APIResponse(http.StatusBadRequest, response.ErrInvalidBodyResponse)
		}
	}

	return h.handleRuleDownload(machineID, ruledownloadRequest)
}

// The "meat and potatoes" of the ruledownload flow.
func (h *PostRuledownloadHandler) handleRuleDownload(machineID string, ruledownloadRequest *RuledownloadRequest) (*events.APIGatewayProxyResponse, error) {
	cursor, err := h.cursorService.ConstructCursor(*ruledownloadRequest, machineID)
	if err != nil {
		return syncErrorResponse("constructing the ruledownload cursor", err)
	}

	// Whenever the client requests a clean sync, we need to echo it back to the client in the response;
	// this gives the client permission to wipe out its local rules database and restart from scratch
	switch cursor.Strategy {
	case ruledownloadStrategyClean:
		return h.ghandler.handle(machineID, cursor)

	case ruledownloadStrategyIncremental:
		return h.fhandler.handle(machineID, cursor)

	case ruledownloadStrategyMachine:
		return h.mhandler.handle(machineID, ruledownloadRequest)
	}

	// How did you get here??
	return response.InternalServerError(fmt.Errorf("unreachable: unknown ruledownload strategy %v", cursor.Strategy), response.ErrInternalServerErrorResponse)
}

// errNoSyncState is a ruledownload for a machine that has no sync state: it did not preflight, or its sync state
// expired or was forgotten.
var errNoSyncState = errors.New("no sync state: ruledownload without a preflight")

// syncErrorResponse answers a ruledownload that failed with err. Two failures are the client's, not the server's, and
// answered 500 the api_5xx alarm would count them and Santa would retry requests that can never succeed: a machine
// without a sync state is answered 400, and a sync that a newer sync of the machine overtook (Santa gave up on it and
// preflighted again, or the cursor's seq was forged) is answered 409. Both are refused before anything is recorded, or, mid-request, after the tracking summary is invalidated.
func syncErrorResponse(what string, err error) (*events.APIGatewayProxyResponse, error) {
	switch {
	case errors.Is(err, syncstate.ErrSyncSuperseded):
		log.Printf("ruledownload: %s: %v", what, err)
		return response.APIResponse(http.StatusConflict, response.ErrSyncSupersededResponse)
	case errors.Is(err, errNoSyncState):
		log.Printf("ruledownload: %s: %v", what, err)
		return response.APIResponse(http.StatusBadRequest, response.ErrNoSyncStateResponse)
	}
	return response.InternalServerError(fmt.Errorf("%s: %w", what, err), response.ErrInternalServerErrorResponse)
}
