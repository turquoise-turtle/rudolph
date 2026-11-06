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

	var ruleSilentTTYCmd = &cobra.Command{
		Use:     ruleUse("silent-tty", "[--expires <duration-or-time>|--permanent]"),
		Aliases: []string{"silenttyblock"},
		Short:   "Create a rule that applies the SilentTTYBlocklist policy (block without TTY notifications; GUI notifications are still shown)",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return createRule(cmd, types.RulePolicySilentTTYBlocklist, tf, rf, ef, of.Options()...)
		},
	}

	tf.AddRuleTargetFlags(ruleSilentTTYCmd)
	ef.AddRuleExpiryFlags(ruleSilentTTYCmd)
	rf.AddRuleInfoFlags(ruleSilentTTYCmd)
	of.AddRuleOptionFlags(ruleSilentTTYCmd)

	RuleCmd.AddCommand(ruleSilentTTYCmd)
}
