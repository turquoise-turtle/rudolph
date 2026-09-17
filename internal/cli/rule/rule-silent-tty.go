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

	var ruleSilentTTYCmd = &cobra.Command{
		Use:     "silent-tty",
		Aliases: []string{"silenttyblock"},
		Short:   "Create a rule that applies the SilentTTYBlocklist policy (block without TTY notifications; GUI notifications are still shown)",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			region, _ := cmd.Flags().GetString("region")
			table, _ := cmd.Flags().GetString("dynamodb_table")

			dynamodbClient := dynamodb.GetClient(table, region)
			time := clock.ConcreteTimeProvider{}

			return applyPolicyForPath(time, dynamodbClient, types.RulePolicySilentTTYBlocklist, tf, rf, of.Options()...)
		},
	}

	tf.AddTargetFlags(ruleSilentTTYCmd)
	rf.AddRuleInfoFlags(ruleSilentTTYCmd)
	of.AddRuleOptionFlags(ruleSilentTTYCmd)

	RuleCmd.AddCommand(ruleSilentTTYCmd)
}
