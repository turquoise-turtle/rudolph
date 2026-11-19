package eventupload

// Third review of the input handling: the value limit pinned at its exact boundary.

import (
	"strings"
	"testing"
)

// 200,001 values, the first count refused. TestReviewR2_UploadValueLimitCountsNestedArrays checks 200,000 and
// 200,003 values, which a limit loosened to 200,002 (an off-by-one in the count or the comparison) would still pass.
func TestReviewR3_UploadValueLimitRefuses200001Values(t *testing.T) {
	// The root object, the event (as an element and as an object) and each user are values: 3 plus the users.
	body := `{"events":[{"file_sha256":"` + strings.Repeat("ab", 32) + `","logged_in_users":[` +
		strings.Repeat(`"u",`, 199_997) + `"u"]}]}`
	if got := r2Status(t, body); got != 413 {
		t.Errorf("one event with 199,998 logged in users (200,001 JSON values) was answered %d, want 413", got)
	}
}
