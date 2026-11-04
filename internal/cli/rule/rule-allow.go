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

	var ruleAllowCmd = &cobra.Command{
		Use:   "allow [-f <file-path>|-i <identifier/sha256>] -t <rule-type> (-m <machine-id>|--global)",
		Short: "Create a rule that applies the Allowlist policy to the specified file",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return createRule(cmd, types.Allowlist, tf, rf, ef, of.Options()...)
		},
	}

	tf.AddRuleTargetFlags(ruleAllowCmd)
	ef.AddRuleExpiryFlags(ruleAllowCmd)
	rf.AddRuleInfoFlags(ruleAllowCmd)
	of.AddRuleOptionFlags(ruleAllowCmd)

	RuleCmd.AddCommand(ruleAllowCmd)
}
