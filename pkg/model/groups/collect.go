package groups

import (
	"errors"
	"fmt"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/eventcollection"
	"github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// A group rule can be flagged CollectAllowedEvents like a global or machine rule (see pkg/model/eventcollection and
// docs/events-table.md): eventupload then stores the executions the rule allows on the Macs of the group. The flagged
// rules of a group are listed in the collection index's Group#<group> set, which this package keeps in step with the
// rule rows.

// ErrNotAnAllowRule is returned by SetCollectAllowedEvents when asked to flag a rule that allows nothing.
var ErrNotAnAllowRule = errors.New("only an allow rule (ALLOWLIST or ALLOWLIST_COMPILER) can have its allowed executions collected")

// IsAllow reports whether a group rule's policy allows executions, so that the rule can be flagged.
func IsAllow(policy types.Policy) bool {
	return policy == types.RulePolicyAllowlist || policy == types.RulePolicyAllowlistCompiler
}

// BeatsAllow reports whether a rule of another of a Mac's groups wins over a group allow for the same key (see
// Resolve): a block or a CEL rule that Santa can apply. The allow then did not decide the execution.
func BeatsAllow(rule rules.SantaRule) bool {
	return Applicable(rule) && strength[rule.Policy] > strength[types.RulePolicyAllowlist]
}

// GetRule reads one rule of a group, strongly consistently; nil when the group has no such rule.
func GetRule(client dynamodb.GetItemAPI, group, ruleSortKey string) (*RuleRow, error) {
	out, err := client.GetItem(dynamodb.PrimaryKey{PartitionKey: rulesPartition(group), SortKey: ruleSortKey}, true)
	if err != nil {
		return nil, fmt.Errorf("failed to read the group rule: %w", err)
	}
	if len(out.Item) == 0 {
		return nil, nil
	}
	var row RuleRow
	if err := attributevalue.UnmarshalMap(out.Item, &row); err != nil {
		return nil, fmt.Errorf("failed to unmarshal the group rule: %w", err)
	}
	return &row, nil
}

// SetCollectAllowedEvents sets or clears CollectAllowedEvents on an existing rule of a registered group and updates
// the group's set in the collection index in the same transaction. The rule's other attributes and the group's
// RulesVersion are left as they are: Santa never sees the flag, so no Mac needs to download anything.
//
// Only an allow can be flagged (ErrNotAnAllowRule); clearing is accepted whatever the policy. It returns
// ErrGroupNotFound for a group that is not registered and ErrRuleNotFound for a rule the group does not have, and
// writes nothing then.
func SetCollectAllowedEvents(client RegistryWriter, group, ruleSortKey string, collect bool) error {
	refusal := func() error {
		registry, err := GetRegistry(client, true)
		if err != nil {
			return err
		}
		if _, ok := registry.Groups[group]; !ok {
			return ErrGroupNotFound
		}
		row, err := GetRule(client, group, ruleSortKey)
		if err != nil {
			return err
		}
		if row == nil {
			return ErrRuleNotFound
		}
		if collect && !IsAllow(row.Policy) {
			return ErrNotAnAllowRule
		}
		return nil
	}
	if err := refusal(); err != nil {
		return err
	}

	// The conditions hold what was just read against a rule removed, or written again as a block, in the meantime
	update, condition := "REMOVE #flag", "attribute_exists(PK)"
	attrs := names("#flag", eventcollection.RuleAttribute)
	var values map[string]awstypes.AttributeValue
	operation := "DELETE"
	if collect {
		update, condition = "SET #flag = :true", "attribute_exists(PK) AND #policy IN (:allow, :compiler)"
		attrs["#policy"] = "Policy"
		allow, err := types.RulePolicyAllowlist.MarshalDynamoDBAttributeValue()
		if err != nil {
			return err
		}
		compiler, err := types.RulePolicyAllowlistCompiler.MarshalDynamoDBAttributeValue()
		if err != nil {
			return err
		}
		values = map[string]awstypes.AttributeValue{":true": &awstypes.AttributeValueMemberBOOL{Value: true}, ":allow": allow, ":compiler": compiler}
		operation = "ADD"
	}
	rule, err := client.CreateTransactUpdateExpression(dynamodb.PrimaryKey{PartitionKey: rulesPartition(group), SortKey: ruleSortKey}, update, condition, attrs, values)
	if err != nil {
		return err
	}
	index, err := client.CreateTransactUpdateExpression(eventcollection.IndexKey(), operation+" #rules :rule", "",
		names("#rules", eventcollection.GroupIndexAttribute(group)),
		map[string]awstypes.AttributeValue{":rule": &awstypes.AttributeValueMemberSS{Value: []string{ruleSortKey}}})
	if err != nil {
		return err
	}
	_, err = client.TransactWriteItems([]awstypes.TransactWriteItem{*rule, *index}, nil)
	if dynamodb.IsTransactionConditionalCheckFailed(err) {
		if err := refusal(); err != nil {
			return err
		}
		return ErrRegistryChanged
	}
	if err != nil {
		return fmt.Errorf("failed to change whether the group rule's allowed events are collected: %w", err)
	}
	return nil
}

// withIndexRemoval returns write, the write that replaces or deletes a group's rule, followed by the removal of the
// rule from the collection index when the index lists it: the row it named no longer exists as flagged. Nothing is
// added for a rule that is not listed, so that group rules never create the index item.
func withIndexRemoval(client RegistryWriter, group, ruleSortKey string, write awstypes.TransactWriteItem) ([]awstypes.TransactWriteItem, error) {
	items := []awstypes.TransactWriteItem{write}
	index, err := eventcollection.ReadIndex(client)
	if err != nil {
		return nil, err
	}
	if !index.Groups[group][ruleSortKey] {
		return items, nil
	}
	removal, err := client.CreateTransactUpdateExpression(eventcollection.IndexKey(), "DELETE #rules :rule", "",
		names("#rules", eventcollection.GroupIndexAttribute(group)),
		map[string]awstypes.AttributeValue{":rule": &awstypes.AttributeValueMemberSS{Value: []string{ruleSortKey}}})
	if err != nil {
		return nil, err
	}
	return append(items, *removal), nil
}
