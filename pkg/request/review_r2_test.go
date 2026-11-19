package request

// Second review of the input handling: the key normaliser's cost on objects that mix spellings, which
// TestNormalizeProtoJSONKeys_TimeScalesLinearlyWithKeys (camelCase keys only) never sends through the snake_case key
// map built to find a collision.

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

// mixedSpellingObject is a body whose one nested object has n snake_case keys and n camelCase keys. When colliding,
// each camelCase key is the other spelling of one of the snake_case keys; otherwise none is.
func mixedSpellingObject(n int, colliding bool) []byte {
	var b strings.Builder
	b.WriteString(`{"events":[],"padding":{`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		camel := fmt.Sprintf("g%dX", i)
		if colliding {
			camel = fmt.Sprintf("f%dX", i)
		}
		fmt.Fprintf(&b, `"f%d_x":0,%q:0`, i, camel)
	}
	b.WriteString(`}}`)
	return []byte(b.String())
}

// bestOfThree times normalizeProtoJSONKeys on body, keeping the fastest run so a pause of the test process cannot
// fail a linear implementation. The collector is off while timing: on a loaded machine a collection that has to stop
// every thread waits for threads the scheduler has parked, and the larger body triggers more of them, which made a
// linear normaliser look quadratic.
func bestOfThree(t *testing.T, body []byte) time.Duration {
	t.Helper()
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	best := time.Duration(1<<63 - 1)
	for run := 0; run < 3; run++ {
		runtime.GC()
		start := time.Now()
		if _, err := normalizeProtoJSONKeys(body); err != nil {
			t.Fatalf("a valid body was refused: %v", err)
		}
		best = min(best, time.Since(start))
	}
	return best
}

// Eight times the keys must take about eight times as long, with and without fields spelled both ways. The allowed
// ratio (24) sits between linear (8) and quadratic (64) with a factor of about three on each side, which covers the
// map growing out of the processor caches. Both sizes stay under a few hundred KB of body: above about a megabyte the
// time per key rises on a loaded machine whatever the algorithm. The smaller size takes about a millisecond when
// linear, well above timer resolution. The comparison is made up to three times and passes on the first that fits:
// on a machine with a load average of 60, one in a few comparisons of a linear normaliser went over, while a quadratic
// one is over on every attempt.
func TestReviewR2_MixedSpellingKeysTakeLinearTime(t *testing.T) {
	for _, colliding := range []bool{false, true} {
		t.Run(fmt.Sprintf("colliding=%t", colliding), func(t *testing.T) {
			smallBody, largeBody := mixedSpellingObject(2_500, colliding), mixedSpellingObject(20_000, colliding)
			var small, large time.Duration
			var ratio float64
			for attempt := 0; attempt < 3; attempt++ {
				small, large = bestOfThree(t, smallBody), bestOfThree(t, largeBody)
				if ratio = float64(large) / float64(small); ratio <= 24 {
					return
				}
			}
			t.Errorf("8 times the keys took %.1f times as long (%s for 2,500 pairs, %s for 20,000), want about 8: normalising mixed spellings is quadratic in the keys of one object",
				ratio, small, large)
		})
	}
}

// allocatedBy returns the bytes fn allocates. TotalAlloc only grows and does not depend on when the garbage collector
// runs, so the count is the same from run to run for the same code.
func allocatedBy(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// manyCollidingObjects is a body of n small objects that each spell one field both ways.
func manyCollidingObjects(n int) []byte {
	return []byte(`{"p":[` + strings.Repeat(`{"aB":0,"a_b":0},`, n-1) + `{"aB":0,"a_b":0}]}`)
}

// wideCollidingObject is a body whose one object has n snake_case keys besides a field spelled both ways.
func wideCollidingObject(n int) []byte {
	var b strings.Builder
	b.WriteString(`{"p":{"a_b":0`)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, `,"k_%d":0`, i)
	}
	b.WriteString(`,"aB":0}}`)
	return []byte(b.String())
}

// An object that spells a field both ways needs the map of its snake_case keys once to find the collision and again to
// rewrite its keys, and keeping the first rather than building it again saves about a third or more of what
// normalising such a body allocates. Neither body is anything Santa sends, but both are what a holder of the sync
// token can send to make every sync route allocate.
//
// Measured with Go 1.27 on darwin/arm64, for 30,000 objects (510 KB) and for one object of 30,000 keys (349 KB):
// building the map twice allocates 36.7 and 32.5 bytes per byte of body, and building it once 21.4 and 22.2. The bound
// of 28 sits between them; the narrowest margin is on the one wide object, where the bound is only about 14% below what
// building the map twice allocates.
// The counts are exact from run to run (TotalAlloc does not depend on when the collector runs), but a change to Go's
// map implementation moves them, so a failure after a Go upgrade with no change here means measuring again.
func TestReviewR2_CollidingSpellingsBuildTheSnakeKeyMapOnce(t *testing.T) {
	const maxAllocPerBodyByte = 28
	for name, body := range map[string][]byte{
		"30,000 objects each spelling a field both ways":  manyCollidingObjects(30_000),
		"one object of 30,000 snake_case keys and a pair": wideCollidingObject(30_000),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := normalizeProtoJSONKeys(body); err != nil {
				t.Fatalf("a valid body was refused: %v", err)
			}
			allocated := allocatedBy(func() {
				if _, err := normalizeProtoJSONKeys(body); err != nil {
					t.Error(err)
				}
			})
			if perByte := float64(allocated) / float64(len(body)); perByte > maxAllocPerBodyByte {
				t.Errorf("normalising a %d KB body allocated %d KB, %.1f bytes per byte of body, want at most %d: the snake_case key map of a colliding object is built more than once",
					len(body)>>10, allocated>>10, perByte, maxAllocPerBodyByte)
			}
		})
	}
}
