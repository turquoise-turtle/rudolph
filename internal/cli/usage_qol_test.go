package cli

// Usage strings, aliases, shorthands and help texts of the whole command tree (plan review of 2026-11-06, C.7, C.8,
// C.9). Fails until the CLI batch lands.

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func walkCommands(cmd *cobra.Command, fn func(*cobra.Command)) {
	fn(cmd)
	for _, c := range cmd.Commands() {
		walkCommands(c, fn)
	}
}

func TestQOL_UsageStringsAliasesAndHelpTexts(t *testing.T) {
	shorthands := map[string]map[string]bool{} // shorthand -> flag names it stands for, across the tree
	walkCommands(RootCmd, func(c *cobra.Command) {
		path := c.CommandPath()
		if strings.Contains(c.Use, "  ") {
			t.Errorf("%s: double space in Use %q", path, c.Use)
		}
		for _, a := range c.Aliases {
			if a == c.Name() {
				t.Errorf("%s: aliased to its own name", path)
			}
		}
		for _, s := range []string{c.Use, c.Short, c.Long} {
			for _, typo := range []string{"complier", "riles", "Retreiv"} {
				if strings.Contains(s, typo) {
					t.Errorf("%s: typo %q in %q", path, typo, s)
				}
			}
		}
		for _, ph := range []string{"<file-path>", "<file-name>"} {
			if strings.Contains(c.Use, ph) && !strings.Contains(c.Use, "-f "+ph) &&
				!strings.Contains(c.Use, "--filepath "+ph) && !strings.Contains(c.Use, "--filename "+ph) {
				t.Errorf("%s: Use %q shows %s as a positional argument, but the file is given with -f", path, c.Use, ph)
			}
		}
		if strings.Contains(c.Use, "=XXX") {
			t.Errorf("%s: Use %q; write -m <machine-id> like the other commands", path, c.Use)
		}
		visit := func(f *pflag.Flag) {
			if strings.TrimSpace(f.Usage) == "." || strings.TrimSpace(f.Usage) == "" {
				t.Errorf("%s --%s: no help text", path, f.Name)
			}
			if f.Name == "filepath" && strings.Contains(f.Usage, "--sha") {
				t.Errorf("%s --filepath: help names --sha, but the other flag is --identifier", path)
			}
			if f.Shorthand != "" {
				if shorthands[f.Shorthand] == nil {
					shorthands[f.Shorthand] = map[string]bool{}
				}
				shorthands[f.Shorthand][f.Name] = true
			}
		}
		c.Flags().VisitAll(visit)
		c.PersistentFlags().VisitAll(visit)
	})
	for short, names := range shorthands {
		if len(names) > 1 {
			var list []string
			for n := range names {
				list = append(list, n)
			}
			t.Errorf("-%s stands for different flags on different commands: %s", short, strings.Join(list, ", "))
		}
	}
}
