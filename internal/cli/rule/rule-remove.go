package rule

import (
	"errors"
	"fmt"

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

// `rule remove` takes the rule as -t <rule-type> -i <identifier>, like the other rule commands, or as the rule's sort
// key (<rule-name>), which `rudolph rules` prints
func init() {
	tf := flags.TargetFlags{}
	var ruleType flags.RuleType
	var identifier string

	var removeRuleCmd = &cobra.Command{
		Use:     "remove (-t <rule-type> -i <identifier>|<rule-name>) (-m <machine-id>|--global|--group <group>|--ungrouped)",
		Aliases: []string{"delete"},
		Short:   "Remove a rule",
		Long: `Remove a rule, given with -t <rule-type> -i <identifier>, or as <rule-name>: the rule's type prefix and
identifier as "rudolph rules" lists them, e.g. TeamID#EQHXZ8M8AV. The prefixes are Binary#, Cert#, TeamID#,
SigningID# and CDHash#.

A removed global rule is deleted, and Macs drop it at their next sync. A removed machine rule is marked for deletion:
the Mac drops it, or gets the global rule for the same identifier back, at its next sync.`,
		Args: cobra.MaximumNArgs(1),
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

			dynamodbClient, err := dynamodb.GetClient(table, region)

			if err != nil {

				return err

			}
			if err := tf.ResolveMachineID(dynamodbClient); err != nil {
				return err
			}
			if err := clockcheck.Check(cmd, dynamodbClient); err != nil {
				return err
			}
			approval := flags.ApprovalOf(cmd)
			if approval.DryRun {
				if err := requireRuleToRemove(dynamodbClient, tf, ruleName); err != nil {
					return err
				}
			}
			if tf.IsGroup() {
				return removeGroupRule(dynamodbClient, tf.Group, ruleName, approval)
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

			if err := removeRule(globalRemover, machineRuleRemover, ruleName, tf, approval); err != nil {
				return err
			}
			if approval.DryRun {
				return nil
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

// removeGroupRule removes a device group's rule after a confirmation (none with --yes, nothing written with --dry-run)
func removeGroupRule(client dynamodb.DynamoDBClient, group, ruleName string, approval flags.Approval) error {
	writer, ok := client.(groups.RegistryWriter)
	if !ok {
		return errors.New("this DynamoDB client cannot write group rules")
	}
	fmt.Println("Removing the following rule:")
	fmt.Println("  Group:       ", group)
	fmt.Println("  Identifier/SHA256:      ", ruleName)
	fmt.Println("")
	if apply, err := approval.Confirm(); !apply {
		return err
	}
	if err := groups.RemoveRule(writer, group, ruleName); err != nil {
		return fmt.Errorf("rule %s of group %s: %w", ruleName, group, err)
	}
	fmt.Printf("Removed rule %s from group %s; member Macs drop it, or get the global rule back, at their next sync\n", ruleName, group)
	return nil
}

func removeRule(globalRemover globalrules.RuleRemovalService, machineRuleRemover machinerules.RuleRemovalService, ruleName string, tf flags.TargetFlags, approval flags.Approval) error {
	if err := tf.RequireExplicitTarget(); err != nil {
		return err
	}

	fmt.Println("Removing the following rule:")
	if tf.IsGlobal {
		fmt.Println("  MachineID:    (Global)")
	} else {
		fmt.Println("  MachineID:   ", tf.MachineID)
	}
	fmt.Println("  Rule:        ", ruleName)
	fmt.Println("")
	if apply, err := approval.Confirm(); !apply {
		return err
	}

	if !tf.IsGlobal {
		err := machineRuleRemover.RemoveMachineRule(tf.MachineID, ruleName)
		if errors.Is(err, machinerules.ErrRuleNotFound) {
			return fmt.Errorf("machine %s has no rule %s", tf.MachineID, ruleName)
		}
		if err != nil {
			return fmt.Errorf("failed to remove rule %s of machine %s: %w", ruleName, tf.MachineID, err)
		}
		fmt.Printf("Removed rule %s of machine %s; the Mac drops it, or gets the global rule back, at its next sync\n", ruleName, tf.MachineID)
		return nil
	}

	if err := globalRemover.RemoveGlobalRule(ruleName, uuid.NewString()); err != nil {
		return fmt.Errorf("failed to remove global rule %s: %w", ruleName, err)
	}
	fmt.Printf("Removed global rule %s; Macs drop it at their next sync\n", ruleName)
	return nil
}

// requireRuleToRemove fails, as the removal itself would, when there is no rule to remove. A dry run needs it: the
// removals only find out when they write.
func requireRuleToRemove(client dynamodb.DynamoDBClient, tf flags.TargetFlags, ruleName string) error {
	if tf.IsGroup() {
		reader, ok := client.(groups.RegistryWriter)
		if !ok {
			return errors.New("this DynamoDB client cannot read group rules")
		}
		registry, err := groups.GetRegistry(reader, true)
		if err != nil {
			return err
		}
		if _, ok := registry.Groups[tf.Group]; !ok {
			return fmt.Errorf("rule %s of group %s: %w", ruleName, tf.Group, groups.ErrGroupNotFound)
		}
		rows, err := groups.GetRules(reader, tf.Group)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if row.SortKey == ruleName {
				return nil
			}
		}
		return fmt.Errorf("rule %s of group %s: %w", ruleName, tf.Group, groups.ErrRuleNotFound)
	}
	if err := tf.RequireExplicitTarget(); err != nil {
		return err
	}
	if tf.IsGlobal {
		rule, err := globalrules.GetGlobalRuleBySortKey(client, ruleName)
		if err != nil {
			return fmt.Errorf("failed to remove global rule %s: query to retrieve existing rule failed: %w", ruleName, err)
		}
		if rule == nil {
			return fmt.Errorf("failed to remove global rule %s: no such rule with sk (%s) exists", ruleName, ruleName)
		}
		return nil
	}
	rule, err := machinerules.GetMachineRuleConsistently(client, tf.MachineID, ruleName)
	if err != nil {
		return fmt.Errorf("failed to remove rule %s of machine %s: %w", ruleName, tf.MachineID, err)
	}
	if rule == nil {
		return fmt.Errorf("machine %s has no rule %s", tf.MachineID, ruleName)
	}
	return nil
}

// ruleRemoved reports whether the rule is gone (global) or marked for removal (machine), so that a rule written again
// since the removal keeps its collection. The reads are strongly consistent: the removal was written moments ago.
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
