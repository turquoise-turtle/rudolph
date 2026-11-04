package response

import (
	"log"
	"net/http"

	"github.com/aws/aws-lambda-go/events"
)

// InternalServerError answers HTTP 500 with a generic body and logs why.
//
// The cause is logged, not sent: AWS error messages name tables, ARNs and keys. It is logged quoted, on one line, so
// that no error text can add log lines of its own, such as the "status=NNN" lines the CloudWatch alarms count. The
// router logs the request's status line right after it.
//
// The error is not returned to Lambda either: API Gateway turns a Lambda error into a 502 without a body, which Santa
// cannot tell apart from a dropped connection.
//
// Do not pass errors that embed request or response values (json.UnsupportedValueError, for example); log a
// description instead and pass nil.
func InternalServerError(cause error) (*events.APIGatewayProxyResponse, error) {
	if cause != nil {
		log.Printf("internal server error: %q", cause.Error())
	}
	return APIResponse(http.StatusInternalServerError, ErrInternalServerErrorResponse)
}
