// Package clockcheck refuses to write rules from a machine whose clock is too far off.
//
// Rules feed rows are sorted by the writer's clock. Macs read the feed from 10 minutes before the start of their
// earlier syncs (syncstate.FeedSyncCursorOverlap), so a rule written by a CLI whose clock is more than that behind can
// land before a Mac's cursor and never reach it until its next clean sync. A clock that is ahead delays nothing but
// makes the rule's position misleading, and machine rule expiry times come from the same clock.
package clockcheck

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/aws/smithy-go"
	"github.com/spf13/cobra"
)

// MaxSkew is the largest difference between the local clock and DynamoDB's that rules are written with
const MaxSkew = 2 * time.Minute

const forceFlag = "force"

// AddFlag adds the --force flag, which writes rules even when the clock is off, to a command and its subcommands
func AddFlag(cmd *cobra.Command) {
	cmd.PersistentFlags().Bool(forceFlag, false, fmt.Sprintf("write rules even if the local clock is more than %s off", MaxSkew))
}

// Check returns an error when the local clock differs from DynamoDB's by more than MaxSkew, unless --force is set.
func Check(cmd *cobra.Command, client dynamodb.ClockSkewAPI) error {
	force, _ := cmd.Flags().GetBool(forceFlag)
	if force {
		return nil
	}
	skew, err := client.ClockSkew()
	if isClockRejected(err) {
		// AWS refuses requests signed more than about 5 minutes off; skipping this check would not help
		return fmt.Errorf("AWS rejected the request because the local clock is too far off; fix the clock "+
			"(e.g. sudo sntp -sS time.apple.com) and try again: %w", err)
	}
	if err != nil {
		return fmt.Errorf("failed to compare the local clock with DynamoDB's (pass --%s to skip the check): %w", forceFlag, err)
	}
	return checkSkew(skew)
}

// isClockRejected reports whether AWS refused a request because of the time it was signed at
func isClockRejected(err error) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	message := strings.ToLower(apiErr.ErrorMessage())
	return apiErr.ErrorCode() == "InvalidSignatureException" && strings.Contains(message, "signature") &&
		(strings.Contains(message, "expired") || strings.Contains(message, "not yet current")) ||
		apiErr.ErrorCode() == "RequestTimeTooSkewed"
}

func checkSkew(skew time.Duration) error {
	if skew <= MaxSkew && skew >= -MaxSkew {
		return nil
	}
	direction := "ahead of"
	if skew < 0 {
		direction, skew = "behind", -skew
	}
	return fmt.Errorf("the local clock is %s %s DynamoDB's; rules written with it may never reach some Macs. "+
		"Fix the clock (e.g. sudo sntp -sS time.apple.com), or pass --%s", skew.Round(time.Second), direction, forceFlag)
}
