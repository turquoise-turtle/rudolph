package rule

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/airbnb/rudolph/internal/cli/clockcheck"
	"github.com/airbnb/rudolph/internal/cli/flags"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/eventcollection"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	"github.com/airbnb/rudolph/pkg/model/machineconfiguration"
	"github.com/airbnb/rudolph/pkg/model/machinerules"
	"github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/spf13/cobra"
)

type ruleHandler struct {
	timeProvider       clock.TimeProvider
	dynamodbClient     dynamodb.DynamoDBClient
	globalRuleUpdater  globalrules.GlobalRulesUpdater
	machineRuleUpdater machinerules.MachineRulesUpdater
}

func init() {
	tf := flags.TargetFlags{}
	rf := flags.RuleInfoFlags{}
	ru := flags.RuleUpdateFlags{}

	ruleHandler := ruleHandler{}

	var ruleUpdateCmd = &cobra.Command{
		Use:   "update (-f <file-path>|-i <identifier>) -t <rule-type> (-m <machine-id>|--global) [-p <policy>] [--expires <duration-or-time>|--permanent] [--custom-msg <text>] [--custom-url <url>] [--event-detail-button-label <text>] [--notification-app-name <name>] [--collect-allowed-events=true|false]",
		Short: "Change an existing rule's policy, expiry, messages or event collection",
		Long: `Change an existing rule. Only what is given changes; the rest of the rule is kept. An empty message or URL
(e.g. --custom-msg "") clears it. A rule cannot be updated to the CEL policy or removed with -p: use "rule cel" and
"rule remove". A group rule is changed by writing it again with "rule <policy> ... --group <group>".`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := tf.RequireExplicitTarget(); err != nil {
				return err
			}
			if tf.IsGroup() {
				return errors.New(`a group rule is changed by writing it again (e.g. "rule deny ... --group <group>"), which replaces it`)
			}
			if err := ru.RequireChange(); err != nil {
				return err
			}
			if err := rf.RequireOne(); err != nil {
				return err
			}
			timeProvider := clock.ConcreteTimeProvider{}
			// When the rule expires after the update; the zero time keeps the current expiry
			var newExpiry *time.Time
			if ru.ExpiryChanged() {
				expires, err := ru.Expiry.Expiry(timeProvider.Now(), tf.IsGlobal)
				if err != nil {
					return err
				}
				newExpiry = &expires
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
			if err := clockcheck.Check(cmd, client); err != nil {
				return err
			}

			ruleHandler.dynamodbClient = client
			ruleHandler.timeProvider = timeProvider
			ruleHandler.globalRuleUpdater = globalrules.ConcreteGlobalRulesUpdater{
				ClockProvider: timeProvider,
				TransactWrite: client,
			}
			ruleHandler.machineRuleUpdater = machinerules.ConcreteMachineRulesUpdater{
				Updater:      client,
				TimeProvider: timeProvider,
			}

			return ruleHandler.updateRule(tf, rf, ru, newExpiry, flags.ApprovalOf(cmd))
		},
	}

	tf.AddRuleTargetFlags(ruleUpdateCmd)
	ru.AddRuleUpdateFlags(ruleUpdateCmd)
	rf.AddRuleInfoFlags(ruleUpdateCmd)

	RuleCmd.AddCommand(ruleUpdateCmd)
}

// existingRule is what updateRule needs of a global or machine rule
type existingRule struct {
	key         dynamodb.PrimaryKey
	rule        rules.SantaRule
	description string
	// expires is when a machine rule expires; the zero time for one that never expires and for global rules
	expires    time.Time
	collecting bool
	// removing is a machine rule that is being removed (marked DeleteOnNextSync, or the REMOVE policy)
	removing bool
	// The row as stored, written again with its new fields by a rewrite
	globalRow  *globalrules.GlobalRuleRow
	machineRow *machinerules.MachineRuleRow
}

func (rh *ruleHandler) readRule(global bool, machineID, identifier string, ruleType types.RuleType) (*existingRule, error) {
	sortKey := rules.RuleSortKeyFromTypeIdentifier(identifier, ruleType)
	if global {
		row, err := globalrules.GetGlobalRuleByShaType(rh.dynamodbClient, identifier, ruleType)
		if err != nil {
			return nil, err
		}
		if row == nil {
			return nil, fmt.Errorf("there is no global rule %s", sortKey)
		}
		return &existingRule{key: row.PrimaryKey, rule: row.SantaRule, description: row.Description, collecting: row.CollectAllowedEvents, globalRow: row}, nil
	}
	row, err := machinerules.GetMachineRuleByShaType(rh.dynamodbClient, machineID, identifier, ruleType)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, fmt.Errorf("machine %s has no rule %s", machineID, sortKey)
	}
	// An update cannot bring back a rule that has expired (and that DynamoDB has not deleted yet): it would look
	// successful and change nothing on any Mac.
	if row.ExpiresAfter != 0 && row.ExpiresAfter <= rh.timeProvider.Now().Unix() {
		return nil, fmt.Errorf("the rule %s of machine %s expired at %s; create it again with --expires or --permanent", sortKey, machineID, time.Unix(row.ExpiresAfter, 0).UTC().Format(time.RFC3339))
	}
	existing := &existingRule{key: row.PrimaryKey, rule: row.SantaRule, description: row.Description, collecting: row.CollectAllowedEvents, machineRow: row,
		removing: row.DeleteOnNextSync || row.Policy == types.RulePolicyRemove}
	if row.ExpiresAfter != 0 {
		existing.expires = time.Unix(row.ExpiresAfter, 0).UTC()
	}
	return existing, nil
}

func printRule(heading, machineID, suffix string, rule rules.SantaRule, description string, global bool, expires time.Time, collecting bool) {
	policyDescription, _ := rule.Policy.MarshalText()
	ruleTypeDescription, _ := rule.RuleType.MarshalText()
	fmt.Println(heading)
	fmt.Println("  MachineID:   ", machineID, suffix)
	fmt.Println("  Identifier/SHA256:      ", rule.Identifier)
	fmt.Println("  Policy:      ", rule.Policy, "  (", string(policyDescription), ")")
	fmt.Println("  RuleType:    ", rule.RuleType, "  (", string(ruleTypeDescription), ")")
	fmt.Println("  Description: ", description)
	if !global {
		fmt.Println("  Expires:     ", expiryText(expires))
	}
	printRuleOptions([]rules.RuleOption{
		rules.WithCustomMessage(rule.CustomMessage),
		rules.WithCustomURL(rule.CustomURL),
		rules.WithEventDetailButtonLabel(rule.EventDetailButtonLabel),
		rules.WithNotificationAppName(rule.NotificationAppName),
		rules.WithCELExpr(rule.CELExpr),
	})
	fmt.Println("  CollectAllowedEvents:", collecting)
}

// updateRule changes what the flags give of an existing global or machine rule, after a confirmation (none with
// --yes, nothing written with --dry-run). newExpiry is when a machine rule expires after the update, or nil to keep
// its expiry.
func (rh *ruleHandler) updateRule(tf flags.TargetFlags, rf flags.RuleInfoFlags, ru flags.RuleUpdateFlags, newExpiry *time.Time, approval flags.Approval) error {
	ruleType := rf.RuleType.AsRuleType()
	identifier, _, err := identifyRule(rf, ruleType)
	if err != nil {
		return err
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
	}

	current, err := rh.readRule(tf.IsGlobal, machineID, identifier, ruleType)
	if err != nil {
		return err
	}
	if current.removing {
		// Updating it would bring back a rule the Mac is told to drop
		return fmt.Errorf("the rule %s of machine %s is being removed; create it again with rudolph rule <policy> instead of updating it", current.key.SortKey, machineID)
	}

	updated := current.rule
	if ru.PolicyChanged() {
		updated.Policy = ru.RulePolicy.AsRulePolicy()
		if updated.Policy != current.rule.Policy {
			// Only CEL rules may have an expression
			updated.CELExpr = ""
		}
	}
	ru.ApplyOptions(&updated)
	expires := current.expires
	if newExpiry != nil {
		expires = *newExpiry
	}
	collecting := current.collecting
	if ru.CollectAllowedEventsChanged() {
		collecting = *ru.CollectAllowedEvents
	}

	policyChanged := updated.Policy != current.rule.Policy
	// What a policy update alone stores; any other change writes the rule row again
	policyUpdated := current.rule
	if policyChanged {
		policyUpdated = withPolicy(current.rule, updated.Policy)
	}
	rewrite := updated != policyUpdated || !expires.Equal(current.expires)
	collectionChanged := collecting != current.collecting
	if !policyChanged && !rewrite && !collectionChanged {
		// Not an error: a command copied from rudolph-web (e.g. collection off) may already have been applied
		fmt.Printf("The rule %s already is as given; nothing was changed.\n", current.key.SortKey)
		return nil
	}

	printRule("The current rule:", machineID, suffix, current.rule, current.description, tf.IsGlobal, current.expires, current.collecting)
	fmt.Println("")
	printRule("Updating the rule to:", machineID, suffix, updated, current.description, tf.IsGlobal, expires, collecting)
	if collectionChanged && collecting {
		warnIfAllEventUploadOff(rh.dynamodbClient)
	}
	fmt.Println("")
	if apply, err := approval.Confirm(); !apply {
		return err
	}

	switch {
	case rewrite:
		// One conditional write keeps the row's other attributes (CollectAllowedEvents among them) and does not
		// bring back a rule removed since it was read
		if tf.IsGlobal {
			row := *current.globalRow
			row.SantaRule = updated
			err = globalrules.ReplaceGlobalRule(rh.timeProvider, rh.dynamodbClient, row)
		} else {
			writer, ok := rh.dynamodbClient.(dynamodb.PutItemIfAPI)
			if !ok {
				return errors.New("this DynamoDB client cannot write rules conditionally")
			}
			row := *current.machineRow
			row.SantaRule = updated
			row.ExpiresAfter = 0
			if !expires.IsZero() {
				row.ExpiresAfter = expires.Unix()
			}
			err = machinerules.ReplaceMachineRule(writer, row)
		}
	case policyChanged:
		if tf.IsGlobal {
			err = rh.globalRuleUpdater.UpdateGlobalRule(identifier, ruleType, updated.Policy)
		} else {
			err = rh.machineRuleUpdater.UpdateMachineRulePolicy(machineID, identifier, ruleType, updated.Policy)
		}
	}
	if err != nil {
		return fmt.Errorf("could not update the rule in DynamoDB: %w", err)
	}
	if collectionChanged {
		if err := rh.setCollectAllowedEvents(tf, machineID, current.key, collecting); err != nil {
			return err
		}
	}
	if tf.IsGlobal {
		fmt.Printf("Updated global rule %s\n", current.key.SortKey)
	} else {
		fmt.Printf("Updated rule %s of machine %s, expires %s\n", current.key.SortKey, machineID, expiryText(expires))
	}
	var changes []string
	if policyChanged {
		was, _ := current.rule.Policy.MarshalText()
		changes = append(changes, fmt.Sprintf("policy was %s", was))
	}
	if updated != policyUpdated {
		changes = append(changes, "rule options changed")
	}
	if !expires.Equal(current.expires) {
		changes = append(changes, "expires "+expiryText(expires))
	}
	if collectionChanged {
		changes = append(changes, fmt.Sprintf("collect allowed events %t", collecting))
	}
	target := flags.GlobalTarget
	if !tf.IsGlobal {
		target = flags.MachineTarget(machineID)
	}
	return flags.Record(rh.dynamodbClient, "rule.update", target, ruleSummary(updated.RuleType, updated.Identifier, updated.Policy, strings.Join(changes, ", ")))
}

// withPolicy returns rule with the given policy, as a policy update stores it (UpdateGlobalRule,
// UpdateMachineRulePolicy): the expression is cleared, the other fields are kept
func withPolicy(rule rules.SantaRule, policy types.Policy) rules.SantaRule {
	rule.Policy = policy
	rule.CELExpr = ""
	return rule
}

func (rh *ruleHandler) setCollectAllowedEvents(tf flags.TargetFlags, machineID string, ruleKey dynamodb.PrimaryKey, collect bool) error {
	writer, ok := rh.dynamodbClient.(eventcollection.Writer)
	if !ok {
		return errors.New("this DynamoDB client cannot change whether allowed events are collected")
	}
	indexMachineID := ""
	if !tf.IsGlobal {
		indexMachineID = machineID
	}
	err := eventcollection.SetCollectAllowedEvents(writer, indexMachineID, ruleKey, collect, rh.timeProvider.Now())
	if errors.Is(err, eventcollection.ErrRuleNotFound) {
		return errors.New("the rule was removed, or the machine rule expired, while it was being updated")
	}
	return err
}

// warnIfAllEventUploadOff warns that flagging a rule collects nothing from Macs where Santa does not upload allowed
// executions. Only the global config is checked: machine configs have no index, and the warning names the per-Mac
// setting.
func warnIfAllEventUploadOff(client dynamodb.GetItemAPI) {
	global, err := machineconfiguration.GetGlobalConfig(client)
	if err != nil {
		fmt.Printf("[WARNING] could not read the global config to check EnableAllEventUpload: %v\n", err)
		return
	}
	if global != nil && global.EnableAllEventUpload != nil && *global.EnableAllEventUpload {
		return
	}
	fmt.Println("[WARNING] Santa uploads allowed executions only with EnableAllEventUpload; it is off in the global")
	fmt.Println("          configuration. Enable it for the Macs to watch with")
	fmt.Println("          rudolph config update --machine <id> --enable-all-event-upload")
}
