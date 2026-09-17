package rules

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/airbnb/rudolph/internal/csv"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	rudolphrules "github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/types"
)

func addRuleExportCommand() {
	var filename string
	var format string
	var ruleExportCmd = &cobra.Command{
		Use:     "export  <file-name>",
		Aliases: []string{"rules-export"},
		Short:   "Export all rules into a csv file",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// args[0] has already been validated as a file before this
			region, _ := cmd.Flags().GetString("region")
			table, _ := cmd.Flags().GetString("dynamodb_table")

			dynamodbClient := dynamodb.GetClient(table, region)

			return runExport(dynamodbClient, filename, format)
		},
	}

	ruleExportCmd.Flags().StringVarP(&filename, "filename", "f", "", "The filename")
	_ = ruleExportCmd.MarkFlagRequired("filename")

	ruleExportCmd.Flags().StringVarP(&format, "fileformat", "t", "csv", "File format (one of: [json|csv])")

	RulesCmd.AddCommand(ruleExportCmd)
}

func runExport(
	client dynamodb.QueryAPI,
	filename string,
	format string,
) (err error) {
	switch format {
	case "json":
		return runJsonExport(client, filename)
	case "csv":
		return runCsvExport(client, filename)
	}
	return
}

type fileRule struct {
	RuleType               types.RuleType `json:"type"`
	Policy                 types.Policy   `json:"policy"`
	Identifier             string         `json:"identifier"`
	CustomMessage          string         `json:"custom_msg,omitempty"`
	CustomURL              string         `json:"custom_url,omitempty"`
	EventDetailButtonLabel string         `json:"event_detail_button_label,omitempty"`
	NotificationAppName    string         `json:"notification_app_name,omitempty"`
	CELExpr                string         `json:"cel_expr,omitempty"`
	Description            string         `json:"description"`
}

// options returns the optional rule fields of a file rule
func (r fileRule) options() []rudolphrules.RuleOption {
	return []rudolphrules.RuleOption{
		rudolphrules.WithCustomMessage(r.CustomMessage),
		rudolphrules.WithCustomURL(r.CustomURL),
		rudolphrules.WithEventDetailButtonLabel(r.EventDetailButtonLabel),
		rudolphrules.WithNotificationAppName(r.NotificationAppName),
		rudolphrules.WithCELExpr(r.CELExpr),
	}
}

func runJsonExport(client dynamodb.QueryAPI, filename string) (err error) {
	var jsonRules []fileRule
	fmt.Println("Querying rules from DynamoDB...")
	total, err := getRules(client, func(rule globalrules.GlobalRuleRow) (err error) {
		jsonRules = append(jsonRules, fileRule{
			Identifier:             rule.Identifier,
			RuleType:               rule.RuleType,
			Policy:                 rule.Policy,
			CustomMessage:          rule.CustomMessage,
			CustomURL:              rule.CustomURL,
			EventDetailButtonLabel: rule.EventDetailButtonLabel,
			NotificationAppName:    rule.NotificationAppName,
			CELExpr:                rule.CELExpr,
			Description:            rule.Description,
		})
		return
	})
	if err != nil {
		return
	}

	jsondata, err := json.MarshalIndent(jsonRules, "", "  ")
	if err != nil {
		return
	}
	err = os.WriteFile(filename, jsondata, 0644)
	if err != nil {
		return
	}

	fmt.Printf("rules discovered: %d, rules written: %d\n", total, len(jsonRules))

	return
}

func runCsvExport(
	client dynamodb.QueryAPI,
	filename string,
) (err error) {
	csvRules := make(chan []string)

	// Same column names as read by the import command
	header := []string{
		"identifier",
		"type",
		"policy",
		"custom_msg",
		"custom_url",
		"event_detail_button_label",
		"notification_app_name",
		"cel_expr",
		"description",
	}

	wg, err := csv.WriteCsvFile(filename, header, csvRules)
	if err != nil {
		panic(err)
	}

	fmt.Println("Querying rules from DynamoDB...")
	var totalWritten int64
	total, err := getRules(client, func(rule globalrules.GlobalRuleRow) (err error) {
		ruleType, err := rule.RuleType.MarshalText()
		if err != nil {
			return
		}
		policy, err := rule.Policy.MarshalText()
		if err != nil {
			return
		}
		record := []string{
			rule.Identifier,
			string(ruleType),
			string(policy),
			rule.CustomMessage,
			rule.CustomURL,
			rule.EventDetailButtonLabel,
			rule.NotificationAppName,
			rule.CELExpr,
			rule.Description,
		}
		if err != nil {
			return
		}

		totalWritten += 1
		csvRules <- record
		return
	})
	if err != nil {
		return
	}

	close(csvRules)
	wg.Wait()

	fmt.Printf("rules discovered: %d, rules written: %d\n", total, totalWritten)

	return
}

func getRules(client dynamodb.QueryAPI, callback func(globalrules.GlobalRuleRow) error) (total int64, err error) {
	var key *dynamodb.PrimaryKey
	for {
		rules, nextkey, inerr := globalrules.GetPaginatedGlobalRules(client, 50, key)
		if inerr != nil {
			err = fmt.Errorf("something went wrong querying global rules: %w", inerr)
			return
		}
		if len(rules) == 0 {
			break
		}

		for _, rule := range rules {
			total += 1
			err = callback(*rule)
			if err != nil {
				return
			}
		}

		if nextkey == nil {
			break
		}
		key = nextkey
	}
	return
}
