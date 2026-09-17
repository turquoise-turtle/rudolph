package rule

import (
	"fmt"

	"github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/spf13/cobra"
)

// Santa's sync protocol has no ALLOWLIST_TRANSITIVE policy: transitive rules are created locally by Santa for files
// written by binaries allowed with ALLOWLIST_COMPILER. The command is kept (hidden) to explain that to its users.
func init() {
	var ruleTransitiveCmd = &cobra.Command{
		Use:                "transitive",
		Aliases:            []string{"allow-transitive"},
		Short:              "Not supported: transitive rules are created locally by Santa; use \"rule compiler\" instead",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("%w; allow the compiler with \"rudolph rule compiler\" and enable transitive rules in the Santa configuration", rules.ErrTransitivePolicy)
		},
	}

	RuleCmd.AddCommand(ruleTransitiveCmd)
}
