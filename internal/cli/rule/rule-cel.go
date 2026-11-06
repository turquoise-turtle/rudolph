package rule

import (
	"github.com/airbnb/rudolph/internal/cli/flags"
	"github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/spf13/cobra"
)

func init() {
	tf := flags.TargetFlags{}
	rf := flags.RuleInfoFlags{}
	of := flags.RuleOptionFlags{}
	ef := flags.RuleExpiryFlags{}
	var celExpr string

	var ruleCELCmd = &cobra.Command{
		Use:   ruleUse("cel", "-e <cel-expression> [--expires <duration-or-time>|--permanent]"),
		Short: "Create a rule that applies the CEL policy: Santa evaluates the CEL expression to allow or block",
		Long:  `The expression must return a santa.cel.v1.ReturnValue or a boolean (true allows, false blocks).`,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			options := append(of.Options(), rules.WithCELExpr(celExpr))
			return createRule(cmd, types.RulePolicyCEL, tf, rf, ef, options...)
		},
	}

	tf.AddRuleTargetFlags(ruleCELCmd)
	ef.AddRuleExpiryFlags(ruleCELCmd)
	rf.AddRuleInfoFlags(ruleCELCmd)
	of.AddRuleOptionFlags(ruleCELCmd)
	ruleCELCmd.Flags().StringVarP(&celExpr, "expression", "e", "", `The CEL expression to evaluate, e.g. "target.signing_time >= timestamp('2025-01-01T00:00:00Z')"`)
	_ = ruleCELCmd.MarkFlagRequired("expression")

	RuleCmd.AddCommand(ruleCELCmd)
}
