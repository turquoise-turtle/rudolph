package request

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/airbnb/rudolph/pkg/response"
	"github.com/aws/aws-lambda-go/events"
)

// MaxBodySize is the largest request body any sync handler accepts, measured after base64 decoding and
// decompression. Santa's sync payloads are far smaller than this; the limit exists so that a small
// compressed body cannot make a Lambda allocate an unbounded amount of memory.
const MaxBodySize = 6 << 20 // 6 MiB

// ErrBodyTooLarge is returned when a request body exceeds MaxBodySize, before or after decompression.
var ErrBodyTooLarge = errors.New("request body is larger than the maximum accepted size")

// DecodeBody returns the bytes of a request body as the client sent them.
//
// API Gateway hands binary media types to Lambda base64 encoded, and Santa compresses request bodies with
// `Content-Encoding: deflate` (zlib) by default, or with `gzip` when `SyncClientContentEncoding` says so.
// Both are undone here, so handlers only ever see plain bytes.
func DecodeBody(request events.APIGatewayProxyRequest) ([]byte, error) {
	body := []byte(request.Body)
	if request.IsBase64Encoded {
		decoded, err := base64.StdEncoding.DecodeString(request.Body)
		if err != nil {
			return nil, fmt.Errorf("request body is not valid base64: %w", err)
		}
		body = decoded
	}

	encoding := Header(request, "Content-Encoding")
	if alreadyDecompressed(encoding, body) {
		// API Gateway (REST API) decompresses a body sent with Content-Encoding deflate or gzip before invoking the
		// Lambda, but passes the header on unchanged (found in the first production deployment): the body is
		// already the plain JSON. Checked before decompressing, because a raw deflate decoder fed plain bytes may not
		// fail. Corrupt compressed data is not valid JSON and still goes to decompressBody, which refuses it.
		if len(body) > MaxBodySize {
			return nil, ErrBodyTooLarge
		}
		return body, nil
	}
	return decompressBody(encoding, body)
}

// alreadyDecompressed reports whether a body whose header claims a compression encoding is in fact plain JSON. Every
// sync request is JSON; a zlib, gzip or raw deflate stream is practically never valid JSON.
func alreadyDecompressed(contentEncoding string, body []byte) bool {
	switch strings.ToLower(strings.TrimSpace(contentEncoding)) {
	case "deflate", "gzip", "x-gzip":
		return json.Valid(body)
	}
	return false
}

// GetBody is DecodeBody with the API response to return when the body cannot be decoded.
func GetBody(request events.APIGatewayProxyRequest) (body []byte, errorResponse *events.APIGatewayProxyResponse, err error) {
	body, decodeErr := DecodeBody(request)
	if decodeErr != nil {
		body = nil
		if errors.Is(decodeErr, ErrBodyTooLarge) {
			errorResponse, err = response.APIResponse(http.StatusRequestEntityTooLarge, response.ErrBodyTooLargeResponse)
			return
		}
		errorResponse, err = response.APIResponse(http.StatusBadRequest, response.ErrInvalidBodyResponse)
		return
	}
	return
}

// Header returns a request header, whatever case API Gateway used for its name.
func Header(request events.APIGatewayProxyRequest, name string) string {
	for header, value := range request.Headers {
		if strings.EqualFold(header, name) {
			return value
		}
	}
	return ""
}

// UnmarshalSantaJSON parses a request body sent by a Santa client into v.
//
// Santa serializes requests as protobuf JSON: a field that has a `json_name` in sync_v1.proto is sent under
// that name (`serial_num`, `file_sha256`), and a field without one is sent lowerCamelCased (`sipStatus`,
// `csFlags`, `syncType`). Rudolph's request types are tagged with the snake_case spelling throughout, so
// camelCase keys are translated before unmarshalling and both spellings are accepted.
func UnmarshalSantaJSON(data []byte, v any) error {
	normalized, err := normalizeProtoJSONKeys(data)
	if err != nil {
		return err
	}
	return json.Unmarshal(normalized, v)
}

func decompressBody(contentEncoding string, body []byte) ([]byte, error) {
	switch encoding := strings.ToLower(strings.TrimSpace(contentEncoding)); encoding {
	case "", "identity":
		if len(body) > MaxBodySize {
			return nil, ErrBodyTooLarge
		}
		return body, nil
	case "deflate":
		// Santa uses zlib, which is what RFC 9110 calls "deflate". Only when the body is not a zlib stream at
		// all do we try a raw deflate stream, which is what some other HTTP clients send under this name; a
		// zlib stream that is truncated or corrupt is reported as such instead of being decompressed twice.
		reader, err := zlib.NewReader(bytes.NewReader(body))
		if err != nil {
			return readLimited(flate.NewReader(bytes.NewReader(body)))
		}
		return readLimited(reader)
	case "gzip", "x-gzip":
		reader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("request body could not be decompressed: %w", err)
		}
		return readLimited(reader)
	default:
		return nil, fmt.Errorf("unsupported Content-Encoding %q", encoding)
	}
}

// readLimited decompresses a stream, refusing to allocate more than MaxBodySize for it.
func readLimited(reader io.ReadCloser) ([]byte, error) {
	defer reader.Close()

	decompressed, err := io.ReadAll(io.LimitReader(reader, MaxBodySize+1))
	if err != nil {
		return nil, fmt.Errorf("request body could not be decompressed: %w", err)
	}
	if len(decompressed) > MaxBodySize {
		return nil, ErrBodyTooLarge
	}
	return decompressed, nil
}
