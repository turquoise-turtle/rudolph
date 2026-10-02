package clockcheck

// CLI2 (batch 4): unit tests for the clock check that guards rule writes (shipped in batch 3 without tests).

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/smithy-go"
	"github.com/spf13/cobra"
)

type fakeSkew struct {
	skew  time.Duration
	err   error
	calls int
}

func (f *fakeSkew) ClockSkew() (time.Duration, error) {
	f.calls++
	return f.skew, f.err
}

func command(t *testing.T, args ...string) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "write"}
	AddFlag(cmd)
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func TestCLI2_SkewWithinTwoMinutesIsAccepted(t *testing.T) {
	for _, skew := range []time.Duration{0, time.Second, -time.Second, MaxSkew, -MaxSkew} {
		if err := Check(command(t), &fakeSkew{skew: skew}); err != nil {
			t.Errorf("skew %s must be accepted: %v", skew, err)
		}
	}
}

func TestCLI2_SkewAboveTwoMinutesIsRefusedWithDirectionAndFix(t *testing.T) {
	for skew, direction := range map[time.Duration]string{
		MaxSkew + time.Second:    "ahead",
		-MaxSkew - time.Second:   "behind",
		10 * time.Minute:         "ahead",
		-3*time.Hour - time.Hour: "behind",
	} {
		err := Check(command(t), &fakeSkew{skew: skew})
		if err == nil {
			t.Errorf("skew %s must be refused", skew)
			continue
		}
		for _, want := range []string{direction, "--force", "clock"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("skew %s: error must mention %q: %v", skew, want, err)
			}
		}
	}
}

func TestCLI2_ForceSkipsTheCheck(t *testing.T) {
	f := &fakeSkew{skew: time.Hour, err: errors.New("unreachable")}
	if err := Check(command(t, "--force"), f); err != nil {
		t.Fatalf("--force must skip the check: %v", err)
	}
	if f.calls != 0 {
		t.Fatal("--force must not even measure the skew")
	}
}

func TestCLI2_OtherErrorsSuggestForce(t *testing.T) {
	err := Check(command(t), &fakeSkew{err: errors.New("no Date header")})
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("a failed measurement must fail and offer --force: %v", err)
	}
}

// When AWS itself rejects the request because of the clock, --force cannot help: the message says to fix the clock.
func TestCLI2_ClockRejectedByAWSSaysToFixTheClock(t *testing.T) {
	for _, apiErr := range []error{
		&smithy.GenericAPIError{Code: "InvalidSignatureException", Message: "Signature expired: 20260101T000000Z is now earlier than 20260101T000500Z"},
		&smithy.GenericAPIError{Code: "InvalidSignatureException", Message: "Signature not yet current: 20260101T001000Z is still later than 20260101T000500Z"},
		&smithy.GenericAPIError{Code: "RequestTimeTooSkewed", Message: "The difference between the request time and the current time is too large."},
	} {
		err := Check(command(t), &fakeSkew{err: apiErr})
		if err == nil {
			t.Fatalf("%v must fail", apiErr)
		}
		if !strings.Contains(err.Error(), "fix the clock") {
			t.Errorf("must say to fix the clock: %v", err)
		}
		if strings.Contains(err.Error(), "--force") {
			t.Errorf("must not suggest --force, which cannot help: %v", err)
		}
	}
	// An unrelated signature error is not a clock problem.
	err := Check(command(t), &fakeSkew{err: &smithy.GenericAPIError{Code: "InvalidSignatureException", Message: "The request signature we calculated does not match"}})
	if err == nil || strings.Contains(err.Error(), "fix the clock") {
		t.Errorf("a wrong signature is not a clock problem: %v", err)
	}
}
