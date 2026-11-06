package rules

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/spf13/cobra"

	"github.com/airbnb/rudolph/internal/cli/clockcheck"
	"github.com/airbnb/rudolph/internal/cli/flags"
	"github.com/airbnb/rudolph/internal/csv"
	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/eventcollection"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	rudolphrules "github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/types"
)

// consts for "workers" in pool for dynamodb operations
const (
	defaultWorkers = 10
	minWorkers     = 1
	maxWorkers     = 2 << defaultWorkers // 2048 default, relative to defaultWorkers
)

func addRuleImportCommand() {
	var filename string
	var format string
	var workers int

	var ruleImportCmd = &cobra.Command{
		Use:     "import -f <file-path> [--fileformat csv|json] [--workers <n>]",
		Aliases: []string{"rules-import"},
		Short:   "Create or replace global rules from a CSV or JSON file",
		Long: `Create or replace global rules from a CSV or JSON file, in the format "rules export" writes. The format is taken
from --fileformat, or else from the file's extension (.csv or .json). A rule with the REMOVE policy removes the
global rule. The import asks for confirmation first (none with --yes).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if filename == "" {
				return errors.New("pass the file to import with -f <file-path>")
			}
			fileFormat, err := importFormat(filename, format)
			if err != nil {
				return err
			}
			region, _ := cmd.Flags().GetString("region")
			table, _ := cmd.Flags().GetString("dynamodb_table")

			dynamodbClient := dynamodb.GetClient(table, region)
			if err := clockcheck.Check(cmd, dynamodbClient); err != nil {
				return err
			}

			// Try to prevent stupidity
			if workers < minWorkers || workers > maxWorkers {
				fmt.Printf("[WARNING] invalid worker count (%d); using default: %d\n", workers, defaultWorkers)
				workers = defaultWorkers
			}

			return runImportFormat(dynamodbClient, clock.ConcreteTimeProvider{}, filename, fileFormat, workers, confirmImport(dynamodbClient, filename, flags.ApprovalOf(cmd)))
		},
	}

	addFileFlags(ruleImportCmd, &filename, "The CSV or JSON file to import")
	ruleImportCmd.Flags().StringVar(&format, "fileformat", "", "File format: csv or json; by default taken from the file's extension")
	ruleImportCmd.Flags().IntVarP(&workers, "workers", "w", defaultWorkers, fmt.Sprintf("Number of rules written at the same time, between %d and %d", minWorkers, maxWorkers))
	clockcheck.AddFlag(ruleImportCmd)
	flags.AddYesFlag(ruleImportCmd)

	RulesCmd.AddCommand(ruleImportCmd)
}

// importFormat returns the format of the file to import: format if given, else the file's extension
func importFormat(filename, format string) (string, error) {
	if format == "" {
		switch strings.ToLower(filepath.Ext(filename)) {
		case ".csv":
			return "csv", nil
		case ".json":
			return "json", nil
		}
		return "", fmt.Errorf("cannot tell the format of %s from its extension: name it .csv or .json, or pass --fileformat csv|json", filename)
	}
	format = strings.ToLower(format)
	if format != "csv" && format != "json" {
		return "", fmt.Errorf("--fileformat %q: must be csv or json", format)
	}
	return format, nil
}

// runImport imports a file in the format its extension names
func runImport(
	client dynamodb.DynamoDBClient,
	timeProvider clock.TimeProvider,
	filename string,
	numWorkers int,
) error {
	format, err := importFormat(filename, "")
	if err != nil {
		return err
	}
	return runImportFormat(client, timeProvider, filename, format, numWorkers, nil)
}

// runImportFormat imports a file in the given format. confirm, unless nil, is asked with the rules read from the file
// before anything is written; unless it returns true, nothing is written. A dry run (false without an error) still
// fails on records of the file that are not rules.
func runImportFormat(
	client dynamodb.DynamoDBClient,
	timeProvider clock.TimeProvider,
	filename string,
	format string,
	numWorkers int,
	confirm func([]fileRule) (bool, error),
) error {
	var rules []fileRule
	var parseErrors []error
	var err error
	if format == "json" {
		rules, err = readJsonRules(filename)
	} else {
		rules, parseErrors, err = readCsvRules(filename)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", filename, err)
	}
	if confirm != nil {
		apply, err := confirm(rules)
		if err != nil {
			return err
		}
		if !apply {
			if len(parseErrors) == 0 {
				return nil
			}
			for _, parseErr := range parseErrors {
				fmt.Println("[ERROR]", parseErr)
			}
			return fmt.Errorf("%d record(s) of %s are not rules: %w", len(parseErrors), filename, errors.Join(parseErrors...))
		}
	}

	result := &importResult{}
	for _, parseErr := range parseErrors {
		result.fail(parseErr)
	}
	buffer := make(chan fileRule)
	wg := startWriters(client, timeProvider, buffer, result, numWorkers)
	for _, rule := range rules {
		buffer <- rule
	}
	close(buffer)
	wg.Wait()

	return result.report()
}

// confirmImport shows how many rules the file creates or replaces and removes, and asks before they are written. A dry
// run also lists each rule and what the import would do with it.
func confirmImport(client dynamodb.BatchGetItemAPI, filename string, approval flags.Approval) func([]fileRule) (bool, error) {
	return func(rules []fileRule) (bool, error) {
		removals := 0
		for _, rule := range rules {
			if rule.Policy == types.RulePolicyRemove {
				removals++
			}
		}
		fmt.Printf("Importing %d global rules from %s: %d to create or replace, %d to remove.\n", len(rules), filename, len(rules)-removals, removals)
		fmt.Println("Rules already stored as in the file are not written again.")
		fmt.Println()
		if approval.DryRun {
			if err := printImportPlan(client, rules); err != nil {
				return false, err
			}
		}
		return approval.Confirm()
	}
}

// printImportPlan lists what an import would do with each rule of the file, reading the stored rules as the import does
func printImportPlan(client dynamodb.BatchGetItemAPI, rules []fileRule) error {
	for _, rule := range rules {
		ruleName := rudolphrules.RuleSortKeyFromTypeIdentifier(rule.Identifier, rule.RuleType)
		switch rule.Policy {
		case types.RulePolicyAllowlistTransitive:
			fmt.Printf("  Skip (ALLOWLIST_TRANSITIVE): %s\n", ruleName)
		case types.RulePolicyRemove:
			fmt.Printf("  Remove: %s\n", ruleName)
		default:
			current, unchanged, err := storedRule(client, rule)
			if err != nil {
				return fmt.Errorf("rule %s: %w", ruleName, err)
			}
			action := "Create"
			switch {
			case unchanged:
				action = "Unchanged (not written)"
			case current != nil:
				action = "Replace"
			}
			policy, _ := rule.Policy.MarshalText()
			fmt.Printf("  %s: %s %s\n", action, ruleName, policy)
		}
	}
	fmt.Println()
	return nil
}

func readJsonRules(filename string) ([]fileRule, error) {
	contents, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	var rules []fileRule
	if err := json.Unmarshal(contents, &rules); err != nil {
		return nil, err
	}
	return rules, nil
}

// readCsvRules reads the rules of a CSV file; a record that is not a rule is returned as an error, the others are
// still imported
func readCsvRules(filename string) (rules []fileRule, parseErrors []error, err error) {
	data, err := csv.ParseCsvFile(filename)
	if err != nil {
		return nil, nil, err
	}
	var record int
	for line := range data {
		record++
		rule, err := parseCsvRule(line)
		if err != nil {
			parseErrors = append(parseErrors, fmt.Errorf("record %d: %w", record, err))
			continue
		}
		rules = append(rules, rule)
	}
	return rules, parseErrors, nil
}

// parseCsvRule converts a csv line into a rule
func parseCsvRule(line map[string]string) (rule fileRule, err error) {
	identifier, ok := line["identifier"]
	if !ok {
		return rule, errors.New("no identifier")
	}
	ruleTypeStr, ok := line["type"]
	if !ok {
		return rule, errors.New("no type")
	}
	policyStr, ok := line["policy"]
	if !ok {
		return rule, errors.New("no policy")
	}

	var ruleType types.RuleType
	if err = ruleType.UnmarshalText([]byte(ruleTypeStr)); err != nil {
		return rule, fmt.Errorf("invalid rule type: %w", err)
	}
	var policy types.Policy
	if err = policy.UnmarshalText([]byte(policyStr)); err != nil {
		return rule, fmt.Errorf("invalid policy: %w", err)
	}

	// Optional columns default to empty
	return fileRule{
		Identifier:             identifier,
		RuleType:               ruleType,
		Policy:                 policy,
		Description:            line["description"],
		CustomMessage:          line["custom_msg"],
		CustomURL:              line["custom_url"],
		EventDetailButtonLabel: line["event_detail_button_label"],
		NotificationAppName:    line["notification_app_name"],
		CELExpr:                line["cel_expr"],
	}, nil
}

// importResult tracks the outcome of an import across workers
type importResult struct {
	// Counters are incremented with atomic.Add
	total     uint64
	skipped   uint64
	unchanged uint64

	mu     sync.Mutex
	errors []error
}

func (r *importResult) fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, err)
}

// report prints the outcome and returns an error if any rule could not be imported.
// Skipped legacy ALLOWLIST_TRANSITIVE rules alone are not an error.
func (r *importResult) report() error {
	fmt.Println("processed lines:", r.total)
	if r.unchanged > 0 {
		fmt.Printf("unchanged rules (not written): %d\n", r.unchanged)
	}
	if r.skipped > 0 {
		fmt.Printf("[WARNING] skipped %d rule(s) with the ALLOWLIST_TRANSITIVE policy: %v\n", r.skipped, rudolphrules.ErrTransitivePolicy)
	}
	if len(r.errors) == 0 {
		return nil
	}
	for _, err := range r.errors {
		fmt.Println("[ERROR]", err)
	}
	return fmt.Errorf("%d rule(s) could not be imported: %w", len(r.errors), errors.Join(r.errors...))
}

// storedRuleUnchanged reports whether the global rule is already stored with every field the file sets. The read is
// strongly consistent, so a change written moments ago by someone else is not mistaken for the file's version.
// storedRule returns the global rule stored for the imported rule, or nil, and whether writing the imported rule would
// change it.
func storedRule(client dynamodb.BatchGetItemAPI, rule fileRule) (*globalrules.GlobalRuleRow, bool, error) {
	sortKey := rudolphrules.RuleSortKeyFromTypeIdentifier(rule.Identifier, rule.RuleType)
	stored, err := globalrules.GetGlobalRulesBySortKeysConsistently(client, []string{sortKey})
	if err != nil {
		return nil, false, err
	}
	current := stored[sortKey]
	if current == nil || current.SHA256 != "" || current.RuleType != rule.RuleType || current.Identifier != rule.Identifier {
		return current, false, nil
	}
	want := rudolphrules.SantaRule{RuleType: rule.RuleType, Policy: rule.Policy, Identifier: rule.Identifier}
	want.ApplyOptions(rule.options()...)
	return current, current.SantaRule == want && current.Description == rule.Description, nil
}

func startWriters(
	client dynamodb.DynamoDBClient,
	timeProvider clock.TimeProvider,
	rules chan fileRule,
	result *importResult,
	numWorkers int,
) *sync.WaitGroup {
	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done() // ensure Done is called after this worker is complete
			ddbWriter(
				client,
				timeProvider,
				rules,
				result,
			)
		}()
	}
	return &wg
}

func ddbWriter(
	client dynamodb.DynamoDBClient,
	timeProvider clock.TimeProvider,
	rules chan fileRule,
	result *importResult,
) {
	for rule := range rules {
		var err error
		atomic.AddUint64(&result.total, 1)

		var suffix string
		switch rule.RuleType {
		case types.RuleTypeCertificate:
			suffix = " (Cert)"
		case types.RuleTypeTeamID:
			suffix = " (TeamID)"
		case types.RuleTypeSigningID:
			suffix = " (SigningID)"
		case types.RuleTypeCDHash:
			suffix = " (CDHash)"
		default:
			suffix = ""
		}

		switch rule.Policy {
		case types.RulePolicyAllowlistTransitive:
			// Legacy rows in older backups; Santa has no such policy, so skip them instead of failing the import
			fmt.Printf("  [WARNING] Skipping rule with the ALLOWLIST_TRANSITIVE policy: %s%s\n", rule.Identifier, suffix)
			atomic.AddUint64(&result.skipped, 1)
			continue
		case types.RulePolicyRemove:
			fmt.Printf("  Removing rule: [%s]\n", rule.Identifier)
			sortkey := rudolphrules.RuleSortKeyFromTypeIdentifier(rule.Identifier, rule.RuleType)
			err = globalrules.RemoveGlobalRule(
				timeProvider,
				client,
				client,
				sortkey,
				"",
			)
			if err == nil {
				// A removed rule's allowed executions are no longer collected, also if it is imported again later
				err = forgetCollectedRule(client, sortkey)
			}
		default:
			// Writing a rule that is already stored as it is would add a feed row every Mac downloads again, and for
			// Macs whose postflights do not arrive such rows hold back the feed cursor (see docs/rule-pagination.md)
			current, unchanged, checkErr := storedRule(client, rule)
			if checkErr != nil {
				result.fail(fmt.Errorf("rule %s%s: %w", rule.Identifier, suffix, checkErr))
				continue
			}
			if unchanged {
				atomic.AddUint64(&result.unchanged, 1)
				continue
			}
			fmt.Printf("  Writing rule: [%+v] %s%s\n", rule.Policy, rule.Identifier, suffix)
			err = globalrules.AddNewGlobalRule(
				timeProvider,
				client,
				rule.Identifier,
				rule.RuleType,
				rule.Policy,
				rule.Description,
				rule.options()...,
			)
			if err == nil && current != nil && current.CollectAllowedEvents {
				// The new row replaced the old one: keep collecting the rule's allowed executions
				err = keepCollectingRule(client, current.PrimaryKey, timeProvider)
			}
		}

		if err != nil {
			result.fail(fmt.Errorf("rule %s%s: %w", rule.Identifier, suffix, err))
		}
	}
}

func forgetCollectedRule(client dynamodb.DynamoDBClient, ruleSortKey string) error {
	writer, ok := client.(eventcollection.Writer)
	if !ok {
		return nil
	}
	return eventcollection.Forget(writer, "", ruleSortKey)
}

func keepCollectingRule(client dynamodb.DynamoDBClient, ruleKey dynamodb.PrimaryKey, timeProvider clock.TimeProvider) error {
	writer, ok := client.(eventcollection.Writer)
	if !ok {
		return errors.New("this DynamoDB client cannot keep the rule's allowed executions collected")
	}
	if err := eventcollection.SetCollectAllowedEvents(writer, "", ruleKey, true, timeProvider.Now()); err != nil {
		return fmt.Errorf("the rule was written, but its allowed executions are no longer collected: %w", err)
	}
	return nil
}
