package eventupload

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"

	"github.com/airbnb/rudolph/pkg/eventsink"
	"github.com/airbnb/rudolph/pkg/firehose"
	"github.com/airbnb/rudolph/pkg/kinesis"
	"github.com/airbnb/rudolph/pkg/lambda"

	"github.com/airbnb/rudolph/pkg/response"
	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

type PostEventuploadHandler struct {
	booted         bool
	firehoseClient firehose.FirehoseClient
	enableFirehose bool
	kinesisClient  kinesis.KinesisClient
	enableKinesis  bool

	lambdaClient lambda.LambdaClient
	enableLambda bool

	// eventStore writes execution events to the events table read by the web view, independently of the sinks.
	// Nil when EVENTS_DYNAMODB_NAME is not set.
	eventStore *eventStore
}

func (h *PostEventuploadHandler) Boot() (err error) {
	if h.booted {
		return
	}

	handler := os.Getenv("HANDLER")
	region := os.Getenv("REGION")

	switch handler {
	case "FIREHOSE":
		firehoseName := os.Getenv("FIREHOSE_NAME")
		h.firehoseClient = firehose.GetClient(firehoseName, region)
		h.enableFirehose = true
	case "KINESIS":
		kinesisName := os.Getenv("KINESIS_NAME")
		h.kinesisClient = kinesis.GetClient(kinesisName, region)
		h.enableKinesis = true
	case "NONE":
		fallthrough
	default:
		// No handling of eventupload.
	}

	lambdaName := os.Getenv("LAMBDA_NAME")
	if lambdaName != "" {
		h.enableLambda = true
		h.lambdaClient = lambda.GetClient(lambdaName, "$LATEST", region)
	}

	if eventsTable := os.Getenv("EVENTS_DYNAMODB_NAME"); eventsTable != "" {
		cfg, err := config.LoadDefaultConfig(context.Background(), config.WithRegion(region))
		if err != nil {
			return fmt.Errorf("events table client: %w", err)
		}
		client := dynamodb.NewFromConfig(cfg)
		// The rules whose allowed executions are stored are listed in the store table; without it none are.
		var collected *collectedRules
		if storeTable := os.Getenv("DYNAMODB_NAME"); storeTable != "" {
			collected = newCollectedRules(client, storeTable)
		}
		h.eventStore = newEventStore(client, eventsTable, collected)
	}

	h.booted = true
	return
}

func (h *PostEventuploadHandler) Handles(request events.APIGatewayProxyRequest) bool {
	return request.Resource == "/eventupload/{machine_id}" && request.HTTPMethod == "POST"
}

func (h *PostEventuploadHandler) Handle(request events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, error) {
	return h.HandleContext(context.Background(), request)
}

// HandleContext handles an upload within ctx's deadline (the Lambda invocation's). Sink calls stop
// eventsink.DeadlineMargin before it (half the remaining time under a short timeout, see
// eventsink.WithSendDeadline), so the handler answers 5xx itself instead of being killed by the Lambda timeout;
// without a deadline they are bounded by eventsink.DefaultBudget.
func (h *PostEventuploadHandler) HandleContext(ctx context.Context, request events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, error) {
	machineID, eventsRequest, errorResponse, err := parseRequest(request)
	if errorResponse != nil || err != nil {
		return errorResponse, err
	}

	sinksEnabled := h.enableFirehose || h.enableKinesis || h.enableLambda
	storeEvents := h.eventStore != nil && len(eventsRequest.Events) > 0
	// Execution events, file access events and audit events are all forwarded, each record tagged with the
	// machine that reported it.
	var forwardedEvents []interface{}
	if sinksEnabled {
		forwardedEvents = convertRequestEventsToUploadEvents(machineID, eventsRequest)
	}
	if len(forwardedEvents) == 0 && !storeEvents {
		// Nothing to forward (sinks reject empty batches) and nothing to store.
		return response.APIResponse(http.StatusOK, map[string]string{"status": "ok"})
	}

	ctx, cancel := eventsink.WithSendDeadline(ctx)
	defer cancel()

	// The events table is written alongside the sinks, not after them, so both share the whole deadline.
	var storeErr error
	var stored sync.WaitGroup
	if storeEvents {
		stored.Add(1)
		go func() {
			defer stored.Done()
			storeErr = h.eventStore.Store(ctx, machineID, eventsRequest.Events)
		}()
	}

	if len(forwardedEvents) > 0 {
		if h.enableFirehose {
			err = sendToFirehose(ctx, h.firehoseClient, machineID, forwardedEvents)
		}

		if err == nil && h.enableKinesis {
			err = sendToKinesis(ctx, h.kinesisClient, machineID, forwardedEvents)
		}

		if err == nil && h.enableLambda {
			err = sendToLambda(ctx, h.lambdaClient, machineID, forwardedEvents)
		}
	}
	stored.Wait()

	if err != nil {
		return response.InternalServerError(fmt.Errorf("forwarding events: %w", err), response.ErrInternalServerErrorResponse)
	}
	if storeErr != nil {
		return response.InternalServerError(fmt.Errorf("storing events: %w", storeErr), response.ErrInternalServerErrorResponse)
	}

	return response.APIResponse(http.StatusOK, map[string]string{"status": "ok"})
}
