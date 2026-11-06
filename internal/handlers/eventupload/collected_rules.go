package eventupload

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/airbnb/rudolph/pkg/model/eventcollection"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	"github.com/airbnb/rudolph/pkg/model/machinerules"
)

// collectedRulesCacheFor is how long a warm Lambda keeps what it read about flagged rules (the index, and whether each
// rule row confirms its flag): a change made with the CLI reaches the events table within this time.
const collectedRulesCacheFor = 60 * time.Second

const (
	globalRulesPartition   = "GlobalRules"
	machineRulesPartPrefix = "MachineRules#"
)

type collectionAPI interface {
	GetItem(ctx context.Context, in *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	TransactWriteItems(ctx context.Context, in *dynamodb.TransactWriteItemsInput, optFns ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error)
}

// collectedRules decides which allowed-by-rule executions are stored. The index of flagged rules
// (pkg/model/eventcollection) only names candidates: a rule listed there is collected only once its row confirms it
// (it exists, is flagged and, for a machine rule, still applies), as the index can outlive a row deleted by the TTL or
// rewritten without the flag. Both reads are cached for collectedRulesCacheFor. A failed read is logged and the
// executions it concerns are not stored; it never fails the upload.
type collectedRules struct {
	api   collectionAPI
	table string
	now   func() time.Time

	mu        sync.Mutex
	index     eventcollection.Index
	indexAt   time.Time
	hasIndex  bool
	confirmed map[ruleRowKey]confirmation
}

type ruleRowKey struct{ pk, sk string }

type confirmation struct {
	collects bool
	at       time.Time
}

func newCollectedRules(api collectionAPI, storeTable string) *collectedRules {
	return &collectedRules{api: api, table: storeTable, now: time.Now, confirmed: map[ruleRowKey]confirmation{}}
}

func fresh(at, now time.Time) bool {
	return !now.Before(at) && now.Sub(at) < collectedRulesCacheFor
}

func (c *collectedRules) readIndex(ctx context.Context) (eventcollection.Index, error) {
	c.mu.Lock()
	if c.hasIndex && fresh(c.indexAt, c.now()) {
		defer c.mu.Unlock()
		return c.index, nil
	}
	c.mu.Unlock()

	key := eventcollection.IndexKey()
	out, err := c.api.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(c.table),
		Key:            map[string]types.AttributeValue{"PK": str(key.PartitionKey), "SK": str(key.SortKey)},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return eventcollection.Index{}, err
	}
	index := eventcollection.ParseIndex(out.Item)
	c.mu.Lock()
	c.index, c.indexAt, c.hasIndex = index, c.now(), true
	c.mu.Unlock()
	return index, nil
}

// confirm reports whether the rule row at key exists, is flagged and (machine rules) applies now. A row that is gone
// or not flagged is taken out of the index.
func (c *collectedRules) confirm(ctx context.Context, machineID string, key ruleRowKey) (bool, error) {
	c.mu.Lock()
	if cached, ok := c.confirmed[key]; ok && fresh(cached.at, c.now()) {
		c.mu.Unlock()
		return cached.collects, nil
	}
	c.mu.Unlock()

	out, err := c.api.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(c.table),
		Key:            map[string]types.AttributeValue{"PK": str(key.pk), "SK": str(key.sk)},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return false, err
	}
	now := c.now()
	exists, flagged, collects := len(out.Item) > 0, false, false
	if exists {
		if machineID == "" {
			var row globalrules.GlobalRuleRow
			if err := attributevalue.UnmarshalMap(out.Item, &row); err != nil {
				return false, err
			}
			flagged = row.CollectAllowedEvents
			collects = flagged
		} else {
			var row machinerules.MachineRuleRow
			if err := attributevalue.UnmarshalMap(out.Item, &row); err != nil {
				return false, err
			}
			flagged = row.CollectAllowedEvents
			collects = flagged && row.AppliesAt(now)
		}
	}
	c.mu.Lock()
	c.confirmed[key] = confirmation{collects: collects, at: now}
	c.mu.Unlock()
	if !flagged {
		c.forget(ctx, machineID, key)
	}
	return collects, nil
}

// forget takes a rule whose row is gone or not flagged out of the index, unless its row has been flagged in the
// meantime (the condition check), so a concurrent CLI run flagging it again is never undone. Best effort: a failure
// only leaves the stale entry, which the next confirmation reads again.
func (c *collectedRules) forget(ctx context.Context, machineID string, key ruleRowKey) {
	index := eventcollection.IndexKey()
	_, err := c.api.TransactWriteItems(ctx, &dynamodb.TransactWriteItemsInput{TransactItems: []types.TransactWriteItem{
		{ConditionCheck: &types.ConditionCheck{
			TableName:                aws.String(c.table),
			Key:                      map[string]types.AttributeValue{"PK": str(key.pk), "SK": str(key.sk)},
			ConditionExpression:      aws.String("attribute_not_exists(PK) OR attribute_not_exists(#flag) OR #flag = :false"),
			ExpressionAttributeNames: map[string]string{"#flag": eventcollection.RuleAttribute},
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":false": &types.AttributeValueMemberBOOL{Value: false},
			},
		}},
		{Update: &types.Update{
			TableName:                aws.String(c.table),
			Key:                      map[string]types.AttributeValue{"PK": str(index.PartitionKey), "SK": str(index.SortKey)},
			UpdateExpression:         aws.String("DELETE #rules :rule"),
			ConditionExpression:      aws.String("attribute_exists(PK)"),
			ExpressionAttributeNames: map[string]string{"#rules": eventcollection.IndexAttribute(machineID)},
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":rule": &types.AttributeValueMemberSS{Value: []string{key.sk}},
			},
		}},
	}})
	if err != nil {
		log.Printf("eventupload: could not take a rule that no longer collects out of the collection index: %v", err)
	}
}

// collectionLookup answers, for one upload, whether executions allowed by a rule are stored.
type collectionLookup struct {
	rules     *collectedRules
	machineID string

	index     *eventcollection.Index
	failed    bool
	decisions map[string]bool
}

// lookup starts the lookups of one upload of this Mac. A nil collectedRules collects nothing.
func (c *collectedRules) lookup(machineID string) *collectionLookup {
	return &collectionLookup{rules: c, machineID: machineID, decisions: map[string]bool{}}
}

func (l *collectionLookup) collects(ctx context.Context, ruleSortKey string) bool {
	if l.rules == nil || l.failed {
		return false
	}
	matchKey := eventcollection.MatchKey(ruleSortKey)
	if decided, ok := l.decisions[matchKey]; ok {
		return decided
	}
	if l.index == nil {
		index, err := l.rules.readIndex(ctx)
		if err != nil {
			// Never logs the rule: it identifies what the Mac runs.
			log.Printf("eventupload: could not read which rules collect allowed executions; not storing those of machine %s in this upload: %v", l.machineID, err)
			l.failed = true
			return false
		}
		l.index = &index
	}
	global, machine := l.index.Candidates(l.machineID, ruleSortKey)
	collects := false
	for _, candidate := range []struct {
		machineID string
		pk        string
		sks       []string
	}{
		{"", globalRulesPartition, global},
		{l.machineID, machineRulesPartPrefix + l.machineID, machine},
	} {
		for _, sk := range candidate.sks {
			ok, err := l.rules.confirm(ctx, candidate.machineID, ruleRowKey{pk: candidate.pk, sk: sk})
			if err != nil {
				log.Printf("eventupload: could not read a rule that collects allowed executions; not storing its executions from machine %s in this upload: %v", l.machineID, err)
				continue
			}
			collects = collects || ok
		}
	}
	l.decisions[matchKey] = collects
	return collects
}
