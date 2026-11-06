package rules

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/airbnb/rudolph/internal/cli/flags"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	"github.com/airbnb/rudolph/pkg/model/machinerules"
	modelrules "github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/types"
)

var (
	RulesCmd *cobra.Command
)

func init() {
	tf := flags.TargetFlags{}
	var filter ruleFilter

	RulesCmd = &cobra.Command{
		Use:   "rules [-m <machine-id>|--global] [--type <rule-type>] [--policy <policy>] [--limit <n>]",
		Short: "List the global rules, or the rules of a machine (this Mac without --machine)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := tf.RequireAtMostOneTarget(); err != nil {
				return err
			}
			if filter.limit < 0 {
				return errors.New("--limit must be 0 (every rule) or more")
			}
			filter.byType = cmd.Flags().Changed("type")
			filter.byPolicy = cmd.Flags().Changed("policy")

			region, _ := cmd.Flags().GetString("region")
			table, _ := cmd.Flags().GetString("dynamodb_table")

			dynamodbClient := dynamodb.GetClient(table, region)
			if err := tf.ResolveMachineID(dynamodbClient); err != nil {
				return err
			}

			return rules(dynamodbClient, tf, filter)
		},
	}

	tf.AddTargetFlagsRules(RulesCmd)
	RulesCmd.Flags().Var(&filter.ruleType, "type", `List only rules of this type: "binary", "certificate", "teamid", "signingid" or "cdhash"`)
	RulesCmd.Flags().Var(&filter.policy, "policy", `List only rules with this policy: "allowlist", "blocklist", "silent_blocklist", "silent_gui_blocklist", "silent_tty_blocklist", "cel" or "remove"`)
	RulesCmd.Flags().IntVar(&filter.limit, "limit", 0, "List at most this many rules; 0 lists every rule")

	addRuleExportCommand()
	addRuleImportCommand()
	addRuleBackfillPolicyIndexCommand()
}

// ruleFilter selects the rules `rudolph rules` lists
type ruleFilter struct {
	ruleType flags.RuleType
	byType   bool
	policy   flags.RulePolicy
	byPolicy bool
	// limit is the most rules to list; 0 lists every rule
	limit int
}

func (f ruleFilter) matches(rule modelrules.SantaRule) bool {
	if f.byType && rule.RuleType != f.ruleType.AsRuleType() {
		return false
	}
	return !f.byPolicy || rule.Policy == f.policy.AsRulePolicy()
}

// full reports whether count rules reach the limit
func (f ruleFilter) full(count int) bool {
	return f.limit > 0 && count >= f.limit
}

// globalRulesPageSize is how many global rules are read per query
const globalRulesPageSize = 200

func rules(client dynamodb.QueryAPI, tf flags.TargetFlags, filter ruleFilter) error {
	if tf.IsGlobal {
		fmt.Println("==========================")
		fmt.Println("Retrieving global rules: ")
		fmt.Println("")

		// The policy filter reads the rules too, not the GlobalRulePolicy_SK index: rules written before the index
		// lack its attribute until rules backfill-policy-index is run
		var listed []*globalrules.GlobalRuleRow
		more := false
		var key *dynamodb.PrimaryKey
		for {
			page, next, err := globalrules.GetPaginatedGlobalRules(client, globalRulesPageSize, key)
			if err != nil {
				return fmt.Errorf("failed to get rules: %w", err)
			}
			for _, rule := range page {
				if !filter.matches(rule.SantaRule) {
					continue
				}
				if filter.full(len(listed)) {
					more = true
					break
				}
				listed = append(listed, rule)
			}
			if more || next == nil {
				break
			}
			key = next
		}

		if more {
			fmt.Printf("Showing the first %d rules (raise --limit to see more):\n", len(listed))
		} else {
			fmt.Printf("Retrieved %d rules:\n", len(listed))
		}
		for i, rule := range listed {
			printRule(i, rule.SortKey, rule.SantaRule, rule.Description, "", rule.CollectAllowedEvents)
		}
		return nil
	}

	fmt.Println("==========================")
	fmt.Printf("Retrieving for ")
	machineID, err := tf.GetMachineID()
	if err != nil {
		return fmt.Errorf("failed to get machineID: %w", err)
	}

	if tf.IsTargetSelf() {
		fmt.Printf("current machine, %s\n", machineID)
	} else {
		fmt.Printf("target machine, %s\n", machineID)
	}
	fmt.Println("")

	rows, err := machinerules.GetMachineRules(client, machineID)
	if err != nil {
		return fmt.Errorf("failed to GetMachineRules: %w", err)
	}

	var listed []machinerules.MachineRuleRow
	more := false
	for _, rule := range *rows {
		if !filter.matches(rule.SantaRule) {
			continue
		}
		if filter.full(len(listed)) {
			more = true
			break
		}
		listed = append(listed, rule)
	}
	if more {
		fmt.Printf("Showing the first %d MachineRules (raise --limit to see more):\n", len(listed))
	} else {
		fmt.Printf("Retrieved %d MachineRules:\n", len(listed))
	}
	now := time.Now()
	for i, rule := range listed {
		state := "Expires: never"
		if rule.ExpiresAfter != 0 {
			state = "Expires: " + time.Unix(rule.ExpiresAfter, 0).UTC().Format(time.RFC3339)
		}
		if !rule.AppliesAt(now) {
			state += " (no longer applies: expired or removed)"
		}
		printRule(i, rule.SortKey, rule.SantaRule, rule.Description, state, rule.CollectAllowedEvents)
	}
	return nil
}

func printRule(i int, sortKey string, rule modelrules.SantaRule, description, state string, collecting bool) {
	fmt.Println("----- [", i, "] (", sortKey, ")")
	fmt.Println(renderRule(rule))
	if description != "" {
		fmt.Println("  Description:", description)
	}
	if state != "" {
		fmt.Println(" ", state)
	}
	printCollecting(collecting)
	fmt.Println("")
}

func printCollecting(collecting bool) {
	if collecting {
		fmt.Println("  CollectAllowedEvents: true (allowed executions are collected in the events table)")
	}
}

// String returns the rule in human readable string format
func renderRule(item modelrules.SantaRule) string {
	var predicate string
	switch item.RuleType {
	case types.RuleTypeBinary:
		predicate = "binary"
	case types.RuleTypeCertificate:
		predicate = "certificate"
	case types.RuleTypeTeamID:
		predicate = "teamID"
	case types.RuleTypeSigningID:
		predicate = "signingID"
	case types.RuleTypeCDHash:
		predicate = "cdhash"
	default:
		predicate = "?"
	}

	var verb string
	switch item.Policy {
	case types.RulePolicyAllowlist:
		verb = "allows"
	case types.RulePolicyBlocklist:
		verb = "blocks"
	case types.RulePolicySilentBlocklist:
		verb = "silent blocks"
	case types.RulePolicySilentGUIBlocklist:
		verb = "blocks without GUI notification"
	case types.RulePolicySilentTTYBlocklist:
		verb = "blocks without TTY notification"
	case types.RulePolicyCEL:
		verb = "evaluates CEL for"
	case types.RulePolicyRemove:
		verb = "removes"
	case types.RulePolicyAllowlistCompiler:
		verb = "allows compiler of"
	case types.RulePolicyAllowlistTransitive:
		verb = "(not sent to hosts) allows transitive of"
	default:
		verb = "?"
	}

	// Support backwards compatibility with SHA256
	if item.SHA256 != "" && item.Identifier == "" {
		item.Identifier = item.SHA256
	}

	rendered := fmt.Sprintf("Rule %v %v (%s)", verb, predicate, item.Identifier)
	if item.CELExpr != "" {
		rendered = fmt.Sprintf("%s with CEL expression %q", rendered, item.CELExpr)
	}
	return rendered
}
