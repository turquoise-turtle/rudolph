package rules

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/spf13/cobra"

	"github.com/airbnb/rudolph/internal/cli/clockcheck"
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
	var workers int

	var ruleImportCmd = &cobra.Command{
		Use:     "import <file-name>",
		Aliases: []string{"rules-import"},
		Short:   "Imports riles from a csv file",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// args[0] has already been validated as a file before this
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

			return runImport(dynamodbClient, clock.ConcreteTimeProvider{}, filename, workers)
		},
	}

	ruleImportCmd.Flags().StringVarP(&filename, "filename", "f", "", "The filename")
	ruleImportCmd.Flags().IntVarP(&workers, "workers", "w", defaultWorkers, "Number of workers")
	clockcheck.AddFlag(ruleImportCmd)
	_ = ruleImportCmd.MarkFlagRequired("filename")

	RulesCmd.AddCommand(ruleImportCmd)
}

func runImport(
	client dynamodb.DynamoDBClient,
	timeProvider clock.TimeProvider,
	filename string,
	numWorkers int,
) error {
	if strings.HasSuffix(filename, ".csv") {
		return runCsvImport(client, timeProvider, filename, numWorkers)
	} else if strings.HasSuffix(filename, ".json") {
		return runJsonImport(client, timeProvider, filename, numWorkers)
	}

	return errors.New("unrecognized file extension")
}

func runJsonImport(
	client dynamodb.DynamoDBClient,
	timeProvider clock.TimeProvider,
	filename string,
	numWorkers int,
) (err error) {
	fp, err := os.Open(filename)
	if err != nil {
		return
	}
	defer fp.Close()
	contents, err := ioutil.ReadAll(fp)
	if err != nil {
		return
	}

	var rules []fileRule
	err = json.Unmarshal(contents, &rules)
	if err != nil {
		return
	}

	// Start the workers
	result := &importResult{}
	rulesBuffer := make(chan fileRule)
	wg := startWriters(client, timeProvider, rulesBuffer, result, numWorkers)

	// Shovel all the json-parsed rules into the worker queue
	for _, rule := range rules {
		rulesBuffer <- rule
	}
	close(rulesBuffer)

	// Chill
	wg.Wait()

	return result.report()
}

func runCsvImport(
	client dynamodb.DynamoDBClient,
	timeProvider clock.TimeProvider,
	filename string,
	numWorkers int,
) error {
	// ParseCsvFile returns a data channel and an optional error if any issues
	// occurred while opening the file for reading
	data, err := csv.ParseCsvFile(filename)
	if err != nil {
		return err
	}

	// Channel for csv parsing and workers to communicate over
	// Fanning out workers allows us to make multiple HTTP requests concurrently which can
	// improve performance assuming we aren't network I/O bottlenecked or something.
	result := &importResult{}
	rules := make(chan fileRule)
	wg := startWriters(client, timeProvider, rules, result, numWorkers)

	// Start taking lines from the csv and shoveling them into the workers
	var record int
	for line := range data {
		record++
		rule, err := parseCsvRule(line)
		if err != nil {
			result.fail(fmt.Errorf("record %d: %w", record, err))
			continue
		}
		rules <- rule
	}
	close(rules)

	// chill
	wg.Wait()

	return result.report()
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
