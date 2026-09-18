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

	return decompressBody(Header(request, "Content-Encoding"), body)
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
		// Santa uses zlib, which is what RFC 9110 calls "deflate"; some HTTP clients send a raw deflate
		// stream instead, so fall back to that rather than failing the sync.
		decompressed, err := readLimited(func() (io.ReadCloser, error) { return zlib.NewReader(bytes.NewReader(body)) })
		if err != nil && !errors.Is(err, ErrBodyTooLarge) {
			return readLimited(func() (io.ReadCloser, error) { return flate.NewReader(bytes.NewReader(body)), nil })
		}
		return decompressed, err
	case "gzip", "x-gzip":
		return readLimited(func() (io.ReadCloser, error) { return gzip.NewReader(bytes.NewReader(body)) })
	default:
		return nil, fmt.Errorf("unsupported Content-Encoding %q", encoding)
	}
}

// readLimited decompresses a stream, refusing to allocate more than MaxBodySize for it.
func readLimited(open func() (io.ReadCloser, error)) ([]byte, error) {
	reader, err := open()
	if err != nil {
		return nil, fmt.Errorf("request body could not be decompressed: %w", err)
	}
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

// normalizeProtoJSONKeys rewrites every lowerCamelCase object key in a JSON document to snake_case. Keys
// that are already snake_case are left alone, so both spellings of a protobuf JSON field are accepted.
func normalizeProtoJSONKeys(data []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	var document any
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, errors.New("unexpected data after the end of the JSON document")
	}

	normalized, changed := normalizeValue(document)
	if !changed {
		return data, nil
	}
	return json.Marshal(normalized)
}

func normalizeValue(value any) (any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		normalized := make(map[string]any, len(typed))
		changed := false
		for key, keyValue := range typed {
			normalizedValue, valueChanged := normalizeValue(keyValue)
			normalizedKey := snakeCase(key)
			changed = changed || valueChanged || normalizedKey != key
			normalized[normalizedKey] = normalizedValue
		}
		return normalized, changed
	case []any:
		changed := false
		for i, element := range typed {
			normalizedElement, elementChanged := normalizeValue(element)
			typed[i] = normalizedElement
			changed = changed || elementChanged
		}
		return typed, changed
	default:
		return value, false
	}
}

func snakeCase(key string) string {
	var builder strings.Builder
	for i := 0; i < len(key); i++ {
		character := key[i]
		if character < 'A' || character > 'Z' {
			if builder.Len() > 0 {
				builder.WriteByte(character)
			}
			continue
		}
		if builder.Len() == 0 {
			builder.WriteString(key[:i])
		}
		// A leading capital is not a word boundary: "Foo" is "foo", but "fooBar" is "foo_bar".
		if i > 0 {
			builder.WriteByte('_')
		}
		builder.WriteByte(character - 'A' + 'a')
	}
	if builder.Len() == 0 {
		return key
	}
	return builder.String()
}
