package globalrules

import (
	"fmt"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
)

func GetGlobalRuleBySortKey(client dynamodb.GetItemAPI, ruleSortKey string) (*GlobalRuleRow, error) {
	return getItemAsGlobalRule(client, globalRulesPK, ruleSortKey)
}

// GetGlobalRulesBySortKeysConsistently returns the global rules that exist among the given rule sort keys, by sort
// key, with strongly consistent reads (one BatchGetItem request per 100 keys). It is for callers that send the result
// to a Mac right away and must not act on a rule that was just changed or removed.
func GetGlobalRulesBySortKeysConsistently(client dynamodb.BatchGetItemAPI, ruleSortKeys []string) (map[string]*GlobalRuleRow, error) {
	keys := make([]dynamodb.PrimaryKey, len(ruleSortKeys))
	for i, sortKey := range ruleSortKeys {
		keys[i] = dynamodb.PrimaryKey{PartitionKey: globalRulesPK, SortKey: sortKey}
	}
	items, err := client.BatchGetItems(keys, true)
	if err != nil {
		return nil, fmt.Errorf("failed to read global rules: %w", err)
	}
	found := make(map[string]*GlobalRuleRow, len(items))
	for _, item := range items {
		var rule *GlobalRuleRow
		if err := attributevalue.UnmarshalMap(item, &rule); err != nil {
			return nil, fmt.Errorf("failed to unmarshal a global rule: %w", err)
		}
		if rule.SHA256 != "" && rule.Identifier == "" {
			rule.Identifier = rule.SHA256
		}
		found[rule.SortKey] = rule
	}
	return found, nil
}

// @deprecated Use GetGlobalRuleByIdentifier
func GetGlobalRuleByShaType(client dynamodb.GetItemAPI, sha256 string, ruleType types.RuleType) (*GlobalRuleRow, error) {
	return GetGlobalRuleByIdentifier(client, sha256, ruleType)
}

func GetGlobalRuleByIdentifier(client dynamodb.GetItemAPI, identifier string, ruleType types.RuleType) (*GlobalRuleRow, error) {
	pk := globalRulesPK
	sk := globalRulesSK(identifier, ruleType)
	return getItemAsGlobalRule(client, pk, sk)
}

func getItemAsGlobalRule(
	client dynamodb.GetItemAPI,
	partitionKey string,
	sortKey string,
) (rule *GlobalRuleRow, err error) {
	output, err := client.GetItem(
		dynamodb.PrimaryKey{
			PartitionKey: partitionKey,
			SortKey:      sortKey,
		},
		false,
	)

	if err != nil {
		return
	}

	if len(output.Item) == 0 {
		return
	}

	err = attributevalue.UnmarshalMap(output.Item, &rule)

	if err != nil {
		err = fmt.Errorf("succeeded GetItem but failed to unmarshalMap into GlobalRuleRow: %w", err)
		return
	}

	if rule.SHA256 != "" && rule.Identifier == "" {
		rule.Identifier = rule.SHA256
	}

	return
}
