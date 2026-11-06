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

// bumpRulesVersion is the registry update that goes into the same transaction as every change to a group's rules. It
// gives the group the next version from the registry-wide counter (see Registry.nextRulesVersion), conditional on
// the counter not having moved since registry was read, so that no version is ever issued twice.
func bumpRulesVersion(client dynamodb.TransactUpdateExpressionAPI, registry *Registry, group string) (*awstypes.TransactWriteItem, error) {
	values := map[string]awstypes.AttributeValue{":next": number(registry.nextRulesVersion())}
	return client.CreateTransactUpdateExpression(registryKey(),
		"SET #g.#n.#v = :next, #c = :next", "attribute_exists(#g.#n) AND "+registry.counterUnchanged(values),
		names("#g", "Groups", "#n", group, "#v", "RulesVersion", "#c", "RulesVersionCounter"),
		values)
}

// changeRules runs a transaction holding item and the version bump of group, reading the registry again and retrying
// when another writer issued a version meanwhile. It returns ErrGroupNotFound when the group is not registered, and
// errItemCondition when the registry did not change but the transaction's condition still failed (item's own).
func changeRules(client RegistryWriter, group string, item awstypes.TransactWriteItem, errItemCondition error) error {
	for attempt := 0; attempt < 5; attempt++ {
		registry, err := GetRegistry(client, true)
		if err != nil {
			return err
		}
		if _, ok := registry.Groups[group]; !ok {
			return ErrGroupNotFound
		}
		bump, err := bumpRulesVersion(client, registry, group)
		if err != nil {
			return err
		}
		_, err = client.TransactWriteItems([]awstypes.TransactWriteItem{item, *bump}, nil)
		if err == nil {
			return nil
		}
		if !dynamodb.IsTransactionConditionalCheckFailed(err) {
			return err
		}
		after, rerr := GetRegistry(client, true)
		if rerr != nil {
			return rerr
		}
		if _, ok := after.Groups[group]; !ok {
			return ErrGroupNotFound
		}
		if after.RulesVersionCounter == registry.RulesVersionCounter && after.nextRulesVersion() == registry.nextRulesVersion() {
			return errItemCondition
		}
	}
	return ErrRegistryChanged
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
	// Not atomic with the write: two operators adding rules at once can take a group slightly past the limit
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
	if err := changeRules(client, group, *put, ErrRegistryChanged); err != nil {
		if errors.Is(err, ErrGroupNotFound) || errors.Is(err, ErrRegistryChanged) {
			return err
		}
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
	if err := changeRules(client, group, *del, ErrRuleNotFound); err != nil {
		if errors.Is(err, ErrGroupNotFound) || errors.Is(err, ErrRuleNotFound) || errors.Is(err, ErrRegistryChanged) {
			return err
		}
		return fmt.Errorf("failed to remove the group rule: %w", err)
	}
	return nil
}
