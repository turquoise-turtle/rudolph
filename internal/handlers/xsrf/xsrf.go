package xsrf

import (
	"net/http"

	apirequest "github.com/airbnb/rudolph/pkg/request"
	"github.com/airbnb/rudolph/pkg/response"
	"github.com/aws/aws-lambda-go/events"
)

// XSRFHandler handles requests to the /xsrf and /xsrf/{machine_id} API endpoints
type PostXSRFHandler struct {
}

func (h *PostXSRFHandler) Boot() (err error) {
	return
}

func (h *PostXSRFHandler) Handles(request events.APIGatewayProxyRequest) bool {
	return request.Resource == "/xsrf/{machine_id}" && request.HTTPMethod == "POST"
}

func (h *PostXSRFHandler) Handle(request events.APIGatewayProxyRequest) (*events.APIGatewayProxyResponse, error) {
	// Like every other sync stage, refuse a machine ID that is not a canonical UUID
	_, errorResponse, err := apirequest.GetMachineID(request)
	if errorResponse != nil || err != nil {
		return errorResponse, err
	}

	// The body is not used, but a request Rudolph cannot decode (bad base64 or compression) is still a bad
	// request rather than an "ok".
	if _, errorResponse, err = apirequest.GetBody(request); errorResponse != nil || err != nil {
		return errorResponse, err
	}

	// FIXME (derek.wang) just returning stub for now
	return response.APIResponse(http.StatusOK, map[string]string{"status": "ok"})
}
