package rule

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/airbnb/rudolph/internal/cli/clockcheck"
	"github.com/airbnb/rudolph/internal/cli/flags"
	"github.com/airbnb/rudolph/internal/cli/santa_sensor"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/eventcollection"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	"github.com/airbnb/rudolph/pkg/model/machinerules"
	"github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/model/sensordata"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/spf13/cobra"
)

// The `rule` command itself does not take any flags or run anything, it's
// simply a passthrough to other subcommands

var (
	RuleCmd = &cobra.Command{
		Use:   "rule",
		Short: "Perform various rule operations",
	}
)

func init() {
	// Every rule subcommand that writes checks the local clock first (see clockcheck)
	clockcheck.AddFlag(RuleCmd)
}

// createRule runs a command that creates a rule with the given policy. The target and expiry flags are checked before
// anything is read or written.
func createRule(cmd *cobra.Command, policy types.Policy, tf flags.TargetFlags, rf flags.RuleInfoFlags, ef flags.RuleExpiryFlags, options ...rules.RuleOption) error {
	if err := tf.RequireExplicitTarget(); err != nil {
		return err
	}
	timeProvider := clock.ConcreteTimeProvider{}
	expires, err := ef.Expiry(timeProvider.Now(), tf.IsGlobal)
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

	return applyPolicyForPath(timeProvider, dynamodbClient, policy, tf, rf, expires, options...)
}

// applyPolicyForPath creates a rule after a typed confirmation. A machine rule expires at expires, or never when it is
// the zero time.
func applyPolicyForPath(timeProvider clock.TimeProvider, client dynamodb.DynamoDBClient, policy types.Policy, tf flags.TargetFlags, rf flags.RuleInfoFlags, expires time.Time, options ...rules.RuleOption) (err error) {
	// Second, determine the rule type and identifier
	ruleType := (*rf.RuleType).AsRuleType()
	var description string
	var identifier string

	if *rf.FilePath != "" {
		fileInfo, err := santa_sensor.RunSantaFileInfo(*rf.FilePath)
		if err != nil {
			return fmt.Errorf("encountered an error while attempting to get file information for %q", *rf.FilePath)
		}
		identifier = fileInfo.SHA256
		description = fmt.Sprintf("%s from %s", fileInfo.Path, tf.SelfMachineID) // FIXME (derek.wang) tf.SelfMachineID is Not initialized.

		switch ruleType {
		case types.RuleTypeBinary:
			break
		case types.RuleTypeCertificate:
			if len(fileInfo.SigningChain) == 0 {
				return fmt.Errorf("NO SIGNING INFO FOUND FOR GIVEN BINARY")
			}
			if fileInfo.SigningChain[0].SHA256 == "" {
				return fmt.Errorf("NO CERTIFICATE SHA FOUND FOR GIVEN BINARY")
			}
			if fileInfo.SigningChain[0].CommonName == "" {
				return fmt.Errorf("NO CERTIFICATE NAME FOUND FOR GIVEN BINARY")
			}

			identifier = fileInfo.SigningChain[0].SHA256
			description = fmt.Sprintf("%v, by %v (%v)", fileInfo.SigningChain[0].CommonName, fileInfo.SigningChain[0].Organization, fileInfo.SigningChain[0].OrganizationalUnit)
		case types.RuleTypeTeamID:
			identifier = fileInfo.TeamID
		case types.RuleTypeSigningID:
			identifier = fileInfo.SigningID
		case types.RuleTypeCDHash:
			return fmt.Errorf("CDHash rules cannot be created from a filepath; provide the CDHash with --identifier")
		default:
			log.Printf("error (recovered): encountered unknown ruleType: (%+v)", ruleType)
			return fmt.Errorf("error (recovered): encountered unknown ruleType: (%+v)", ruleType)
		}
	} else if *rf.Identifier != "" {
		identifier = *rf.Identifier
	}

	// TODO
	// Query if there is an existing rule: and show the before/after
	// partitionKey := fmt.Sprintf("%s%s", store.MachineRulesPKPrefix, machineID)
	// ruleName :=
	// existingItem, err := store.GetRuleByPK(&store.DynamoDBPrimaryKey{
	// 	PartitionKey: partitionKey,
	// 	SortKey:      ruleName,
	// })

	policyDescription, err := policy.MarshalText()
	if err != nil {
		return
	}
	ruleTypeDescription, err := ruleType.MarshalText()
	if err != nil {
		return
	}

	// First, determine which machine to apply
	machineID := "(Global)"
	suffix := ""
	if !tf.IsGlobal {
		machineID, err = tf.GetMachineID()
		if err != nil {
			return fmt.Errorf("failed to get MachineID: %w", err)
		}
		// All args set up; send confirmation message
		if tf.IsTargetSelf() {
			suffix = " (This machine)"
		}
		warnIfMachineNeverSynced(client, machineID)
	}

	fmt.Println("Uploading the following rule:")
	fmt.Println("  MachineID:   ", machineID, suffix)
	fmt.Println("  Identifier/SHA256:      ", identifier)
	fmt.Println("  Policy:      ", policy, "  (", string(policyDescription), ")")
	fmt.Println("  RuleType:    ", ruleType, "  (", string(ruleTypeDescription), ")")
	fmt.Println("  Description: ", description)
	if !tf.IsGlobal {
		if expires.IsZero() {
			fmt.Println("  Expires:      never")
		} else {
			fmt.Println("  Expires:     ", expires.Format(time.RFC3339))
		}
	}
	printRuleOptions(options)
	fmt.Println("")
	fmt.Println(`Apply changes? (Enter: "yes" or "ok")`)
	fmt.Print("> ")

	// Read confirmation
	reader := bufio.NewReader(os.Stdin)
	text, _ := reader.ReadString('\n')
	text = strings.Replace(text, "\n", "", -1)
	if strings.ToLower(text) == "ok" || strings.ToLower(text) == "yes" {
		// Do rule creation
		if tf.IsGlobal {
			err = globalrules.AddNewGlobalRule(timeProvider, client, identifier, ruleType, policy, description, options...)
		} else {
			err = machinerules.AddNewMachineRule(client, machineID, identifier, ruleType, policy, description, expires, options...)
		}
		if err != nil {
			return fmt.Errorf("could not upload rule to DynamoDB: %w", err)
		}
		fmt.Println("Successfully sent a rule to dynamodb")
		// The new row replaced any earlier one without its CollectAllowedEvents flag
		indexMachineID := ""
		if !tf.IsGlobal {
			indexMachineID = machineID
		}
		forgetCollectedRule(client, indexMachineID, rules.RuleSortKeyFromTypeIdentifier(identifier, ruleType))
	} else {
		fmt.Println("Well ok then")
	}
	fmt.Println("")

	return
}

// warnIfMachineNeverSynced warns when nothing is known about a machine ID, which means no sensor has ever
// synced under it (in any case: see flags.ResolveMachineID). A typo in the machine ID would otherwise create a rule
// that no host ever reads, without saying anything.
func warnIfMachineNeverSynced(client dynamodb.GetItemAPI, machineID string) {
	sensorData, err := sensordata.GetSensorData(client, machineID)
	if err != nil {
		fmt.Printf("[WARNING] could not check whether %s has ever synced: %v\n", machineID, err)
		return
	}
	if sensorData == nil {
		fmt.Printf("[WARNING] no sensor has ever synced as %s. Check the machine UUID: a rule for a machine\n", machineID)
		fmt.Println("          that never syncs has no effect, and machine IDs are matched exactly, including case.")
	}
}

// printRuleOptions prints the optional rule fields that are set
func printRuleOptions(options []rules.RuleOption) {
	var rule rules.SantaRule
	rule.ApplyOptions(options...)
	if rule.CustomMessage != "" {
		fmt.Println("  CustomMsg:   ", rule.CustomMessage)
	}
	if rule.CustomURL != "" {
		fmt.Println("  CustomURL:   ", rule.CustomURL)
	}
	if rule.EventDetailButtonLabel != "" {
		fmt.Println("  ButtonLabel: ", rule.EventDetailButtonLabel)
	}
	if rule.NotificationAppName != "" {
		fmt.Println("  NotifyApp:   ", rule.NotificationAppName)
	}
	if rule.CELExpr != "" {
		fmt.Println("  CELExpr:     ", rule.CELExpr)
	}
}

// forgetCollectedRule stops collecting the allowed executions of a rule whose row was replaced or removed (see
// eventcollection.Forget). A failure only leaves the rule collected, so it is a warning.
func forgetCollectedRule(client dynamodb.DynamoDBClient, machineID string, ruleSortKey string) {
	writer, ok := client.(eventcollection.Writer)
	if !ok || ruleSortKey == "" {
		return
	}
	if err := eventcollection.Forget(writer, machineID, ruleSortKey); err != nil {
		fmt.Printf("[WARNING] the rule's allowed executions may still be collected: %v\n", err)
		fmt.Println("          switch collection off with rudolph rule update --collect-allowed-events=false")
	}
}
