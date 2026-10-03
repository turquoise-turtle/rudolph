package lambda

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/airbnb/rudolph/pkg/eventsink"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	awsTypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
)

// maxAsyncPayloadBytes is the payload limit of an asynchronous (Event) invocation. AWS has raised it to 1 MB in
// some regions; 256 KB is accepted everywhere.
const maxAsyncPayloadBytes = 256 * 1024

type InvokeAPI interface {
	Send(ctx context.Context, machineID string, events LambdaEvents) error
}

type invokeAPI interface {
	Invoke(ctx context.Context, params *lambda.InvokeInput, optFns ...func(*lambda.Options)) (*lambda.InvokeOutput, error)
}

func (c *client) Send(
	ctx context.Context,
	machineID string,
	events LambdaEvents,
) error {
	// Every invocation (and the SDK's retries of it) is bounded by ctx's deadline, or eventsink.DefaultBudget if
	// the caller has none.
	ctx, cancel := eventsink.EnsureDeadline(ctx)
	defer cancel()
	return invokeLambda(
		ctx,
		c.lambdaClient,
		c.functionName,
		c.functionQualifier,
		machineID,
		events,
	)
}

// payload is LambdaEvents with its items already encoded.
type payload struct {
	Source string            `json:"source"`
	Items  []json.RawMessage `json:"items"`
}

// invokeLambda invokes the function asynchronously with the events, split over as many invocations as the async
// payload limit requires (none for no events); every invocation carries the source and the events keep their
// order. An event too large for any invocation is not sent and reported with an *eventsink.UnstorableError (alone,
// if everything else was delivered). A failed invocation (after the SDK's retries) is reported with another error.
func invokeLambda(
	ctx context.Context,
	api invokeAPI,
	functionName string,
	functionQualifier string,
	machineID string,
	events LambdaEvents,
) error {
	empty, err := json.Marshal(payload{Source: events.Source, Items: []json.RawMessage{}})
	if err != nil {
		return fmt.Errorf("failed json marshall lambda payload events: %w", err)
	}

	var items []json.RawMessage
	var sizes []int
	unmarshalable := 0
	for _, event := range events.Items {
		item, err := json.Marshal(event)
		if err != nil {
			unmarshalable++
			continue
		}
		items = append(items, item)
		sizes = append(sizes, len(item))
	}

	requests, tooLarge := eventsink.Split(sizes, eventsink.Limits{
		MaxRecords:        math.MaxInt, // only the payload size is limited
		MaxRequestBytes:   maxAsyncPayloadBytes,
		RequestOverhead:   len(empty),
		PerRecordOverhead: 1, // the comma between items
	})

	var failures []error
	for _, request := range requests {
		if err := ctx.Err(); err != nil {
			failures = append(failures, fmt.Errorf("lambda: %d event(s) not delivered: %w", len(request), err))
			break
		}
		p := payload{Source: events.Source, Items: make([]json.RawMessage, len(request))}
		for j, i := range request {
			p.Items[j] = items[i]
		}
		body, err := json.Marshal(p)
		if err != nil {
			failures = append(failures, fmt.Errorf("failed json marshall lambda payload events: %w", err))
			continue
		}
		_, err = api.Invoke(ctx, &lambda.InvokeInput{
			FunctionName:   aws.String(functionName),
			Qualifier:      aws.String(functionQualifier),
			InvocationType: awsTypes.InvocationTypeEvent,
			LogType:        awsTypes.LogTypeNone,
			Payload:        body,
		})
		if err != nil {
			// Refused even after the SDK's retries (throttled or failing). The send fails either way, so do not
			// hold the caller for the remaining invocations.
			failures = append(failures, fmt.Errorf("lambda:InvokeFunction call failed: %w", err))
			break
		}
	}

	var unstorable *eventsink.UnstorableError
	if dropped := len(tooLarge) + unmarshalable; dropped > 0 {
		unstorable = &eventsink.UnstorableError{Sink: "lambda", Dropped: dropped, Reason: fmt.Sprintf(
			"%d above the %d byte async payload limit, %d not encodable as JSON", len(tooLarge), maxAsyncPayloadBytes, unmarshalable)}
	}
	return eventsink.Outcome(failures, unstorable)
}
