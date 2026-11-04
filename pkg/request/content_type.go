package request

import (
	"mime"
	"strings"

	"github.com/aws/aws-lambda-go/events"
)

// HasJSONBody reports whether the request's Content-Type is application/json. The header name and the media type are
// matched case-insensitively (RFC 9110) and parameters such as charset are allowed, so "content-type:
// Application/JSON; charset=UTF-8" is accepted; any other media type, or none, is not.
func HasJSONBody(request events.APIGatewayProxyRequest) bool {
	value := strings.TrimSpace(Header(request, "Content-Type"))
	if value == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil && err != mime.ErrInvalidMediaParameter {
		// A malformed parameter does not change the media type, which ParseMediaType still returns
		return false
	}
	// ParseMediaType lower-cases the media type
	return mediaType == "application/json"
}
