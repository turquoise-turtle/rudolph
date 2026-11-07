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
// disappears. The item is only a hint: a rule row can be deleted by the TTL, or rewritten, without it, so the reader
// confirms each rule on its row (and takes stale entries out).
package eventcollection

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

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

// Index is the set of flagged rules, by rule sort key as stored. It is a hint: a rule removed, re-created or expired
// since may still be listed, so a reader confirms a rule on its row before acting on it.
type Index struct {
	Global   map[string]bool
	Machines map[string]map[string]bool

	// By MatchKey, the stored sort keys that match it.
	globalByMatch   map[string][]string
	machinesByMatch map[string]map[string][]string
}

// Empty reports whether no rule is flagged.
func (ix Index) Empty() bool {
	return len(ix.Global) == 0 && len(ix.Machines) == 0
}

// hashRulePrefixes are the sort key prefixes of rules whose identifier is a hash. Rules stored by older versions may
// have an upper-case hash, while Santa reports lower case.
var hashRulePrefixes = []string{"Binary#", "Cert#", "CDHash#"}

// MatchKey is the form of a rule sort key that events are matched on: hash identifiers in lower case, others as they
// are (team IDs and signing IDs are case-sensitive).
func MatchKey(ruleSortKey string) string {
	for _, prefix := range hashRulePrefixes {
		if strings.HasPrefix(ruleSortKey, prefix) {
			return prefix + strings.ToLower(strings.TrimPrefix(ruleSortKey, prefix))
		}
	}
	return ruleSortKey
}

// Candidates returns the stored sort keys of the flagged global rules, and of the Mac's flagged machine rules, that
// match the rule sort key of an event (see MatchKey).
func (ix Index) Candidates(machineID, ruleSortKey string) (global, machine []string) {
	if ruleSortKey == "" {
		return nil, nil
	}
	key := MatchKey(ruleSortKey)
	return ix.globalByMatch[key], ix.machinesByMatch[machineID][key]
}

// ParseIndex reads the index item; a nil item (no rule ever flagged) is an empty index. Attributes it does not know
// are ignored.
func ParseIndex(item map[string]awsdynamodbtypes.AttributeValue) Index {
	ix := Index{
		Global:          map[string]bool{},
		Machines:        map[string]map[string]bool{},
		globalByMatch:   map[string][]string{},
		machinesByMatch: map[string]map[string][]string{},
	}
	for name, value := range item {
		set, ok := value.(*awsdynamodbtypes.AttributeValueMemberSS)
		if !ok {
			continue
		}
		switch {
		case name == globalAttribute:
			for _, sk := range set.Value {
				ix.Global[sk] = true
				ix.globalByMatch[MatchKey(sk)] = append(ix.globalByMatch[MatchKey(sk)], sk)
			}
		case strings.HasPrefix(name, machineAttributePrefix):
			machineID := strings.TrimPrefix(name, machineAttributePrefix)
			rules := map[string]bool{}
			byMatch := map[string][]string{}
			for _, sk := range set.Value {
				rules[sk] = true
				byMatch[MatchKey(sk)] = append(byMatch[MatchKey(sk)], sk)
			}
			ix.Machines[machineID] = rules
			ix.machinesByMatch[machineID] = byMatch
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

// ErrRuleNotFound is returned by SetCollectAllowedEvents when the rule row does not exist, or is a machine rule that
// has expired.
var ErrRuleNotFound = errors.New("no such rule exists, or the machine rule has expired")

// IndexAttribute is the index attribute holding the flagged rules of the target: a Mac, or the global rules when
// machineID is empty.
func IndexAttribute(machineID string) string {
	if machineID == "" {
		return globalAttribute
	}
	return machineAttributePrefix + machineID
}

// SetCollectAllowedEvents sets or clears CollectAllowedEvents on an existing rule row (ruleKey: a global rule when
// machineID is empty, else the Mac's machine rule) and updates the index in the same transaction. The rule's other
// attributes, the rules feed and what Santa downloads are left as they are.
//
// A machine rule is only flagged while it has not expired at now: an expired rule allows nothing on the Mac.
func SetCollectAllowedEvents(client Writer, machineID string, ruleKey dynamodb.PrimaryKey, collect bool, now time.Time) error {
	ruleUpdate := "REMOVE #flag"
	condition := "attribute_exists(PK)"
	names := map[string]string{"#flag": RuleAttribute}
	var ruleValues map[string]awsdynamodbtypes.AttributeValue
	if collect {
		ruleUpdate = "SET #flag = :true"
		ruleValues = map[string]awsdynamodbtypes.AttributeValue{":true": &awsdynamodbtypes.AttributeValueMemberBOOL{Value: true}}
		if machineID != "" {
			condition += " AND (attribute_not_exists(#expires) OR #expires = :zero OR #expires > :now)"
			names["#expires"] = "ExpiresAfter"
			ruleValues[":zero"] = &awsdynamodbtypes.AttributeValueMemberN{Value: "0"}
			ruleValues[":now"] = &awsdynamodbtypes.AttributeValueMemberN{Value: strconv.FormatInt(now.Unix(), 10)}
		}
	}
	rule, err := client.CreateTransactUpdateExpression(ruleKey, ruleUpdate, condition, names, ruleValues)
	if err != nil {
		return err
	}

	operation := "DELETE"
	if collect {
		operation = "ADD"
	}
	index, err := client.CreateTransactUpdateExpression(IndexKey(), operation+" #rules :rule", "",
		map[string]string{"#rules": IndexAttribute(machineID)},
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
		map[string]string{"#rules": IndexAttribute(machineID)},
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

// ForgetMachine takes all of a Mac's machine rules out of the index (rudolph machine forget). It writes nothing when
// there is no index item.
func ForgetMachine(client dynamodb.UpdateExpressionAPI, machineID string) error {
	_, err := client.UpdateItemExpression(IndexKey(), "REMOVE #rules", "attribute_exists(PK)",
		map[string]string{"#rules": IndexAttribute(machineID)}, nil)
	if err != nil && !dynamodb.IsConditionalCheckFailed(err) {
		return fmt.Errorf("failed to update the rules whose allowed events are collected: %w", err)
	}
	return nil
}
