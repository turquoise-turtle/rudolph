package handlers

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/airbnb/rudolph/internal/handlers/eventupload"
	"github.com/airbnb/rudolph/internal/handlers/health"
	"github.com/airbnb/rudolph/internal/handlers/postflight"
	"github.com/airbnb/rudolph/internal/handlers/preflight"
	"github.com/airbnb/rudolph/internal/handlers/ruledownload"
	"github.com/airbnb/rudolph/internal/handlers/xsrf"
	"github.com/airbnb/rudolph/pkg/response"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-lambda-go/events"
)

var (
	handlers []HandlerInterface
)

func init() {
	handlers = newHandlers()
}

func newHandlers() []HandlerInterface {
	return []HandlerInterface{
		&health.GetHealthHandler{},
		&eventupload.PostEventuploadHandler{},
		&preflight.PostPreflightHandler{},
		&ruledownload.PostRuledownloadHandler{},
		&postflight.PostPostflightHandler{},
		&xsrf.PostXSRFHandler{},
	}
}

// ApiRouter dispatches an API Gateway request to the handler that serves it.
//
// Requests and responses are never logged: a request carries the fleet's bearer token in its headers and
// personal data (user names, paths, binaries) in its body, and a response carries every rule a host may see.
// Only what is needed to operate the API is logged: the route, the machine it is for, the status and how
// long it took. Handlers that answer 500 log the cause just before (see response.InternalServerError).
func ApiRouter(request events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, error) {
	return ApiRouterContext(context.Background(), request)
}

// ApiRouterContext is ApiRouter for a Lambda invocation: ctx carries the invocation's deadline, which handlers
// that call other services (eventupload) use to answer before the Lambda timeout.
func ApiRouterContext(ctx context.Context, request events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, error) {
	start := time.Now()

	response, err := getResponse(ctx, request)

	status := 0
	if response != nil {
		status = response.StatusCode
	}
	machine := loggedMachineID(request)
	if err != nil {
		log.Printf("%s %s machine=%s: handler error: %q", request.HTTPMethod, request.Resource, machine, err.Error())
	}
	log.Printf("%s %s machine=%s status=%d duration=%s",
		request.HTTPMethod, request.Resource, machine, status, time.Since(start))

	return response, err
}

// loggedMachineID is the machine ID to write to the request's log line. It comes from the request path, which the
// client chooses, so anything but a UUID is replaced: it could otherwise add text to the line or whole lines to the
// log, such as the "status=NNN" lines the CloudWatch alarms count.
func loggedMachineID(request events.APIGatewayProxyRequest) string {
	machineID, ok := request.PathParameters["machine_id"]
	if !ok {
		return "-"
	}
	if types.ValidateMachineID(machineID) != nil {
		return "invalid"
	}
	return machineID
}

func getResponse(ctx context.Context, request events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, error) {
	for _, h := range handlers {
		if h.Handles(request) {
			if err := h.Boot(); err != nil {
				return response.InternalServerError(fmt.Errorf("handler failed to start: %w", err), nil)
			}
			if hc, ok := h.(ContextHandler); ok {
				return hc.HandleContext(ctx, request)
			}
			return h.Handle(request)
		}
	}

	log.Printf("ERROR: ApiRouter failure: unrouteable request: [%+v] %+v", request.HTTPMethod, request.Resource)
	return response.APIResponse(http.StatusMethodNotAllowed, nil)
}

type HandlerInterface interface {
	Handles(request events.APIGatewayProxyRequest) bool
	Boot() error
	Handle(request events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, error)
}

// ContextHandler is a handler that can use the Lambda invocation's context (its deadline).
type ContextHandler interface {
	HandleContext(ctx context.Context, request events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, error)
}
