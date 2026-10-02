package rule

import (
	"github.com/airbnb/rudolph/internal/cli/clockcheck"
	"github.com/airbnb/rudolph/internal/cli/flags"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/spf13/cobra"
)

func init() {
	tf := flags.TargetFlags{}
	rf := flags.RuleInfoFlags{}
	of := flags.RuleOptionFlags{}
	var celExpr string

	var ruleCELCmd = &cobra.Command{
		Use:   "cel -e <cel-expression>",
		Short: "Create a rule that applies the CEL policy: Santa evaluates the CEL expression to allow or block",
		Long:  `The expression must return a santa.cel.v1.ReturnValue or a boolean (true allows, false blocks).`,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			region, _ := cmd.Flags().GetString("region")
			table, _ := cmd.Flags().GetString("dynamodb_table")

			dynamodbClient := dynamodb.GetClient(table, region)
			if err := clockcheck.Check(cmd, dynamodbClient); err != nil {
				return err
			}
			time := clock.ConcreteTimeProvider{}

			options := append(of.Options(), rules.WithCELExpr(celExpr))
			return applyPolicyForPath(time, dynamodbClient, types.RulePolicyCEL, tf, rf, options...)
		},
	}

	tf.AddTargetFlags(ruleCELCmd)
	rf.AddRuleInfoFlags(ruleCELCmd)
	of.AddRuleOptionFlags(ruleCELCmd)
	ruleCELCmd.Flags().StringVarP(&celExpr, "expression", "e", "", `The CEL expression to evaluate, e.g. "target.signing_time >= timestamp('2025-01-01T00:00:00Z')"`)
	_ = ruleCELCmd.MarkFlagRequired("expression")

	RuleCmd.AddCommand(ruleCELCmd)
}
