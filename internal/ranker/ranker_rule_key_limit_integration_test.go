//go:build integration

package ranker_test

// Review finding R1 (path traversal review): the day counter is shared by the whole fleet, and its SigningID and
// TeamID come from whichever Mac first uploaded the binary that day, cut only to 4,096 and 1,024 bytes. The ranker
// looks up a global rule for each of the first rows of the unknown lists by GetItem on "SigningID#<signing ID>" and
// "TeamID#<team ID>", and DynamoDB refuses a sort key over 1,024 bytes. One Mac uploading BLOCK_UNKNOWN executions
// with a 1,015-byte signing ID (or a 1,018-byte team ID) must not stop every ranker run, and so every list, while the
// counter is in a window: an identifier too long to be a rule's sort key can have no global rule, so the row simply
// has none.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/ranker"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
)

const maxRuleSortKeyBytes = 1024

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

func TestRanker_IdentifierTooLongForARuleKeyDoesNotStopTheRun_R1(t *testing.T) {
	signingPrefix, teamPrefix := "SigningID#", "TeamID#"
	for _, c := range []struct {
		name              string
		signingID, teamID string
	}{
		{"signing ID one byte over", "TEAM1:" + strings.Repeat("s", maxRuleSortKeyBytes-len(signingPrefix)+1-len("TEAM1:")), "TEAM1"},
		{"signing ID as cut by the writer", strings.Repeat("s", 4096), "TEAM1"},
		{"team ID one byte over", "", strings.Repeat("T", maxRuleSortKeyBytes-len(teamPrefix)+1)},
		{"team ID as cut by the writer", "", strings.Repeat("T", 1024)},
	} {
		t.Run(c.name, func(t *testing.T) {
			ddblocal.Reset(t)
			identifierCounter(t, 1, c.signingID, c.teamID)
			marker(t, 1, "MAC-A", 0)
			// Another binary whose global rule must still be found: the long identifier leaves only its own row
			// without a rule.
			identifierCounter(t, 2, "TEAM2:bin2", "TEAM2")
			marker(t, 2, "MAC-B", 0)
			storeRule(t, "GlobalRules", "SigningID#TEAM2:bin2")

			if err := runStoreErr(t); err != nil {
				t.Fatalf("one counter with a %d-byte signing ID and a %d-byte team ID stopped the ranker run: %v",
					len(c.signingID), len(c.teamID), err)
			}
			for _, kind := range []string{"Unknown", "UnknownBlocked", "Blocked"} {
				rows := unknownList(t, kind, 1)
				if got, ok := ruleOf(rows, 1); !ok || got != "" {
					t.Errorf("%s 1 day: binary 1 present %v with GlobalRule %q, want present without one", kind, ok, got)
				}
			}
			if got, _ := ruleOf(unknownList(t, "Unknown", 7), 2); got != "SigningID#TEAM2:bin2" {
				t.Errorf("Unknown 7 days: binary 2 has GlobalRule %q, want SigningID#TEAM2:bin2", got)
			}
		})
	}
}

// The limit is DynamoDB's, so an identifier whose rule key is exactly 1,024 bytes is a real rule and must still be
// matched; skipping anything shorter than that would hide a rule an admin added.
func TestRanker_IdentifierAtTheRuleKeyLimitStillFindsItsRule_R1(t *testing.T) {
	ddblocal.Reset(t)
	signingID := "TEAM1:" + strings.Repeat("s", maxRuleSortKeyBytes-len("SigningID#")-len("TEAM1:"))
	teamID := strings.Repeat("T", maxRuleSortKeyBytes-len("TeamID#"))
	if n := len("SigningID#" + signingID); n != maxRuleSortKeyBytes {
		t.Fatalf("fixture: signing ID rule key is %d bytes", n)
	}
	identifierCounter(t, 1, signingID, "TEAM1")
	identifierCounter(t, 2, "TEAM2:bin2", teamID)
	marker(t, 1, "MAC-A", 0)
	marker(t, 2, "MAC-A", 0)
	storeRule(t, "GlobalRules", "SigningID#"+signingID)
	storeRule(t, "GlobalRules", "TeamID#"+teamID)

	if err := runStoreErr(t); err != nil {
		t.Fatalf("ranker: %v", err)
	}
	rows := unknownList(t, "Unknown", 1)
	if got, _ := ruleOf(rows, 1); got != "SigningID#"+signingID {
		t.Errorf("binary 1 has GlobalRule of %d bytes, want its 1,024-byte signing ID rule", len(got))
	}
	if got, _ := ruleOf(rows, 2); got != "TeamID#"+teamID {
		t.Errorf("binary 2 has GlobalRule of %d bytes, want its 1,024-byte team ID rule", len(got))
	}
}
