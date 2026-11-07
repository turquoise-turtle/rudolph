package rule

import (
	"errors"
	"fmt"
	"time"

	"github.com/airbnb/rudolph/internal/cli/clockcheck"
	"github.com/airbnb/rudolph/internal/cli/flags"
	"github.com/airbnb/rudolph/internal/cli/santa_sensor"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/eventcollection"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	"github.com/airbnb/rudolph/pkg/model/groups"
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
	flags.AddYesFlag(RuleCmd)
	flags.AddDryRunFlag(RuleCmd)
}

// ruleUse is the usage line of a command that creates a rule; extra is inserted after the target
func ruleUse(verb, extra string) string {
	use := verb + " (-f <file-path>|-i <identifier>) -t <rule-type> (-m <machine-id>|--global|--group <group>|--ungrouped)"
	if extra != "" {
		use += " " + extra
	}
	return use
}

// createRule runs a command that creates a rule with the given policy. The target, the identifier and the expiry
// flags are checked before anything is read or written.
func createRule(cmd *cobra.Command, policy types.Policy, tf flags.TargetFlags, rf flags.RuleInfoFlags, ef flags.RuleExpiryFlags, options ...rules.RuleOption) error {
	if err := tf.RequireExplicitTarget(); err != nil {
		return err
	}
	if err := rf.RequireOne(); err != nil {
		return err
	}
	timeProvider := clock.ConcreteTimeProvider{}
	var expires time.Time
	var err error
	if tf.IsGroup() {
		err = ef.RefuseForGroup()
	} else {
		expires, err = ef.Expiry(timeProvider.Now(), tf.IsGlobal)
	}
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

	return applyPolicyForPath(timeProvider, dynamodbClient, policy, tf, rf, expires, flags.ApprovalOf(cmd), options...)
}

// identifyRule returns the identifier of the rule given with --identifier, or read with santactl from the file given
// with --filepath, and a description of the file
func identifyRule(rf flags.RuleInfoFlags, ruleType types.RuleType) (identifier string, description string, err error) {
	if err := rf.RequireOne(); err != nil {
		return "", "", err
	}
	if *rf.FilePath == "" {
		return *rf.Identifier, "", nil
	}
	if ruleType == types.RuleTypeCDHash {
		return "", "", errors.New("CDHash rules cannot be created from a file path; give the CDHash with --identifier")
	}
	fileInfo, err := santa_sensor.RunSantaFileInfo(*rf.FilePath)
	if err != nil {
		return "", "", fmt.Errorf("could not read the file information of %q: %w", *rf.FilePath, err)
	}
	description = fileInfo.Path
	if self, err := santa_sensor.GetSelfMachineID(); err == nil {
		description = fmt.Sprintf("%s from %s", fileInfo.Path, self)
	}

	switch ruleType {
	case types.RuleTypeBinary:
		identifier = fileInfo.SHA256
	case types.RuleTypeCertificate:
		if len(fileInfo.SigningChain) == 0 || fileInfo.SigningChain[0].SHA256 == "" {
			return "", "", fmt.Errorf("%s is not signed, so it has no certificate to make a rule for", *rf.FilePath)
		}
		if fileInfo.SigningChain[0].CommonName == "" {
			return "", "", fmt.Errorf("the signing certificate of %s has no common name", *rf.FilePath)
		}
		identifier = fileInfo.SigningChain[0].SHA256
		description = fmt.Sprintf("%v, by %v (%v)", fileInfo.SigningChain[0].CommonName, fileInfo.SigningChain[0].Organization, fileInfo.SigningChain[0].OrganizationalUnit)
	case types.RuleTypeTeamID:
		identifier = fileInfo.TeamID
	case types.RuleTypeSigningID:
		identifier = fileInfo.SigningID
	default:
		return "", "", fmt.Errorf("unknown rule type %d", ruleType)
	}
	if identifier == "" {
		return "", "", fmt.Errorf("santactl found no %s for %s", ruleTypeName(ruleType), *rf.FilePath)
	}
	return identifier, description, nil
}

// applyPolicyForPath creates a rule after a confirmation (none with --yes, nothing written with --dry-run). A machine
// rule expires at expires, or never when it is the zero time.
func applyPolicyForPath(timeProvider clock.TimeProvider, client dynamodb.DynamoDBClient, policy types.Policy, tf flags.TargetFlags, rf flags.RuleInfoFlags, expires time.Time, approval flags.Approval, options ...rules.RuleOption) (err error) {
	ruleType := (*rf.RuleType).AsRuleType()
	identifier, description, err := identifyRule(rf, ruleType)
	if err != nil {
		return err
	}

	policyDescription, err := policy.MarshalText()
	if err != nil {
		return
	}
	ruleTypeDescription, err := ruleType.MarshalText()
	if err != nil {
		return
	}

	if tf.IsGroup() {
		return applyGroupRule(client, policy, tf.Group, ruleType, identifier, description, approval, options...)
	}

	machineID := "(Global)"
	suffix := ""
	if !tf.IsGlobal {
		machineID, err = tf.GetMachineID()
		if err != nil {
			return fmt.Errorf("failed to get MachineID: %w", err)
		}
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
		fmt.Println("  Expires:     ", expiryText(expires))
	}
	printRuleOptions(options)
	fmt.Println("")
	if apply, err := approval.Confirm(); !apply {
		return err
	}

	if tf.IsGlobal {
		err = globalrules.AddNewGlobalRule(timeProvider, client, identifier, ruleType, policy, description, options...)
	} else {
		err = machinerules.AddNewMachineRule(client, machineID, identifier, ruleType, policy, description, expires, options...)
	}
	if err != nil {
		return fmt.Errorf("could not upload rule to DynamoDB: %w", err)
	}
	if tf.IsGlobal {
		fmt.Printf("Added %s %s rule %s for every machine\n", policyDescription, ruleTypeDescription, identifier)
	} else {
		fmt.Printf("Added %s %s rule %s for machine %s, expires %s\n", policyDescription, ruleTypeDescription, identifier, machineID, expiryText(expires))
	}
	// The new row replaced any earlier one without its CollectAllowedEvents flag
	indexMachineID := ""
	if !tf.IsGlobal {
		indexMachineID = machineID
	}
	forgetCollectedRule(client, indexMachineID, rules.RuleSortKeyFromTypeIdentifier(identifier, ruleType))
	if tf.IsGlobal {
		return flags.Record(client, "rule.add", flags.GlobalTarget, ruleSummary(ruleType, identifier, policy, ""))
	}
	return flags.Record(client, "rule.add", flags.MachineTarget(machineID), ruleSummary(ruleType, identifier, policy, "expires "+expiryText(expires)))
}

// ruleSummary describes a rule for the audit trail, e.g. "BINARY rule ab12... BLOCKLIST"; extra is appended after a
// comma
func ruleSummary(ruleType types.RuleType, identifier string, policy types.Policy, extra string) string {
	policyName, err := policy.MarshalText()
	if err != nil {
		policyName = []byte(fmt.Sprint(policy))
	}
	summary := fmt.Sprintf("%s rule %s %s", ruleTypeName(ruleType), identifier, policyName)
	if extra != "" {
		summary += ", " + extra
	}
	return summary
}

// expiryText formats when a machine rule expires; the zero time is never
func expiryText(expires time.Time) string {
	if expires.IsZero() {
		return "never"
	}
	return expires.UTC().Format(time.RFC3339)
}

// ruleTypeName returns Santa's name of a rule type, e.g. TEAMID
func ruleTypeName(ruleType types.RuleType) string {
	name, err := ruleType.MarshalText()
	if err != nil {
		return fmt.Sprint(ruleType)
	}
	return string(name)
}

// applyGroupRule creates or replaces a device group's rule after a confirmation (none with --yes, nothing written
// with --dry-run)
func applyGroupRule(client dynamodb.DynamoDBClient, policy types.Policy, group string, ruleType types.RuleType, identifier, description string, approval flags.Approval, options ...rules.RuleOption) error {
	writer, ok := client.(groups.RegistryWriter)
	if !ok {
		return errors.New("this DynamoDB client cannot write group rules")
	}
	rule := rules.SantaRule{RuleType: ruleType, Policy: policy, Identifier: identifier}
	rule.ApplyOptions(options...)
	if err := groups.ValidateRule(rule); err != nil {
		return err
	}
	registry, err := groups.GetRegistry(writer, true)
	if err != nil {
		return err
	}
	if _, ok := registry.Groups[group]; !ok {
		if group != groups.Ungrouped {
			return fmt.Errorf("no group %q; create it first with rudolph group create", group)
		}
	}

	policyDescription, _ := policy.MarshalText()
	ruleTypeDescription, _ := ruleType.MarshalText()
	fmt.Println("Uploading the following rule:")
	if group == groups.Ungrouped {
		fmt.Println("  Group:        (Macs in no group)")
	} else {
		fmt.Println("  Group:       ", group)
	}
	fmt.Println("  Identifier/SHA256:      ", identifier)
	fmt.Println("  Policy:      ", policy, "  (", string(policyDescription), ")")
	fmt.Println("  RuleType:    ", ruleType, "  (", string(ruleTypeDescription), ")")
	fmt.Println("  Description: ", description)
	printRuleOptions(options)
	fmt.Println("")
	if apply, err := approval.Confirm(); !apply {
		return err
	}
	createdUngrouped := false
	if _, ok := registry.Groups[group]; !ok {
		// The reserved group exists as soon as it has a rule; only operators' own groups need creating
		err := groups.CreateGroup(clock.ConcreteTimeProvider{}, writer, groups.Ungrouped, "Macs in no group", false)
		if err != nil && !errors.Is(err, groups.ErrGroupExists) {
			return err
		}
		createdUngrouped = err == nil
	}
	if err := groups.PutRule(writer, group, rule, description); err != nil {
		err = fmt.Errorf("could not upload rule to DynamoDB: %w", err)
		if createdUngrouped {
			return flags.RecordPartial(client, "rule.add", flags.GroupTarget(group), ruleSummary(ruleType, identifier, policy, "registered the reserved group of Macs in no group"), err)
		}
		return err
	}
	target := fmt.Sprintf("group %s", group)
	if group == groups.Ungrouped {
		target = "the Macs in no group"
	}
	fmt.Printf("Added %s %s rule %s for %s\n", policyDescription, ruleTypeDescription, identifier, target)
	return flags.Record(client, "rule.add", flags.GroupTarget(group), ruleSummary(ruleType, identifier, policy, ""))
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
