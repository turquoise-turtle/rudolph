//go:build integration

package rules

// From the stored-XSS review (2026-11-19): the rules listing prints a CEL rule's expression with Go's %q, which leaves
// variation selectors, the combining grapheme joiner, Khmer inherent vowels, Hangul fillers and the blank braille
// pattern raw (strconv.IsPrint accepts them). An expression imported from a file or written by another tool could hide
// a payload the admin never sees, so each must be shown as a visible escape.

import (
	"fmt"
	"strings"
	"testing"
	"unicode"

	"github.com/airbnb/rudolph/internal/testutil/clitest"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	modelrules "github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/types"
)

func TestRulesList_EscapesHiddenCharactersInACELExpression(t *testing.T) {
	ddblocal.Reset(t)
	// Built from their numbers: written literally they would be invisible here too.
	hiddenRunes := []rune{0xfe00, 0xfe0e, 0xe0100, 0xe01ef, 0x034f, 0x17b4, 0x115f, 0x3164, 0x2800}
	var raw, escapes strings.Builder
	for _, r := range hiddenRunes {
		raw.WriteRune(r)
		if r > 0xffff {
			fmt.Fprintf(&escapes, `\U%08x`, r)
		} else {
			fmt.Fprintf(&escapes, `\u%04x`, r)
		}
	}
	expr := "target.signing_time >= timestamp('2025-01-01T00:00:00Z')" + raw.String()
	if err := globalrules.AddNewGlobalRule(clock.ConcreteTimeProvider{}, ddblocal.Client(t), qolSHA(1), types.RuleTypeBinary,
		types.RulePolicyCEL, "cel", modelrules.WithCELExpr(expr)); err != nil {
		t.Fatal(err)
	}
	out, err := clitest.Run(t, qolEnv, RulesCmd, "", "--global")
	if err != nil {
		t.Fatalf("rules --global: %v\n%s", err, out)
	}
	for _, r := range out {
		if r == 0x3164 || r == 0x115f || r == 0x2800 ||
			unicode.In(r, unicode.Cf, unicode.Other_Default_Ignorable_Code_Point, unicode.Variation_Selector) {
			t.Errorf("rules --global writes U+%04X raw:\n%+q", r, out)
			break
		}
	}
	if !strings.Contains(out, "('2025-01-01T00:00:00Z')"+escapes.String()) {
		t.Errorf("rules --global does not show the expression's hidden characters as %+q:\n%+q", escapes.String(), out)
	}
}
