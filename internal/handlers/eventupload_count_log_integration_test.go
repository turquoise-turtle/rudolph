//go:build integration

package handlers

// The first production deployment measures events per Mac per day (docs/smoke-test.md step 12.1, rudolph-web's
// docs/data-layer-design.md), so eventupload logs how many events each upload carried and how many it stored: counts
// only, never event contents.

import (
	"bytes"
	"log"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/airbnb/rudolph/pkg/types"
)

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func captureLog(t *testing.T) *lockedBuffer {
	t.Helper()
	buf := &lockedBuffer{}
	log.SetOutput(buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return buf
}

func TestEventUpload_LogsReceivedAndStoredCounts(t *testing.T) {
	transport := freshServer(t)
	addGlobalRule(t, strings.Repeat("e5", 32), types.RuleTypeBinary, types.RulePolicyBlocklist)
	h := newHost(t, machineA, transport)
	syncNormal(t, h)

	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	h.PendingEvents = []string{
		execEvent(sha1, "BLOCK_BINARY", 1, at, ""),
		execEvent(sha2, "ALLOW_UNKNOWN", 2, at, ""),
		execEvent(sha3, "ALLOW_BINARY", 3, at, ""), // an allowed execution no rule collects: not stored
	}
	buf := captureLog(t)
	h.MustSync(t)

	line := regexp.MustCompile(`eventupload: machine=` + machineA + ` received=(\d+) stored=(\d+)`).FindStringSubmatch(buf.String())
	if line == nil {
		t.Fatalf("no eventupload count line for %s in the log:\n%s", machineA, buf.String())
	}
	if line[1] != "3" || line[2] != "2" {
		t.Errorf("logged received=%s stored=%s, want received=3 stored=2", line[1], line[2])
	}
	for _, secret := range []string{sha1, sha2, sha3, "BLOCK_BINARY"} {
		if strings.Contains(buf.String(), secret) {
			t.Errorf("the log contains event content %q:\n%s", secret, buf.String())
		}
	}
}
