package request

// Input handling review: every sync route (preflight, eventupload, ruledownload, postflight) passes its body through
// UnmarshalSantaJSON before anything else looks at it, so the time it takes per byte of body is paid by whoever holds
// the fleet's sync token.

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// wideCamelCaseObject is a body whose one nested object has n distinct lowerCamelCase keys and no snake_case key, so
// no field is spelled both ways.
func wideCamelCaseObject(n int) []byte {
	var b strings.Builder
	b.WriteString(`{"events":[],"padding":{`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"f%dX":0`, i)
	}
	b.WriteString(`}}`)
	return []byte(b.String())
}

// An object with many camelCase keys must not cost time quadratic in the number of keys. hasCollision compares every
// camelCase key of an object with every key of that object, so a body of a few hundred KB (about a kilobyte once
// compressed, which API Gateway undoes before the Lambda) keeps a handler busy until its 10 second timeout.
//
// The budget is absolute but generous: a single pass over this body (validate, survey, rewrite, unmarshal) takes
// about a millisecond, well under a twentieth of the budget even with the race detector on, while the quadratic
// comparison takes about a second on a fast laptop and far longer on a 256 MB Lambda. The best of three runs is
// used, so a pause of the test process cannot fail a linear implementation.
func TestUnmarshalSantaJSON_ManyCamelCaseKeysTakeLinearTime(t *testing.T) {
	const keys = 20_000
	const budget = 100 * time.Millisecond
	body := wideCamelCaseObject(keys)

	best := time.Duration(1<<63 - 1)
	for run := 0; run < 3; run++ {
		var parsed struct {
			Events []struct{} `json:"events"`
		}
		start := time.Now()
		err := UnmarshalSantaJSON(body, &parsed)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatalf("a valid body was refused: %v", err)
		}
		best = min(best, elapsed)
		if best <= budget {
			return
		}
	}
	t.Errorf("parsing a %d byte body with %d camelCase keys in one object took %s, want under %s: the time grows with the square of the number of keys",
		len(body), keys, best, budget)
}

// The same body four times as wide must take about four times as long, not sixteen. This is the scaling form of the
// test above and does not depend on how fast the machine is: both sizes are timed on the same machine, best of
// three, and the allowed ratio (10) sits between linear (4) and quadratic (16) with room on both sides. The smaller
// size is large enough (tens of milliseconds when quadratic, about a millisecond when linear) that timer resolution
// does not matter.
func TestNormalizeProtoJSONKeys_TimeScalesLinearlyWithKeys(t *testing.T) {
	timeFor := func(keys int) time.Duration {
		body := wideCamelCaseObject(keys)
		best := time.Duration(1<<63 - 1)
		for run := 0; run < 3; run++ {
			start := time.Now()
			if _, err := normalizeProtoJSONKeys(body); err != nil {
				t.Fatalf("a valid body was refused: %v", err)
			}
			best = min(best, time.Since(start))
		}
		return best
	}
	small, large := timeFor(5_000), timeFor(20_000)
	if ratio := float64(large) / float64(small); ratio > 10 {
		t.Errorf("4 times the keys took %.1f times as long (%s for 5,000 keys, %s for 20,000), want about 4: normalising keys is quadratic in the keys of one object",
			ratio, small, large)
	}
}

// Deeply nested bodies are walked recursively by both passes. encoding/json refuses more than 10,000 levels, which
// bounds the recursion; this pins that a body at the limit is handled and one past it is refused, not crashed on.
// Added because nothing else covers the recursion's bound, which the normaliser relies on without enforcing itself.
func TestUnmarshalSantaJSON_DeepNestingIsBoundedNotACrash(t *testing.T) {
	nested := func(depth int) []byte {
		return []byte(strings.Repeat(`{"aB":`, depth) + `1` + strings.Repeat(`}`, depth))
	}
	var v any
	if err := UnmarshalSantaJSON(nested(9_999), &v); err != nil {
		t.Errorf("a body nested 9,999 deep was refused: %v", err)
	}
	if err := UnmarshalSantaJSON(nested(200_000), &v); err == nil {
		t.Error("a body nested 200,000 deep was accepted")
	}
	if err := UnmarshalSantaJSON([]byte(strings.Repeat("[", 3_000_000)+strings.Repeat("]", 3_000_000)), &v); err == nil {
		t.Error("an array nested 3,000,000 deep was accepted")
	}
}
