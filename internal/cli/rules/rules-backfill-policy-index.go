package rules

import (
	"fmt"

	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/spf13/cobra"

	"github.com/airbnb/rudolph/internal/cli/flags"
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

			ddb, err := dynamodb.GetClient(table, region)
			if err != nil {
				return err
			}
			var client globalrules.PolicyIndexBackfillAPI = ddb
			approval := flags.ApprovalOf(cmd)
			if approval.DryRun {
				client = dryRunBackfillClient{client}
			}
			if err := runBackfillPolicyIndex(client, approval.DryRun); err != nil {
				return err
			}
			if approval.DryRun {
				approval.Write()
			}
			return nil
		},
	}

	RulesCmd.AddCommand(backfillCmd)
}

func runBackfillPolicyIndex(client globalrules.PolicyIndexBackfillAPI, dryRun bool) error {
	updated, err := globalrules.BackfillPolicyIndex(client)
	verb := "Updated"
	if dryRun {
		verb = "Would update"
	}
	if updated > 0 || err == nil {
		fmt.Printf("%s %d global rules\n", verb, updated)
	}
	if err != nil {
		return fmt.Errorf("failed to backfill the policy index: %w", err)
	}
	return nil
}

// dryRunBackfillClient reads global rules as the backfill does, and counts the updates it would write instead of
// writing them
type dryRunBackfillClient struct {
	dynamodb.QueryAPI
}

func (dryRunBackfillClient) UpdateItemIf(dynamodb.PrimaryKey, interface{}, expression.ConditionBuilder) (*awsdynamodb.UpdateItemOutput, error) {
	return &awsdynamodb.UpdateItemOutput{}, nil
}
