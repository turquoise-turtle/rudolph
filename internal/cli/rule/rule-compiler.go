package rule

import (
	"github.com/airbnb/rudolph/internal/cli/clockcheck"
	"github.com/airbnb/rudolph/internal/cli/flags"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/spf13/cobra"
)

func init() {
	tf := flags.TargetFlags{}
	rf := flags.RuleInfoFlags{}
	of := flags.RuleOptionFlags{}

	var ruleCompilerCmd = &cobra.Command{
		Use:     "compiler  <file-path>",
		Aliases: []string{"allow-complier"},
		Short:   "Create a rule that applies the AllowlistCompiler policy to the specified file",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// args[0] has already been validated as a file before this
			region, _ := cmd.Flags().GetString("region")
			table, _ := cmd.Flags().GetString("dynamodb_table")

			dynamodbClient := dynamodb.GetClient(table, region)
			if err := clockcheck.Check(cmd, dynamodbClient); err != nil {
				return err
			}
			time := clock.ConcreteTimeProvider{}

			return applyPolicyForPath(time, dynamodbClient, types.AllowlistCompiler, tf, rf, of.Options()...)
		},
	}

	tf.AddTargetFlags(ruleCompilerCmd)
	rf.AddRuleInfoFlags(ruleCompilerCmd)
	of.AddRuleOptionFlags(ruleCompilerCmd)

	RuleCmd.AddCommand(ruleCompilerCmd)
}
