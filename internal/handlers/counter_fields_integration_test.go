//go:build integration

package handlers

// Ranker memory (owner approved, 2026-11-22): a day counter keeps only display fields the ranker can use, so the
// counters stay small and the ranker never folds a 4 KB name. A counter takes its FileName, SigningID and TeamID from
// the first event of the upload (for that binary and day) whose value is usable: FileName and SigningID at most 255
// bytes, TeamID exactly 10 upper-case letters or digits. An unusable value is skipped, not cut; with none usable the
// counter has no such attribute. The event item itself still stores its fields cut to 4,096 bytes (1,024 for
// TeamID), as before.

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestCounterFields_OnlyUsableValuesAreKept(t *testing.T) {
	transport := eventsServer(t)
	host := syncedHost(t, machineA, transport)
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	day := at.UTC().Format("2006-01-02")
	ev := func(sha string, pid int, name, signing, team string) string {
		return fmt.Sprintf(`{"file_sha256":%q,"file_path":"/x","file_name":%q,"signing_id":%q,"team_id":%q,"decision":"BLOCK_UNKNOWN","pid":%d,"execution_time":%.3f}`,
			sha, name, signing, team, pid, float64(at.Add(time.Duration(pid)*time.Millisecond).UnixMilli())/1000)
	}
	longName := strings.Repeat("n", 256)
	longSigning := "EQHXZ8M8AV:" + strings.Repeat("s", 245)
	host.PendingEvents = []string{
		// sha1: the first event's values are unusable, the second's are used
		ev(sha1, 1, longName, longSigning, "EQHXZ8M8AVX"),
		ev(sha1, 2, "tool", "EQHXZ8M8AV:com.example.tool", "EQHXZ8M8AV"),
		// sha2: nothing usable
		ev(sha2, 3, longName, longSigning, "eqhxz8m8av"),
		// sha3: at the limits
		ev(sha3, 4, strings.Repeat("n", 255), "EQHXZ8M8AV:"+strings.Repeat("s", 244), "ABCDEFGHIJ"),
	}
	host.MustSync(t)

	for _, c := range []struct {
		sha, attr, want string
	}{
		{sha1, "FileName", "tool"}, {sha1, "SigningID", "EQHXZ8M8AV:com.example.tool"}, {sha1, "TeamID", "EQHXZ8M8AV"},
		{sha2, "FileName", ""}, {sha2, "SigningID", ""}, {sha2, "TeamID", ""},
		{sha3, "FileName", strings.Repeat("n", 255)}, {sha3, "SigningID", "EQHXZ8M8AV:" + strings.Repeat("s", 244)}, {sha3, "TeamID", "ABCDEFGHIJ"},
	} {
		counter := eventsItem(t, "BinaryDay#"+day, c.sha)
		if len(counter) == 0 {
			t.Fatalf("no day counter for %s", c.sha[:12])
		}
		if got, _ := sensorString(t, counter, c.attr); got != c.want {
			t.Errorf("counter %s %s = %.40q (%d bytes), want %.40q", c.sha[:12], c.attr, got, len(got), c.want)
		}
	}
	// The events themselves keep their fields as before.
	if got, _ := sensorString(t, eventsItem(t, "Exec#"+machineA, tsOf(at.Add(3*time.Millisecond))+"#"+sha2+"#3"), "FileName"); got != longName {
		t.Errorf("the event's FileName was changed: %d bytes, want the 256 uploaded", len(got))
	}
}
