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

	var ruleSilentCmd = &cobra.Command{
		Use:     "silent",
		Aliases: []string{"silentblock"},
		Short:   "Create a rule that applies the SilentBlocklist policy to the specified file",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return createRule(cmd, types.SilentBlocklist, tf, rf, ef, of.Options()...)
		},
	}

	tf.AddRuleTargetFlags(ruleSilentCmd)
	ef.AddRuleExpiryFlags(ruleSilentCmd)
	rf.AddRuleInfoFlags(ruleSilentCmd)
	of.AddRuleOptionFlags(ruleSilentCmd)

	RuleCmd.AddCommand(ruleSilentCmd)
}
