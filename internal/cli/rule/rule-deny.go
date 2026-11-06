package rule

import (
	"github.com/airbnb/rudolph/internal/cli/flags"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/spf13/cobra"
)

func init() {
	tf := flags.TargetFlags{}
	rf := flags.RuleInfoFlags{}
	of := flags.RuleOptionFlags{}
	ef := flags.RuleExpiryFlags{}

	var ruleDenyCmd = &cobra.Command{
		Use:     ruleUse("deny", "[--expires <duration-or-time>|--permanent]"),
		Aliases: []string{"block"},
		Short:   "Create a rule that applies the Blocklist policy to the specified file",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return createRule(cmd, types.Blocklist, tf, rf, ef, of.Options()...)
		},
	}

	tf.AddRuleTargetFlags(ruleDenyCmd)
	ef.AddRuleExpiryFlags(ruleDenyCmd)
	rf.AddRuleInfoFlags(ruleDenyCmd)
	of.AddRuleOptionFlags(ruleDenyCmd)

	RuleCmd.AddCommand(ruleDenyCmd)
}
