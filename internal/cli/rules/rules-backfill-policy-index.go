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
			updated, err := runBackfillPolicyIndex(client, approval.DryRun)
			if approval.DryRun {
				if err != nil {
					return err
				}
				approval.Write()
				return nil
			}
			if updated == 0 {
				return err
			}
			// Recorded even when the backfill stopped part way: the rules updated so far were written
			return flags.RecordOutcome(ddb, "rules.backfill-policy-index", flags.GlobalTarget, fmt.Sprintf("added GlobalRulePolicy to %d global rules", updated), err)
		},
	}

	RulesCmd.AddCommand(backfillCmd)
}

func runBackfillPolicyIndex(client globalrules.PolicyIndexBackfillAPI, dryRun bool) (int, error) {
	updated, err := globalrules.BackfillPolicyIndex(client)
	verb := "Updated"
	if dryRun {
		verb = "Would update"
	}
	if updated > 0 || err == nil {
		fmt.Printf("%s %d global rules\n", verb, updated)
	}
	if err != nil {
		return updated, fmt.Errorf("failed to backfill the policy index: %w", err)
	}
	return updated, nil
}

// dryRunBackfillClient reads global rules as the backfill does, and counts the updates it would write instead of
// writing them
type dryRunBackfillClient struct {
	dynamodb.QueryAPI
}

func (dryRunBackfillClient) UpdateItemIf(dynamodb.PrimaryKey, interface{}, expression.ConditionBuilder) (*awsdynamodb.UpdateItemOutput, error) {
	return &awsdynamodb.UpdateItemOutput{}, nil
}
