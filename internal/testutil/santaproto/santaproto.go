// Package santaproto loads the Santa sync protocol definition that ships in testdata and uses it to
// produce and parse sync payloads exactly the way a Santa client does.
//
// Tests should use this instead of Rudolph's own request/response structs, so that drift between Rudolph
// and the real protocol shows up as a failing test rather than being mirrored on both sides.
package santaproto

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"testing"

	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

const (
	// SantaVersion is the Santa release whose sync protocol is vendored in testdata.
	SantaVersion = "2026.8"
	protoFile    = "sync_v1.proto"
	packageName  = "santa.sync.v1"
)

var (
	loadOnce sync.Once
	loaded   protoreflect.FileDescriptor
	loadErr  error

	// The HTTP annotations are irrelevant to the payload format and would require vendoring googleapis.
	importAnnotations = regexp.MustCompile(`(?m)^import "google/api/annotations.proto";\s*$`)
	httpOptionBlock   = regexp.MustCompile(`(?s)\{\s*option \(google\.api\.http\) = \{.*?\};\s*\}`)
)

// TestdataDir returns the absolute path of testdata/santa-<version>.
func TestdataDir() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..", "testdata", "santa-"+SantaVersion)
}

func load() (protoreflect.FileDescriptor, error) {
	loadOnce.Do(func() {
		raw, err := os.ReadFile(filepath.Join(TestdataDir(), protoFile))
		if err != nil {
			loadErr = err
			return
		}
		src := importAnnotations.ReplaceAllString(string(raw), "")
		src = httpOptionBlock.ReplaceAllString(src, "{}")

		compiler := protocompile.Compiler{
			Resolver: &protocompile.SourceResolver{
				Accessor: protocompile.SourceAccessorFromMap(map[string]string{protoFile: src}),
			},
		}
		files, err := compiler.Compile(context.Background(), protoFile)
		if err != nil {
			loadErr = fmt.Errorf("compiling %s: %w", protoFile, err)
			return
		}
		loaded = files[0]
	})
	return loaded, loadErr
}

// Descriptor returns the message descriptor for a message in the santa.sync.v1 package, e.g. "PreflightRequest".
func Descriptor(t testing.TB, message string) protoreflect.MessageDescriptor {
	t.Helper()
	fd, err := load()
	if err != nil {
		t.Fatalf("loading Santa %s sync proto: %v", SantaVersion, err)
	}
	md := fd.Messages().ByName(protoreflect.Name(message))
	if md == nil {
		t.Fatalf("message %s.%s not found in Santa %s sync proto", packageName, message, SantaVersion)
	}
	return md
}

// ClientJSON builds a message from protojson text (proto field names or JSON names are both accepted) and
// serializes it the way Santa's client does: JSON names, enum names, default values omitted.
//
// Unknown fields are an error, so fixtures cannot drift from the protocol.
func ClientJSON(t testing.TB, message string, fields string) []byte {
	t.Helper()
	msg := dynamicpb.NewMessage(Descriptor(t, message))
	if err := protojson.Unmarshal([]byte(fields), msg); err != nil {
		t.Fatalf("fixture for %s is not valid against Santa %s protocol: %v", message, SantaVersion, err)
	}
	out, err := protojson.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal %s: %v", message, err)
	}
	return out
}

// ParseServerResponse parses a server response body the way Santa's client does (unknown fields ignored).
// A non-nil error means a real Santa client would fail this sync stage.
func ParseServerResponse(t testing.TB, message string, body []byte) (*dynamicpb.Message, error) {
	t.Helper()
	msg := dynamicpb.NewMessage(Descriptor(t, message))
	err := protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(body, msg)
	return msg, err
}

// MustParseServerResponse is ParseServerResponse that fails the test on error.
func MustParseServerResponse(t testing.TB, message string, body []byte) *dynamicpb.Message {
	t.Helper()
	msg, err := ParseServerResponse(t, message, body)
	if err != nil {
		t.Fatalf("a Santa %s client could not parse the %s: %v\nbody: %s", SantaVersion, message, err, body)
	}
	return msg
}

// Canonical re-serializes a parsed message using proto field names, which makes assertions readable.
func Canonical(t testing.TB, msg proto.Message) string {
	t.Helper()
	out, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// protojson output whitespace is intentionally unstable; normalize it.
	var buf bytes.Buffer
	if err := json.Compact(&buf, out); err != nil {
		t.Fatalf("compact: %v", err)
	}
	return buf.String()
}

// Deflate compresses a body the way Santa does by default (SyncClientContentEncoding unset => "deflate", zlib format).
func Deflate(t testing.TB, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zlib.NewWriter(&buf)
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Gzip compresses a body the way Santa does with SyncClientContentEncoding = gzip.
func Gzip(t testing.TB, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
