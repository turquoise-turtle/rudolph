//go:build integration

package handlers

// Top unknowns (owner, 2026-11-11): the day counter also counts the executions no rule decided, so the ranker can list
// them apart from those blocked by a rule. BLOCK_UNKNOWN adds to UnknownBlocked and ALLOW_UNKNOWN to UnknownAllowed,
// in the same counter update as Blocked and Allowed (same item, same transaction: no extra writes). Blocked and
// Allowed are unchanged. Like them, each attribute is absent until its first event, and a repeated upload counts once.

import (
	"testing"
	"time"
)

func TestTopUnknown_DayCounterCountsUnknownDecisions(t *testing.T) {
	transport := eventsServer(t)
	host := syncedHost(t, machineA, transport)
	at := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	day := at.UTC().Format("2006-01-02")

	events := []string{
		execEvent(sha1, "BLOCK_UNKNOWN", 1, at, ""),
		execEvent(sha1, "BLOCK_UNKNOWN", 2, at, ""),
		execEvent(sha1, "ALLOW_UNKNOWN", 3, at, ""),
		execEvent(sha1, "BLOCK_BINARY", 4, at, ""),
		execEvent(sha2, "BLOCK_BINARY", 5, at, ""),
	}
	host.PendingEvents = append([]string(nil), events...)
	host.MustSync(t)
	host.PendingEvents = append([]string(nil), events...) // Santa uploads again what it could not confirm
	host.MustSync(t)

	counter := eventsItem(t, "BinaryDay#"+day, sha1)
	for attr, want := range map[string]float64{"Blocked": 3, "Allowed": 1, "UnknownBlocked": 2, "UnknownAllowed": 1} {
		if n, _ := numAttr(t, counter, attr); n != want {
			t.Errorf("sha1 %s = %v, want %v (counter %v)", attr, n, want, counter)
		}
	}

	ruleOnly := eventsItem(t, "BinaryDay#"+day, sha2)
	if n, _ := numAttr(t, ruleOnly, "Blocked"); n != 1 {
		t.Errorf("sha2 Blocked = %v, want 1", n)
	}
	for _, attr := range []string{"UnknownBlocked", "UnknownAllowed"} {
		if _, ok := ruleOnly[attr]; ok {
			t.Errorf("sha2 (blocked by a rule only) has %s; want it absent", attr)
		}
	}
}
