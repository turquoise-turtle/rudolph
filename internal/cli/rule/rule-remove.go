package rule

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/airbnb/rudolph/internal/cli/clockcheck"
	"github.com/airbnb/rudolph/internal/cli/flags"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	"github.com/airbnb/rudolph/pkg/model/machinerules"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

// The `rule remove` command takes slightly different arguments than the other
// `rule` commands so we'll handle it a bit differently here
func init() {
	tf := flags.TargetFlags{}

	var removeRuleCmd = &cobra.Command{
		Use:     `remove <rule-name> (-m <machine-id>|--global) ex: 'TeamID#1234567'`,
		Aliases: []string{"delete"},
		Short:   "Removes/deletes a rule from the backing store",
		Long:    `<rule-name> | <RuleType: Binary,Cert,TeamID,SigningID,CDHash>#<Rule Identifier/SHA256: abcdef12345-12345-12345> | 'TeamID#1234567'`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := tf.RequireExplicitTarget(); err != nil {
				return err
			}
			region, _ := cmd.Flags().GetString("region")
			table, _ := cmd.Flags().GetString("dynamodb_table")

			dynamodbClient := dynamodb.GetClient(table, region)
			if err := tf.ResolveMachineID(dynamodbClient); err != nil {
				return err
			}
			if err := clockcheck.Check(cmd, dynamodbClient); err != nil {
				return err
			}

			globalRemover := globalrules.ConcreteRuleRemovalService{
				TimeProvider: clock.ConcreteTimeProvider{},
				Getter:       dynamodbClient,
				Transacter:   dynamodbClient,
			}
			machineRuleRemover := machinerules.ConcreteRuleRemovalService{
				Getter:  dynamodbClient,
				Updater: dynamodbClient,
			}

			if err := removeRule(globalRemover, machineRuleRemover, args[0], tf); err != nil {
				return err
			}
			// A removed rule's allowed executions are no longer collected. Nothing is written unless it was flagged.
			indexMachineID := ""
			if !tf.IsGlobal {
				indexMachineID = tf.MachineID
			}
			if removed, err := ruleRemoved(dynamodbClient, indexMachineID, args[0]); err == nil && removed {
				forgetCollectedRule(dynamodbClient, indexMachineID, args[0])
			}
			return nil
		},
	}

	tf.AddRuleTargetFlags(removeRuleCmd)
	RuleCmd.AddCommand(removeRuleCmd)
}

func removeRule(globalRemover globalrules.RuleRemovalService, machineRuleRemover machinerules.RuleRemovalService, ruleName string, tf flags.TargetFlags) error {
	if err := tf.RequireExplicitTarget(); err != nil {
		return err
	}

	fmt.Println("Removing the following rule:")
	if tf.IsGlobal {
		fmt.Println("  MachineID:    (Global)")
	} else {
		fmt.Println("  MachineID:   ", tf.MachineID)
	}
	fmt.Println("  Identifier/SHA256:      ", ruleName)
	fmt.Println("")
	fmt.Println(`Apply changes? (Enter: "yes" or "ok")`)
	fmt.Print("> ")

	// Read confirmation
	reader := bufio.NewReader(os.Stdin)
	text, _ := reader.ReadString('\n')
	text = strings.Replace(text, "\n", "", -1)
	if strings.ToLower(text) == "ok" || strings.ToLower(text) == "yes" {
		// Do rule deletion
		if !tf.IsGlobal {
			return machineRuleRemover.RemoveMachineRule(tf.MachineID, ruleName)
		}

		idempotencyKey := uuid.NewString()

		err := globalRemover.RemoveGlobalRule(ruleName, idempotencyKey)
		if err != nil {
			return fmt.Errorf("failed to remove global rule: %v", err)
		}

		fmt.Println("Successfully sent a rule to dynamodb")
	} else {
		fmt.Println("Well ok then")
	}
	fmt.Println("")

	return nil
}

// ruleRemoved reports whether the rule is gone (global) or marked for removal (machine), so that the operator did
// not answer no to the confirmation. The reads are strongly consistent: the removal was written moments ago.
func ruleRemoved(client dynamodb.DynamoDBClient, machineID string, ruleSortKey string) (bool, error) {
	if machineID == "" {
		stored, err := globalrules.GetGlobalRulesBySortKeysConsistently(client, []string{ruleSortKey})
		if err != nil {
			return false, err
		}
		return stored[ruleSortKey] == nil, nil
	}
	rule, err := machinerules.GetMachineRuleConsistently(client, machineID, ruleSortKey)
	if err != nil {
		return false, err
	}
	return rule == nil || rule.DeleteOnNextSync || rule.Policy == types.RulePolicyRemove, nil
}
