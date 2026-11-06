package rule

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/airbnb/rudolph/internal/cli/clockcheck"
	"github.com/airbnb/rudolph/internal/cli/flags"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	"github.com/airbnb/rudolph/pkg/model/groups"
	"github.com/airbnb/rudolph/pkg/model/machinerules"
	"github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/google/uuid"
	"github.com/spf13/cobra"
)

// The `rule remove` command takes slightly different arguments than the other
// `rule` commands so we'll handle it a bit differently here
func init() {
	tf := flags.TargetFlags{}
	var ruleType flags.RuleType
	var identifier string

	var removeRuleCmd = &cobra.Command{
		Use:     `remove (<rule-name> | -t <rule-type> -i <identifier>) (-m <machine-id>|--global|--group <group>|--ungrouped) ex: 'TeamID#1234567'`,
		Aliases: []string{"delete"},
		Short:   "Removes/deletes a rule from the backing store",
		Long:    `<rule-name> | <RuleType: Binary,Cert,TeamID,SigningID,CDHash>#<Rule Identifier/SHA256: abcdef12345-12345-12345> | 'TeamID#1234567'; or give the rule type and identifier with -t and -i`,
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := tf.RequireExplicitTarget(); err != nil {
				return err
			}
			ruleName, err := removalRuleName(args, cmd.Flags().Changed("rule-type"), ruleType.AsRuleType(), identifier)
			if err != nil {
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
			if tf.IsGroup() {
				return removeGroupRule(dynamodbClient, tf.Group, ruleName)
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

			if err := removeRule(globalRemover, machineRuleRemover, ruleName, tf); err != nil {
				return err
			}
			// A removed rule's allowed executions are no longer collected. Nothing is written unless it was flagged.
			indexMachineID := ""
			if !tf.IsGlobal {
				indexMachineID = tf.MachineID
			}
			if removed, err := ruleRemoved(dynamodbClient, indexMachineID, ruleName); err == nil && removed {
				forgetCollectedRule(dynamodbClient, indexMachineID, ruleName)
			}
			return nil
		},
	}

	tf.AddRuleTargetFlags(removeRuleCmd)
	removeRuleCmd.Flags().VarP(&ruleType, "rule-type", "t", `With --identifier, instead of <rule-name>: the type of the rule ("binary", "cert", "teamid", "signingid", "cdhash")`)
	removeRuleCmd.Flags().StringVarP(&identifier, "identifier", "i", "", "With --rule-type, instead of <rule-name>: the rule's identifier")
	RuleCmd.AddCommand(removeRuleCmd)
}

// removalRuleName returns the sort key of the rule to remove: <rule-name> as given, or built from --rule-type and
// --identifier
func removalRuleName(args []string, ruleTypeGiven bool, ruleType types.RuleType, identifier string) (string, error) {
	if len(args) == 1 {
		if ruleTypeGiven || identifier != "" {
			return "", errors.New("pass either <rule-name> or --rule-type and --identifier, not both")
		}
		return args[0], nil
	}
	if !ruleTypeGiven || identifier == "" {
		return "", errors.New("pass the rule to remove as <rule-name> (e.g. TeamID#EQHXZ8M8AV), or with --rule-type and --identifier")
	}
	sortKey := rules.RuleSortKeyFromTypeIdentifier(identifier, ruleType)
	if sortKey == "" {
		return "", errors.New("unknown rule type")
	}
	return sortKey, nil
}

// removeGroupRule removes a device group's rule after a typed confirmation
func removeGroupRule(client dynamodb.DynamoDBClient, group, ruleName string) error {
	writer, ok := client.(groups.RegistryWriter)
	if !ok {
		return errors.New("this DynamoDB client cannot write group rules")
	}
	fmt.Println("Removing the following rule:")
	fmt.Println("  Group:       ", group)
	fmt.Println("  Identifier/SHA256:      ", ruleName)
	fmt.Println("")
	fmt.Println(`Apply changes? (Enter: "yes" or "ok")`)
	fmt.Print("> ")
	reader := bufio.NewReader(os.Stdin)
	text, _ := reader.ReadString('\n')
	text = strings.ToLower(strings.TrimSpace(text))
	if text != "ok" && text != "yes" {
		fmt.Println("Well ok then")
		return nil
	}
	if err := groups.RemoveRule(writer, group, ruleName); err != nil {
		return err
	}
	fmt.Println("Successfully removed the group rule; member Macs drop it, or get the global rule back, at their next sync")
	return nil
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
