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

	var ruleSilentGUICmd = &cobra.Command{
		Use:     "silent-gui",
		Aliases: []string{"silentguiblock"},
		Short:   "Create a rule that applies the SilentGUIBlocklist policy (block without GUI notifications; TTY notifications are still shown)",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return createRule(cmd, types.RulePolicySilentGUIBlocklist, tf, rf, ef, of.Options()...)
		},
	}

	tf.AddRuleTargetFlags(ruleSilentGUICmd)
	ef.AddRuleExpiryFlags(ruleSilentGUICmd)
	rf.AddRuleInfoFlags(ruleSilentGUICmd)
	of.AddRuleOptionFlags(ruleSilentGUICmd)

	RuleCmd.AddCommand(ruleSilentGUICmd)
}
