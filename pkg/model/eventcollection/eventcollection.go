// Package eventcollection keeps the index of rules whose allowed executions are stored in the execution events table
// (rule rows with CollectAllowedEvents set; see docs/events-table.md).
//
// Santa has no per-rule upload switch, so the eventupload Lambda decides which allowed-by-rule events to store. To
// avoid a read per event it reads one small item of the store table, PK "EventCollection", SK "Rules", which holds
// the flagged rules' sort keys (e.g. "TeamID#EQHXZ8M8AV"):
//
//   - Global: a string set, the flagged global rules;
//   - Machine#<machine ID>: a string set per Mac, its flagged machine rules.
//
// The CLI changes a rule's flag and this item in one transaction, with ADD and DELETE on the sets, which DynamoDB
// applies atomically: concurrent CLI runs flagging different rules never lose each other's changes. A set left empty
// disappears.
package eventcollection

import (
	"errors"
	"fmt"
	"strings"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	awsdynamodbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	indexPartitionKey      = "EventCollection"
	indexSortKey           = "Rules"
	globalAttribute        = "Global"
	machineAttributePrefix = "Machine#"

	// RuleAttribute is the rule row attribute that says the rule's allowed executions are collected.
	RuleAttribute = "CollectAllowedEvents"
)

// IndexKey is the key of the index item in the store table.
func IndexKey() dynamodb.PrimaryKey {
	return dynamodb.PrimaryKey{PartitionKey: indexPartitionKey, SortKey: indexSortKey}
}

// Index is the set of flagged rules, by rule sort key.
type Index struct {
	Global   map[string]bool
	Machines map[string]map[string]bool
}

// Empty reports whether no rule is flagged.
func (ix Index) Empty() bool {
	return len(ix.Global) == 0 && len(ix.Machines) == 0
}

// Collects reports whether events allowed by the rule with this sort key are stored for this Mac: the global rule is
// flagged, or the Mac's own machine rule is.
func (ix Index) Collects(machineID, ruleSortKey string) bool {
	if ruleSortKey == "" {
		return false
	}
	return ix.Global[ruleSortKey] || ix.Machines[machineID][ruleSortKey]
}

// ParseIndex reads the index item; a nil item (no rule ever flagged) is an empty index. Attributes it does not know
// are ignored.
func ParseIndex(item map[string]awsdynamodbtypes.AttributeValue) Index {
	ix := Index{Global: map[string]bool{}, Machines: map[string]map[string]bool{}}
	for name, value := range item {
		set, ok := value.(*awsdynamodbtypes.AttributeValueMemberSS)
		if !ok {
			continue
		}
		switch {
		case name == globalAttribute:
			for _, sk := range set.Value {
				ix.Global[sk] = true
			}
		case strings.HasPrefix(name, machineAttributePrefix):
			machineID := strings.TrimPrefix(name, machineAttributePrefix)
			rules := map[string]bool{}
			for _, sk := range set.Value {
				rules[sk] = true
			}
			ix.Machines[machineID] = rules
		}
	}
	return ix
}

// ReadIndex reads the index item from the store table.
func ReadIndex(client dynamodb.GetItemAPI) (Index, error) {
	out, err := client.GetItem(IndexKey(), true)
	if err != nil {
		return Index{}, fmt.Errorf("failed to read the rules whose allowed events are collected: %w", err)
	}
	return ParseIndex(out.Item), nil
}

// Writer is what changing a flag needs.
type Writer interface {
	dynamodb.GetItemAPI
	dynamodb.TransactWriteItemsAPI
	dynamodb.TransactUpdateExpressionAPI
}

// ErrRuleNotFound is returned by SetCollectAllowedEvents when the rule row does not exist.
var ErrRuleNotFound = errors.New("no such rule exists")

// indexAttribute is the index attribute holding the flagged rules of the target: a Mac, or the global rules when
// machineID is empty.
func indexAttribute(machineID string) string {
	if machineID == "" {
		return globalAttribute
	}
	return machineAttributePrefix + machineID
}

// SetCollectAllowedEvents sets or clears CollectAllowedEvents on an existing rule row (ruleKey: a global rule when
// machineID is empty, else the Mac's machine rule) and updates the index in the same transaction. The rule's other
// attributes, the rules feed and what Santa downloads are left as they are.
func SetCollectAllowedEvents(client Writer, machineID string, ruleKey dynamodb.PrimaryKey, collect bool) error {
	ruleUpdate := "REMOVE #flag"
	var ruleValues map[string]awsdynamodbtypes.AttributeValue
	if collect {
		ruleUpdate = "SET #flag = :true"
		ruleValues = map[string]awsdynamodbtypes.AttributeValue{":true": &awsdynamodbtypes.AttributeValueMemberBOOL{Value: true}}
	}
	rule, err := client.CreateTransactUpdateExpression(ruleKey, ruleUpdate, "attribute_exists(PK)",
		map[string]string{"#flag": RuleAttribute}, ruleValues)
	if err != nil {
		return err
	}

	operation := "DELETE"
	if collect {
		operation = "ADD"
	}
	index, err := client.CreateTransactUpdateExpression(IndexKey(), operation+" #rules :rule", "",
		map[string]string{"#rules": indexAttribute(machineID)},
		map[string]awsdynamodbtypes.AttributeValue{":rule": &awsdynamodbtypes.AttributeValueMemberSS{Value: []string{ruleKey.SortKey}}})
	if err != nil {
		return err
	}

	_, err = client.TransactWriteItems([]awsdynamodbtypes.TransactWriteItem{*rule, *index}, nil)
	if dynamodb.IsTransactionConditionalCheckFailed(err) {
		return ErrRuleNotFound
	}
	if err != nil {
		return fmt.Errorf("failed to change whether the rule's allowed events are collected: %w", err)
	}
	return nil
}

// Forget takes a rule out of the index, after its row was replaced or removed, so that it is no longer collected. It
// writes nothing unless the rule is in the index.
func Forget(client Writer, machineID string, ruleSortKey string) error {
	ix, err := ReadIndex(client)
	if err != nil {
		return err
	}
	if !(machineID == "" && ix.Global[ruleSortKey]) && !(machineID != "" && ix.Machines[machineID][ruleSortKey]) {
		return nil
	}
	index, err := client.CreateTransactUpdateExpression(IndexKey(), "DELETE #rules :rule", "attribute_exists(PK)",
		map[string]string{"#rules": indexAttribute(machineID)},
		map[string]awsdynamodbtypes.AttributeValue{":rule": &awsdynamodbtypes.AttributeValueMemberSS{Value: []string{ruleSortKey}}})
	if err != nil {
		return err
	}
	_, err = client.TransactWriteItems([]awsdynamodbtypes.TransactWriteItem{*index}, nil)
	if err != nil && !dynamodb.IsTransactionConditionalCheckFailed(err) {
		return fmt.Errorf("failed to update the rules whose allowed events are collected: %w", err)
	}
	return nil
}
