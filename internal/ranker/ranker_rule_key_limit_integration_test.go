//go:build integration

package ranker_test

// The day counter is shared by the whole fleet, and its SigningID and TeamID come from whichever Mac first uploaded
// the binary that day. The ranker looks up a global rule for each of the first rows of the unknown lists by GetItem on
// "SigningID#<signing ID>" and "TeamID#<team ID>". Review finding R1 (path traversal review) made a key over
// DynamoDB's 1,024 bytes harmless; since the ranker memory fix (2026-11-22) the ranker drops a signing ID over 255
// bytes and a team ID that is not exactly 10 upper-case letters or digits as it reads the counter, so no lookup is
// made with one. One Mac uploading BLOCK_UNKNOWN executions with such identifiers must not stop the run or hide other
// binaries' rules, a binary's other identifiers must still find its rule, and an identifier at the limit (counted in
// bytes of UTF-8, not characters) is a real one and must still find its rule.

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/airbnb/rudolph/internal/ranker"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
)

const maxSigningIDBytes = 255

// signingAt returns a signing ID of team TEAM000001 exactly n bytes long.
func signingAt(n int) string {
	prefix := teamN(1) + ":"
	return prefix + strings.Repeat("s", n-len(prefix))
}

// identifierCounter writes today's counter of binary i, all of whose executions were blocked as unknown.
func identifierCounter(t *testing.T, i int, signingID, teamID string) {
	t.Helper()
	item := map[string]any{"PK": "BinaryDay#" + day(0), "SK": sha(i),
		"Blocked": 5, "Allowed": 0, "UnknownBlocked": 5, "UnknownAllowed": 0, "FileName": fmt.Sprintf("bin%d", i)}
	if signingID != "" {
		item["SigningID"] = signingID
	}
	if teamID != "" {
		item["TeamID"] = teamID
	}
	put(t, item)
}

func runStoreErr(t *testing.T) error {
	t.Helper()
	return ranker.Run(ctx, ranker.Options{Client: ddblocal.Raw(t), Table: ddblocal.EventsTableName, StoreTable: ddblocal.TableName,
		Now: func() time.Time { return now }})
}

func ruleOf(rows []urow, i int) (string, bool) {
	for _, r := range rows {
		if r.SHA == sha(i) {
			return r.GlobalRule, true
		}
	}
	return "", false
}

func TestRanker_UnusableIdentifierDoesNotStopTheRun(t *testing.T) {
	for _, c := range []struct {
		name              string
		signingID, teamID string
	}{
		{"signing ID one byte over", signingAt(maxSigningIDBytes + 1), teamN(1)},
		{"signing ID as cut by the writer", strings.Repeat("s", 4096), teamN(1)},
		{"team ID one character over", "", teamN(1) + "X"},
		{"team ID as cut by the writer", "", strings.Repeat("T", 1024)},
	} {
		t.Run(c.name, func(t *testing.T) {
			ddblocal.Reset(t)
			identifierCounter(t, 1, c.signingID, c.teamID)
			marker(t, 1, "MAC-A", 0)
			// Another binary whose global rule must still be found: the unusable identifier leaves only its own row
			// without a rule.
			identifierCounter(t, 2, teamN(2)+":bin2", teamN(2))
			marker(t, 2, "MAC-B", 0)
			storeRule(t, "GlobalRules", "SigningID#"+teamN(2)+":bin2")

			if err := runStoreErr(t); err != nil {
				t.Fatalf("one counter with a %d-byte signing ID and a %d-byte team ID stopped the ranker run: %v",
					len(c.signingID), len(c.teamID), truncate(err.Error()))
			}
			for _, kind := range []string{"Unknown", "UnknownBlocked", "Blocked"} {
				rows := unknownList(t, kind, 1)
				if got, ok := ruleOf(rows, 1); !ok || got != "" {
					t.Errorf("%s 1 day: binary 1 present %v with GlobalRule %q, want present without one", kind, ok, truncate(got))
				}
			}
			if got, _ := ruleOf(unknownList(t, "Unknown", 7), 2); got != "SigningID#"+teamN(2)+":bin2" {
				t.Errorf("Unknown 7 days: binary 2 has GlobalRule %q, want SigningID#%s:bin2", got, teamN(2))
			}
		})
	}
}

// An identifier at the limit is a real one, and its rule must still be found.
func TestRanker_IdentifierAtTheLimitStillFindsItsRule(t *testing.T) {
	ddblocal.Reset(t)
	signingID := signingAt(maxSigningIDBytes)
	identifierCounter(t, 1, signingID, teamN(1))
	identifierCounter(t, 2, "", "ABCDEFGHI9")
	marker(t, 1, "MAC-A", 0)
	marker(t, 2, "MAC-A", 0)
	storeRule(t, "GlobalRules", "SigningID#"+signingID)
	storeRule(t, "GlobalRules", "TeamID#ABCDEFGHI9")

	if err := runStoreErr(t); err != nil {
		t.Fatalf("ranker: %v", truncate(err.Error()))
	}
	rows := unknownList(t, "Unknown", 1)
	if got, _ := ruleOf(rows, 1); got != "SigningID#"+signingID {
		t.Errorf("binary 1 has GlobalRule of %d bytes, want its 255-byte signing ID's rule", len(got))
	}
	if got, _ := ruleOf(rows, 2); got != "TeamID#ABCDEFGHI9" {
		t.Errorf("binary 2 has GlobalRule %q, want TeamID#ABCDEFGHI9", got)
	}
}

// Review finding 3: an unusable identifier has no rule, but the binary's other identifiers may. The lookup must go on
// to the next, less specific candidate rather than give up on the row, or a long signing ID would hide the team ID
// rule that covers the binary.
func TestRanker_UnusableIdentifierFallsThroughToTheNextRule(t *testing.T) {
	for _, c := range []struct {
		name, signingID, teamID, rule string
	}{
		{"signing ID too long, team ID rule", signingAt(maxSigningIDBytes + 1), teamN(1), "TeamID#" + teamN(1)},
		{"team ID not 10 characters, binary rule", teamN(1) + ":bin1", strings.Repeat("T", 1024), "Binary#" + sha(1)},
	} {
		t.Run(c.name, func(t *testing.T) {
			ddblocal.Reset(t)
			identifierCounter(t, 1, c.signingID, c.teamID)
			marker(t, 1, "MAC-A", 0)
			storeRule(t, "GlobalRules", c.rule)

			if err := runStoreErr(t); err != nil {
				t.Fatalf("ranker: %v", truncate(err.Error()))
			}
			if got, _ := ruleOf(unknownList(t, "Unknown", 1), 1); got != c.rule {
				t.Errorf("binary 1 has GlobalRule %q, want %s", truncate(got), c.rule)
			}
		})
	}
}

// Review finding 4: the limit is in bytes of UTF-8, not characters. A signing ID of 2- or 3-byte characters exactly
// 255 bytes long must find its rule; one more character takes it over the limit while it is still under 255
// characters, and that row must simply have no rule.
func TestRanker_MultiByteIdentifierAtTheLimit(t *testing.T) {
	prefix := teamN(1) + ":"
	room := maxSigningIDBytes - len(prefix)
	for _, c := range []struct {
		name, char string
	}{
		{"2-byte characters", "\u00e9"},
		{"3-byte characters", "\u4e2d"},
	} {
		fill := room / len(c.char)
		atLimit := prefix + strings.Repeat(c.char, fill) + strings.Repeat("s", room-fill*len(c.char))
		over := prefix + strings.Repeat(c.char, fill+1)
		t.Run(c.name+" at the limit", func(t *testing.T) {
			if n := len(atLimit); n != maxSigningIDBytes {
				t.Fatalf("fixture: signing ID is %d bytes", n)
			}
			ddblocal.Reset(t)
			identifierCounter(t, 1, atLimit, teamN(1))
			marker(t, 1, "MAC-A", 0)
			storeRule(t, "GlobalRules", "SigningID#"+atLimit)

			if err := runStoreErr(t); err != nil {
				t.Fatalf("ranker: %v", truncate(err.Error()))
			}
			if got, _ := ruleOf(unknownList(t, "Unknown", 1), 1); got != "SigningID#"+atLimit {
				t.Errorf("binary 1 has GlobalRule of %d bytes, want its 255-byte signing ID's rule", len(got))
			}
		})
		t.Run(c.name+" one character over", func(t *testing.T) {
			if n, runes := len(over), utf8.RuneCountInString(over); n <= maxSigningIDBytes || runes >= maxSigningIDBytes {
				t.Fatalf("fixture: signing ID is %d bytes and %d characters", n, runes)
			}
			ddblocal.Reset(t)
			identifierCounter(t, 1, over, "")
			marker(t, 1, "MAC-A", 0)
			identifierCounter(t, 2, teamN(2)+":bin2", teamN(2))
			marker(t, 2, "MAC-B", 0)
			storeRule(t, "GlobalRules", "SigningID#"+teamN(2)+":bin2")
			storeRule(t, "GlobalRules", "SigningID#"+over)

			if err := runStoreErr(t); err != nil {
				t.Fatalf("a %d-byte, %d-character signing ID stopped the ranker run: %v", len(over), utf8.RuneCountInString(over), truncate(err.Error()))
			}
			rows := unknownList(t, "Unknown", 1)
			if got, ok := ruleOf(rows, 1); !ok || got != "" {
				t.Errorf("binary 1 present %v with GlobalRule of %d bytes, want present without one", ok, len(got))
			}
			if got, _ := ruleOf(rows, 2); got != "SigningID#"+teamN(2)+":bin2" {
				t.Errorf("binary 2 has GlobalRule %q, want SigningID#%s:bin2", got, teamN(2))
			}
		})
	}
}

func truncate(s string) string {
	if len(s) > 120 {
		return s[:80] + fmt.Sprintf("... (%d bytes)", len(s))
	}
	return s
}
