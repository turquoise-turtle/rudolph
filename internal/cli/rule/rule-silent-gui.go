package rule

import (
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

	var ruleSilentGUICmd = &cobra.Command{
		Use:     "silent-gui",
		Aliases: []string{"silentguiblock"},
		Short:   "Create a rule that applies the SilentGUIBlocklist policy (block without GUI notifications; TTY notifications are still shown)",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			region, _ := cmd.Flags().GetString("region")
			table, _ := cmd.Flags().GetString("dynamodb_table")

			dynamodbClient := dynamodb.GetClient(table, region)
			time := clock.ConcreteTimeProvider{}

			return applyPolicyForPath(time, dynamodbClient, types.RulePolicySilentGUIBlocklist, tf, rf, of.Options()...)
		},
	}

	tf.AddTargetFlags(ruleSilentGUICmd)
	rf.AddRuleInfoFlags(ruleSilentGUICmd)
	of.AddRuleOptionFlags(ruleSilentGUICmd)

	RuleCmd.AddCommand(ruleSilentGUICmd)
}
