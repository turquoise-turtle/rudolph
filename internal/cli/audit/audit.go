package audit

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/airbnb/rudolph/internal/cli/clitime"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	modelaudit "github.com/airbnb/rudolph/pkg/model/audit"
	"github.com/spf13/cobra"
)

const (
	defaultSince = "7d"
	defaultLimit = 100
)

// AuditCmd is `rudolph audit`: the audit trail's entries, newest first
var AuditCmd = &cobra.Command{
	Use:   "audit [--since <duration or RFC 3339 time>] [--limit <n>]",
	Short: "Show who changed rules, configuration, groups or machines, newest first",
	Long: `Show the audit trail: every change the CLI (and rudolph-web) made to rules, configuration, groups or machines,
with its time (local and UTC), actor, action, target and summary, newest first. Entries are kept forever.

--since takes a duration back from now ("7d", "36h", "90m") or an RFC 3339 time ("2026-11-01T00:00:00+11:00"), at
most 400 days back.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		sinceText, _ := cmd.Flags().GetString("since")
		limit, _ := cmd.Flags().GetInt("limit")
		now := time.Now()
		since, err := parseSince(sinceText, now)
		if err != nil {
			return err
		}
		if limit <= 0 {
			return fmt.Errorf("--limit %d: must be at least 1", limit)
		}
		region, _ := cmd.Flags().GetString("region")
		table, _ := cmd.Flags().GetString("dynamodb_table")
		client, err := dynamodb.GetClient(table, region)
		if err != nil {
			return err
		}
		entries, err := modelaudit.List(client, since, limit)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			fmt.Printf("No audit entries since %s.\n", clitime.Absolute(since))
			return nil
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(w, "LOCAL TIME\tTIME (UTC)\tACTOR\tACTION\tTARGET\tSUMMARY")
		for _, e := range entries {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", e.Time.Local().Format("2006-01-02 15:04:05 MST"), e.Time.UTC().Format(time.RFC3339), e.Actor, e.Action, e.Target, e.Summary)
		}
		if err := w.Flush(); err != nil {
			return err
		}
		if len(entries) == limit {
			fmt.Printf("\nShowing the newest %d entries; pass a larger --limit or a later --since for more.\n", limit)
		}
		return nil
	},
}

func init() {
	AuditCmd.Flags().String("since", defaultSince, `Show entries from this long ago ("7d", "36h", "90m") or since this RFC 3339 time`)
	AuditCmd.Flags().Int("limit", defaultLimit, "Show at most this many entries")
}

// parseSince reads --since: a positive duration back from now, in Go's units or whole days ("7d"), or an RFC 3339 time
// that is not in the future
func parseSince(text string, now time.Time) (time.Time, error) {
	var since time.Time
	if days, ok := strings.CutSuffix(text, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n <= 0 {
			return time.Time{}, fmt.Errorf(`--since %q: give a whole number of days ("7d"), a duration ("36h") or an RFC 3339 time`, text)
		}
		since = now.AddDate(0, 0, -n)
	} else if d, err := time.ParseDuration(text); err == nil {
		if d <= 0 {
			return time.Time{}, fmt.Errorf("--since %q: the duration must be positive", text)
		}
		since = now.Add(-d)
	} else if t, err := time.Parse(time.RFC3339, text); err == nil {
		since = t
	} else {
		return time.Time{}, fmt.Errorf(`--since %q is neither a duration ("7d", "36h") nor an RFC 3339 time ("2026-11-01T00:00:00Z")`, text)
	}
	if since.After(now) {
		return time.Time{}, fmt.Errorf("--since %q is in the future", text)
	}
	if since.Before(now.Add(-modelaudit.MaxLookback)) {
		return time.Time{}, fmt.Errorf("--since %q: %w", text, modelaudit.ErrSinceTooOld)
	}
	return since, nil
}
