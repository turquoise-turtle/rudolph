package cli

// Holiday mode for the fleet health page (owner decision 2, 2026-11-22) is set from the CLI as
// `rudolph fleet holiday --stale-days <days> --until <YYYY-MM-DD>` and ended with `rudolph fleet holiday --end`
// (internal/cli/fleet). The root command offers it, and like the other commands that write the table it needs an
// environment.

import (
	"testing"
)

func TestFleetHolidayIsARootCommand(t *testing.T) {
	cmd, _, err := RootCmd.Find([]string{"fleet", "holiday"})
	if err != nil || cmd == nil || cmd.Name() != "holiday" || cmd.Parent() == nil || cmd.Parent().Name() != "fleet" {
		found := ""
		if cmd != nil {
			found = cmd.CommandPath()
		}
		t.Fatalf("rudolph fleet holiday is not a command (found %q): %v", found, err)
	}
	for _, flag := range []string{"stale-days", "until", "end"} {
		if cmd.Flags().Lookup(flag) == nil {
			t.Errorf("rudolph fleet holiday has no --%s", flag)
		}
	}
	for c := cmd; c != nil; c = c.Parent() {
		if c.Annotations[noEnvironment] != "" || c.Annotations[optionalEnvironment] != "" {
			t.Errorf("%s does not need an environment, but it writes the table", c.CommandPath())
		}
	}
}
