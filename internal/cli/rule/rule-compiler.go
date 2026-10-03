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

	var ruleCompilerCmd = &cobra.Command{
		Use:     "compiler  <file-path>",
		Aliases: []string{"allow-complier"},
		Short:   "Create a rule that applies the AllowlistCompiler policy to the specified file",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return createRule(cmd, types.AllowlistCompiler, tf, rf, ef, of.Options()...)
		},
	}

	tf.AddRuleTargetFlags(ruleCompilerCmd)
	ef.AddRuleExpiryFlags(ruleCompilerCmd)
	rf.AddRuleInfoFlags(ruleCompilerCmd)
	of.AddRuleOptionFlags(ruleCompilerCmd)

	RuleCmd.AddCommand(ruleCompilerCmd)
}
