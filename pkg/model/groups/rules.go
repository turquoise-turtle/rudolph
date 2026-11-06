package groups

import (
	"errors"
	"fmt"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// A group's rules are its own partition, keyed by rule sort key like global rules. They have no feed rows: they are
// sent to member Macs with the machine rules on every sync. Only the CLI writes them; the sync Lambdas only read
// them, so the partition is deliberately left out of the Lambdas' write permissions (dynamodb:LeadingKeys in
// deployments/terraform_modules/santa_api/modules/store/iam.tf), like the global rules.
const (
	groupRulesPartitionPrefix = "GroupRules#"
	groupRuleDataType         = "GroupRule"
)

// RuleRow is a group rule as stored
type RuleRow struct {
	dynamodb.PrimaryKey
	rules.SantaRule
	Description string `dynamodbav:"Description,omitempty"`
	Group       string `dynamodbav:"Group"`
	DataType    string `dynamodbav:"DataType"`
}

func rulesPartition(group string) string {
	return groupRulesPartitionPrefix + group
}

var (
	ErrRuleNotFound = errors.New("the group has no such rule")
	ErrTooManyRules = fmt.Errorf("a group has at most %d rules", MaxRulesPerGroup)
)

// ValidateRule refuses a group rule Santa could not apply, and the policies a group rule cannot have
func ValidateRule(rule rules.SantaRule) error {
	if _, err := rule.RuleType.MarshalText(); err != nil {
		return err
	}
	if _, err := rule.Policy.MarshalText(); err != nil {
		return err
	}
	if rule.Policy == types.RulePolicyRemove {
		return errors.New("a group rule cannot have the REMOVE policy; remove the rule instead")
	}
	if err := rules.ValidatePolicy(rule); err != nil {
		return err
	}
	if err := rules.ValidateFieldSizes(rule); err != nil {
		return err
	}
	if !rules.ValidIdentifier(rule.RuleType, rule.Identifier) {
		return fmt.Errorf("invalid identifier %q for this rule type", rule.Identifier)
	}
	return nil
}

// GetRules reads all of a group's rules, strongly consistently: callers cache them by the RulesVersion they read
// first, so they must not get an older set than that version.
func GetRules(client dynamodb.QueryAPI, group string) ([]RuleRow, error) {
	var all []RuleRow
	var start map[string]awstypes.AttributeValue
	for {
		out, err := client.Query(&awsdynamodb.QueryInput{
			ConsistentRead:         aws.Bool(true),
			KeyConditionExpression: aws.String("PK = :pk"),
			ExpressionAttributeValues: map[string]awstypes.AttributeValue{
				":pk": &awstypes.AttributeValueMemberS{Value: rulesPartition(group)},
			},
			ExclusiveStartKey: start,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to read the group's rules: %w", err)
		}
		var page []RuleRow
		if err := attributevalue.UnmarshalListOfMaps(out.Items, &page); err != nil {
			return nil, fmt.Errorf("failed to unmarshal the group's rules: %w", err)
		}
		all = append(all, page...)
		if len(out.LastEvaluatedKey) == 0 {
			return all, nil
		}
		start = out.LastEvaluatedKey
	}
}

// groupRuleKeys returns the keys of all of a group's rules
func groupRuleKeys(client dynamodb.QueryAPI, group string) ([]dynamodb.PrimaryKey, error) {
	rows, err := GetRules(client, group)
	if err != nil {
		return nil, err
	}
	keys := make([]dynamodb.PrimaryKey, len(rows))
	for i, row := range rows {
		keys[i] = row.PrimaryKey
	}
	return keys, nil
}

// bumpRulesVersion is the registry update that goes into the same transaction as every change to a group's rules
func bumpRulesVersion(client dynamodb.TransactUpdateExpressionAPI, group string) (*awstypes.TransactWriteItem, error) {
	return client.CreateTransactUpdateExpression(registryKey(),
		"SET #g.#n.#v = #g.#n.#v + :one", "attribute_exists(#g.#n)",
		names("#g", "Groups", "#n", group, "#v", "RulesVersion"),
		map[string]awstypes.AttributeValue{":one": number(1)})
}

// PutRule creates or replaces a rule of a registered group. A Mac receives the change at its next sync.
func PutRule(client RegistryWriter, group string, rule rules.SantaRule, description string) error {
	if err := ValidateRule(rule); err != nil {
		return err
	}
	registry, err := GetRegistry(client, true)
	if err != nil {
		return err
	}
	if _, ok := registry.Groups[group]; !ok {
		return ErrGroupNotFound
	}
	sortKey := rules.RuleSortKeyFromTypeIdentifier(rule.Identifier, rule.RuleType)
	existing, err := groupRuleKeys(client, group)
	if err != nil {
		return err
	}
	if len(existing) >= MaxRulesPerGroup {
		replacing := false
		for _, key := range existing {
			replacing = replacing || key.SortKey == sortKey
		}
		if !replacing {
			return ErrTooManyRules
		}
	}

	row := RuleRow{
		PrimaryKey:  dynamodb.PrimaryKey{PartitionKey: rulesPartition(group), SortKey: sortKey},
		SantaRule:   rule,
		Description: description,
		Group:       group,
		DataType:    groupRuleDataType,
	}
	put, err := client.CreateTransactPutItem(row)
	if err != nil {
		return err
	}
	bump, err := bumpRulesVersion(client, group)
	if err != nil {
		return err
	}
	_, err = client.TransactWriteItems([]awstypes.TransactWriteItem{*put, *bump}, nil)
	if dynamodb.IsTransactionConditionalCheckFailed(err) {
		return ErrGroupNotFound
	}
	if err != nil {
		return fmt.Errorf("failed to write the group rule: %w", err)
	}
	return nil
}

// RemoveRule deletes a group's rule. Member Macs get the global rule for the same identifier and type back, or remove
// the rule, at their next sync.
func RemoveRule(client RegistryWriter, group string, sortKey string) error {
	del, err := client.CreateTransactDeleteItem(dynamodb.PrimaryKey{PartitionKey: rulesPartition(group), SortKey: sortKey})
	if err != nil {
		return err
	}
	del.Delete.ConditionExpression = aws.String("attribute_exists(PK)")
	bump, err := bumpRulesVersion(client, group)
	if err != nil {
		return err
	}
	_, err = client.TransactWriteItems([]awstypes.TransactWriteItem{*del, *bump}, nil)
	if dynamodb.IsTransactionConditionalCheckFailed(err) {
		return ErrRuleNotFound
	}
	if err != nil {
		return fmt.Errorf("failed to remove the group rule: %w", err)
	}
	return nil
}
