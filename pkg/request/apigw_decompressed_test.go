package request

// Found in the first production deployment (2026-11-09): API Gateway decompresses a request body that carries
// `Content-Encoding: deflate` or `gzip` before invoking the Lambda, but leaves the header in place. Decoding the
// already plain JSON again failed, so every Santa sync (deflate by default) was answered 400. A body whose header says
// compressed but which is not a compressed stream and is valid JSON is accepted as already decompressed; corrupt
// compressed data and oversized bodies are still refused.

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

const preflightJSON = `{"serial_num":"C02ABC123","hostname":"lab-mac-01","santa_version":"2026.9.0","client_mode":"MONITOR"}`

func apiRequest(encoding string, body []byte, base64Encoded bool) events.APIGatewayProxyRequest {
	r := events.APIGatewayProxyRequest{Headers: map[string]string{"Content-Type": "application/json"}}
	if encoding != "" {
		r.Headers["Content-Encoding"] = encoding
	}
	if base64Encoded {
		r.Body, r.IsBase64Encoded = base64.StdEncoding.EncodeToString(body), true
	} else {
		r.Body = string(body)
	}
	return r
}

func TestDecodeBody_AcceptsBodiesAPIGatewayAlreadyDecompressed(t *testing.T) {
	for _, enc := range []string{"deflate", "gzip", "x-gzip", " Deflate ", "GZIP"} {
		for _, b64 := range []bool{true, false} {
			for _, body := range []string{preflightJSON, "  \n" + preflightJSON, `[]`} {
				got, err := DecodeBody(apiRequest(enc, []byte(body), b64))
				if err != nil || string(got) != body {
					t.Errorf("Content-Encoding %q, base64 %v, body %.20q: %q, %v; want the body as it is", enc, b64, body, got, err)
				}
			}
		}
	}
}

func compress(t *testing.T, kind string, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	switch kind {
	case "zlib":
		w := zlib.NewWriter(&buf)
		w.Write(data)
		w.Close()
	case "raw":
		w, _ := flate.NewWriter(&buf, flate.DefaultCompression)
		w.Write(data)
		w.Close()
	case "gzip":
		w := gzip.NewWriter(&buf)
		w.Write(data)
		w.Close()
	}
	return buf.Bytes()
}

func TestDecodeBody_StillDecompressesCompressedBodies(t *testing.T) {
	for enc, kind := range map[string]string{"deflate": "zlib", "gzip": "gzip"} {
		got, err := DecodeBody(apiRequest(enc, compress(t, kind, []byte(preflightJSON)), true))
		if err != nil || string(got) != preflightJSON {
			t.Errorf("%s body: %q, %v", kind, got, err)
		}
	}
	// A raw deflate stream under "deflate", as some HTTP clients send.
	got, err := DecodeBody(apiRequest("deflate", compress(t, "raw", []byte(preflightJSON)), true))
	if err != nil || string(got) != preflightJSON {
		t.Errorf("raw deflate body: %q, %v", got, err)
	}
}

func TestDecodeBody_RejectsCorruptCompressedBodies(t *testing.T) {
	zl := compress(t, "zlib", []byte(strings.Repeat(preflightJSON, 50)))
	gz := compress(t, "gzip", []byte(strings.Repeat(preflightJSON, 50)))
	for name, c := range map[string]struct {
		enc  string
		body []byte
	}{
		"truncated zlib":        {"deflate", zl[:len(zl)/2]},
		"truncated gzip":        {"gzip", gz[:len(gz)/2]},
		"garbage under deflate": {"deflate", []byte("\x00\x01not json at all\xff")},
		"garbage under gzip":    {"gzip", []byte("not json {")},
	} {
		if got, err := DecodeBody(apiRequest(c.enc, c.body, true)); err == nil {
			t.Errorf("%s: accepted as %q", name, got)
		}
	}
}

func TestDecodeBody_SizeLimitAppliesToAlreadyDecompressedBodies(t *testing.T) {
	big := []byte(`{"pad":"` + strings.Repeat("x", MaxBodySize) + `"}`)
	if _, err := DecodeBody(apiRequest("deflate", big, true)); !errors.Is(err, ErrBodyTooLarge) {
		t.Errorf("an oversized plain body under Content-Encoding deflate: %v, want ErrBodyTooLarge", err)
	}
}
