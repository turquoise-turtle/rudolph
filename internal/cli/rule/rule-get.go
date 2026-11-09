package rule

import (
	"errors"
	"fmt"

	"github.com/airbnb/rudolph/internal/cli/flags"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/spf13/cobra"
)

func init() {
	tf := flags.TargetFlags{}
	rf := flags.RuleInfoFlags{}

	var ruleGetCmd = &cobra.Command{
		Use:   "get (-f <file-path>|-i <identifier>) -t <rule-type> (-m <machine-id>|--global)",
		Short: "Show one global or machine rule",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := tf.RequireExplicitTarget(); err != nil {
				return err
			}
			if tf.IsGroup() {
				return errors.New(`a group's rules are shown with "rudolph group get <group>"`)
			}
			if err := rf.RequireOne(); err != nil {
				return err
			}
			ruleType := rf.RuleType.AsRuleType()
			identifier, _, err := identifyRule(rf, ruleType)
			if err != nil {
				return err
			}

			region, _ := cmd.Flags().GetString("region")
			table, _ := cmd.Flags().GetString("dynamodb_table")
			client, err := dynamodb.GetClient(table, region)
			if err != nil {
				return err
			}
			if err := tf.ResolveMachineID(client); err != nil {
				return err
			}

			machineID, suffix := "(Global)", ""
			if !tf.IsGlobal {
				if machineID, err = tf.GetMachineID(); err != nil {
					return err
				}
				if tf.IsTargetSelf() {
					suffix = " (This machine)"
				}
			}
			rh := ruleHandler{dynamodbClient: client, timeProvider: clock.ConcreteTimeProvider{}}
			current, err := rh.readRule(tf.IsGlobal, machineID, identifier, ruleType)
			if err != nil {
				return err
			}
			printRule("Rule "+current.key.SortKey+":", machineID, suffix, current.rule, current.description, tf.IsGlobal, current.expires, current.collecting, rh.timeProvider.Now())
			if current.removing {
				fmt.Println("  Being removed: the Mac drops it, or gets the global rule back, at its next sync; it cannot be updated")
			}
			return nil
		},
	}

	tf.AddRuleTargetFlags(ruleGetCmd)
	rf.AddRuleInfoFlags(ruleGetCmd)

	RuleCmd.AddCommand(ruleGetCmd)
}
