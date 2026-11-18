package request

// Input handling review: every sync route (preflight, eventupload, ruledownload, postflight) passes its body through
// UnmarshalSantaJSON before anything else looks at it, so the time it takes per byte of body is paid by whoever holds
// the fleet's sync token.

import (
	"fmt"
	"runtime"
	"runtime/debug"
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

// timeBestOfThree returns the fastest of three runs of fn, so a pause of the test process cannot fail a linear
// implementation. The collector is off while timing: on a loaded machine a collection that has to stop every thread
// waits for threads the scheduler has parked, and the larger body triggers more of them, which made a linear
// normaliser look quadratic.
func timeBestOfThree(t *testing.T, fn func() error) time.Duration {
	t.Helper()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	best := time.Duration(1<<63 - 1)
	for run := 0; run < 3; run++ {
		runtime.GC()
		start := time.Now()
		if err := fn(); err != nil {
			t.Fatalf("a valid body was refused: %v", err)
		}
		best = min(best, time.Since(start))
	}
	return best
}

// wantLinearInKeys times run on bodies of 2,500 and 20,000 keys and fails if eight times the keys took more than 24
// times as long. 24 sits between linear (8) and quadratic (64) with a factor of about three on each side, which covers
// the keys growing out of the processor caches. Both bodies stay under a few hundred KB: above about a megabyte the
// time per key rises on a loaded machine whatever the algorithm. The comparison is made up to three times and passes on
// the first that fits: at a load average of 60, one comparison in a few of a linear normaliser went over, while a
// quadratic one is over on every attempt.
func wantLinearInKeys(t *testing.T, what string, run func(keys int) error) {
	t.Helper()
	var small, large time.Duration
	var ratio float64
	for attempt := 0; attempt < 3; attempt++ {
		small = timeBestOfThree(t, func() error { return run(2_500) })
		large = timeBestOfThree(t, func() error { return run(20_000) })
		if ratio = float64(large) / float64(small); ratio <= 24 {
			return
		}
	}
	t.Errorf("8 times the keys took %.1f times as long (%s for 2,500 keys, %s for 20,000), want about 8: %s is quadratic in the keys of one object",
		ratio, small, large, what)
}

// An object with many camelCase keys must not cost time quadratic in the number of keys. hasCollision once compared
// every camelCase key of an object with every key of that object, so a body of a few hundred KB (about a kilobyte once
// compressed, which API Gateway undoes before the Lambda) kept a handler busy until its 10 second timeout. This times
// the whole parse every sync route makes (validate, survey, rewrite, unmarshal). It used to be an absolute budget,
// which a loaded test machine broke for a linear parser; how the time scales does not depend on the machine.
func TestUnmarshalSantaJSON_ManyCamelCaseKeysTakeLinearTime(t *testing.T) {
	bodies := map[int][]byte{2_500: wideCamelCaseObject(2_500), 20_000: wideCamelCaseObject(20_000)}
	wantLinearInKeys(t, "parsing a request body", func(keys int) error {
		var parsed struct {
			Events []struct{} `json:"events"`
		}
		return UnmarshalSantaJSON(bodies[keys], &parsed)
	})
}

// The same for the normaliser alone, which is where the quadratic comparison was.
func TestNormalizeProtoJSONKeys_TimeScalesLinearlyWithKeys(t *testing.T) {
	bodies := map[int][]byte{2_500: wideCamelCaseObject(2_500), 20_000: wideCamelCaseObject(20_000)}
	wantLinearInKeys(t, "normalising keys", func(keys int) error {
		_, err := normalizeProtoJSONKeys(bodies[keys])
		return err
	})
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
