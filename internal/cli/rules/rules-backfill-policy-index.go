package rules

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
)

func addRuleBackfillPolicyIndexCommand() {
	var backfillCmd = &cobra.Command{
		Use:   "backfill-policy-index",
		Short: "Adds GlobalRulePolicy to global rules written before the GlobalRulePolicy_SK index; run once after upgrading",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			region, _ := cmd.Flags().GetString("region")
			table, _ := cmd.Flags().GetString("dynamodb_table")

			dynamodbClient := dynamodb.GetClient(table, region)

			return runBackfillPolicyIndex(dynamodbClient)
		},
	}

	RulesCmd.AddCommand(backfillCmd)
}

func runBackfillPolicyIndex(client globalrules.PolicyIndexBackfillAPI) error {
	updated, err := globalrules.BackfillPolicyIndex(client)
	if updated > 0 || err == nil {
		fmt.Printf("Updated %d global rules\n", updated)
	}
	if err != nil {
		return fmt.Errorf("failed to backfill the policy index: %w", err)
	}
	return nil
}
