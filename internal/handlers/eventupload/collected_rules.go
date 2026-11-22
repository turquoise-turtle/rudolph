package eventupload

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/airbnb/rudolph/pkg/model/eventcollection"
	"github.com/airbnb/rudolph/pkg/model/globalrules"
	"github.com/airbnb/rudolph/pkg/model/groups"
	"github.com/airbnb/rudolph/pkg/model/machinerules"
	"github.com/airbnb/rudolph/pkg/model/rules"
)

// collectedRulesCacheFor is how long a warm Lambda keeps what it read about flagged rules (the index, the group
// registry and what each rule row says): a change made with the CLI reaches the events table within this time.
const collectedRulesCacheFor = 60 * time.Second

// maxCachedRuleRows bounds the rule rows a warm Lambda remembers. Group rules add a row per Mac and flagged key (the
// Mac's machine rule for it), so the cache is cleared of expired entries, or entirely, when it gets this large.
const maxCachedRuleRows = 20000

const (
	globalRulesPartition   = "GlobalRules"
	machineRulesPartPrefix = "MachineRules#"
	groupRulesPartPrefix   = "GroupRules#"
)

type collectionAPI interface {
	GetItem(ctx context.Context, in *dynamodb.GetItemInput, optFns ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	TransactWriteItems(ctx context.Context, in *dynamodb.TransactWriteItemsInput, optFns ...func(*dynamodb.Options)) (*dynamodb.TransactWriteItemsOutput, error)
}

// collectedRules decides which allowed-by-rule executions are stored. The index of flagged rules
// (pkg/model/eventcollection) only names candidates: a rule listed there is collected only once its row confirms it
// (it exists, is flagged and, for a machine rule, still applies), as the index can outlive a row deleted by the TTL or
// rewritten without the flag. A flagged group rule collects only on the Macs it decides for, which takes the Mac's
// groups and the rows of the rules that would beat it (see collectionLookup.groupRuleCollects). The index, the
// registry and the rule rows are cached for collectedRulesCacheFor. A failed read is logged and the executions it
// concerns are not stored; it never fails the upload.
type collectedRules struct {
	api   collectionAPI
	table string
	now   func() time.Time

	mu         sync.Mutex
	index      eventcollection.Index
	indexAt    time.Time
	hasIndex   bool
	registry   *groups.Registry
	registryAt time.Time
	rows       map[ruleRowKey]ruleRowFacts
}

type ruleRowKey struct{ pk, sk string }

// ruleRowFacts is what collection needs of a rule row, as read at a given time.
type ruleRowFacts struct {
	exists  bool
	flagged bool
	// applies is whether the rule is in force: a machine rule that has not expired and is not being removed; any
	// existing global or group rule.
	applies bool
	rule    rules.SantaRule
	at      time.Time
}

func newCollectedRules(api collectionAPI, storeTable string) *collectedRules {
	return &collectedRules{api: api, table: storeTable, now: time.Now, rows: map[ruleRowKey]ruleRowFacts{}}
}

func fresh(at, now time.Time) bool {
	return !now.Before(at) && now.Sub(at) < collectedRulesCacheFor
}

func (c *collectedRules) getItem(ctx context.Context, pk, sk string) (map[string]types.AttributeValue, error) {
	out, err := c.api.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      aws.String(c.table),
		Key:            map[string]types.AttributeValue{"PK": str(pk), "SK": str(sk)},
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return nil, err
	}
	return out.Item, nil
}

func (c *collectedRules) readIndex(ctx context.Context) (eventcollection.Index, error) {
	c.mu.Lock()
	if c.hasIndex && fresh(c.indexAt, c.now()) {
		defer c.mu.Unlock()
		return c.index, nil
	}
	c.mu.Unlock()

	key := eventcollection.IndexKey()
	item, err := c.getItem(ctx, key.PartitionKey, key.SortKey)
	if err != nil {
		return eventcollection.Index{}, err
	}
	index := eventcollection.ParseIndex(item)
	c.mu.Lock()
	c.index, c.indexAt, c.hasIndex = index, c.now(), true
	c.mu.Unlock()
	return index, nil
}

func (c *collectedRules) readRegistry(ctx context.Context) (*groups.Registry, error) {
	c.mu.Lock()
	if c.registry != nil && fresh(c.registryAt, c.now()) {
		defer c.mu.Unlock()
		return c.registry, nil
	}
	c.mu.Unlock()

	key := groups.RegistryKey()
	item, err := c.getItem(ctx, key.PartitionKey, key.SortKey)
	if err != nil {
		return nil, err
	}
	registry, err := groups.RegistryFromItem(item)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.registry, c.registryAt = registry, c.now()
	c.mu.Unlock()
	return registry, nil
}

// row returns what the rule row at key says, reading it unless a read of the last collectedRulesCacheFor is cached.
// read reports whether it was read now.
func (c *collectedRules) row(ctx context.Context, key ruleRowKey) (facts ruleRowFacts, read bool, err error) {
	c.mu.Lock()
	if cached, ok := c.rows[key]; ok && fresh(cached.at, c.now()) {
		c.mu.Unlock()
		return cached, false, nil
	}
	c.mu.Unlock()

	item, err := c.getItem(ctx, key.pk, key.sk)
	if err != nil {
		return ruleRowFacts{}, false, err
	}
	now := c.now()
	facts = ruleRowFacts{at: now}
	if len(item) > 0 {
		facts.exists = true
		switch {
		case strings.HasPrefix(key.pk, machineRulesPartPrefix):
			var row machinerules.MachineRuleRow
			if err := attributevalue.UnmarshalMap(item, &row); err != nil {
				return ruleRowFacts{}, false, err
			}
			facts.flagged, facts.applies, facts.rule = row.CollectAllowedEvents, row.AppliesAt(now), row.SantaRule
		case strings.HasPrefix(key.pk, groupRulesPartPrefix):
			var row groups.RuleRow
			if err := attributevalue.UnmarshalMap(item, &row); err != nil {
				return ruleRowFacts{}, false, err
			}
			facts.flagged, facts.applies, facts.rule = row.CollectAllowedEvents, true, row.SantaRule
		default:
			var row globalrules.GlobalRuleRow
			if err := attributevalue.UnmarshalMap(item, &row); err != nil {
				return ruleRowFacts{}, false, err
			}
			facts.flagged, facts.applies, facts.rule = row.CollectAllowedEvents, true, row.SantaRule
		}
	}
	c.mu.Lock()
	if len(c.rows) >= maxCachedRuleRows {
		for k, cached := range c.rows {
			if !fresh(cached.at, now) {
				delete(c.rows, k)
			}
		}
		if len(c.rows) >= maxCachedRuleRows {
			c.rows = map[ruleRowKey]ruleRowFacts{}
		}
	}
	c.rows[key] = facts
	c.mu.Unlock()
	return facts, true, nil
}

// confirm returns what the row of a rule listed in the index attribute says. A row that is gone or not flagged is
// taken out of the index.
func (c *collectedRules) confirm(ctx context.Context, attribute string, key ruleRowKey) (ruleRowFacts, error) {
	facts, read, err := c.row(ctx, key)
	if err != nil {
		return ruleRowFacts{}, err
	}
	if read && !facts.flagged {
		c.forget(ctx, attribute, key)
	}
	return facts, nil
}

// forget takes a rule whose row is gone or not flagged out of the index, unless its row has been flagged in the
// meantime (the condition check), so a concurrent CLI run flagging it again is never undone. Best effort: a failure
// only leaves the stale entry, which the next confirmation reads again.
func (c *collectedRules) forget(ctx context.Context, attribute string, key ruleRowKey) {
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
			ExpressionAttributeNames: map[string]string{"#rules": attribute},
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

	// The Mac's groups, read with the first execution whose rule some group has flagged.
	groupsRead   bool
	groupsFailed bool
	registry     *groups.Registry
	effective    []string
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
		attribute string
		pk        string
		sks       []string
	}{
		{eventcollection.IndexAttribute(""), globalRulesPartition, global},
		{eventcollection.IndexAttribute(l.machineID), machineRulesPartPrefix + l.machineID, machine},
	} {
		for _, sk := range candidate.sks {
			facts, err := l.rules.confirm(ctx, candidate.attribute, ruleRowKey{pk: candidate.pk, sk: sk})
			if err != nil {
				log.Printf("eventupload: could not read a rule that collects allowed executions; not storing its executions from machine %s in this upload: %v", l.machineID, err)
				continue
			}
			collects = collects || (facts.flagged && facts.applies)
		}
	}
	if !collects {
		collects = l.groupRuleCollects(ctx, l.index.GroupCandidates(ruleSortKey))
	}
	l.decisions[matchKey] = collects
	return collects
}

// machineGroups reads, once per upload, the groups whose rules apply to the Mac (groups.Effective, as ruledownload
// counts them). After a failed read no group rule collects in this upload.
func (l *collectionLookup) machineGroups(ctx context.Context) bool {
	if l.groupsRead {
		return !l.groupsFailed
	}
	l.groupsRead = true
	fail := func(err error) bool {
		log.Printf("eventupload: could not read the groups of machine %s; not storing the executions its groups' rules collect in this upload: %v", l.machineID, err)
		l.groupsFailed = true
		return false
	}
	registry, err := l.rules.readRegistry(ctx)
	if err != nil {
		return fail(err)
	}
	key := groups.MachineGroupsKey(l.machineID)
	item, err := l.rules.getItem(ctx, key.PartitionKey, key.SortKey)
	if err != nil {
		return fail(err)
	}
	membership, err := groups.MachineGroupsFromItem(item)
	if err != nil {
		return fail(err)
	}
	l.registry, l.effective = registry, groups.Effective(registry, membership)
	return true
}

// groupRuleCollects reports whether a flagged group rule among candidates (by group, the stored sort keys the index
// lists for the execution's rule) decided the execution on this Mac and so collects it:
//
//   - the group is registered and one of the Mac's effective groups;
//   - the rule row confirms the flag and is an allow;
//   - the Mac has no machine rule in force for the key, which would beat any group rule (its own flag was already
//     looked at);
//   - no other group of the Mac has a rule for the key that beats an allow (a block, or a CEL rule).
//
// It follows what ruledownload sends the Mac (groups.Resolve), comparing rules by their stored sort key. Nothing is
// read when no group has flagged the rule.
func (l *collectionLookup) groupRuleCollects(ctx context.Context, candidates map[string][]string) bool {
	if len(candidates) == 0 || !l.machineGroups(ctx) {
		return false
	}
	for _, group := range l.effective {
		// The reserved group of Macs in no group is effective even when nobody registered it
		if _, registered := l.registry.Groups[group]; !registered {
			continue
		}
		for _, sk := range candidates[group] {
			decides, err := l.groupRuleDecides(ctx, group, sk)
			if err != nil {
				log.Printf("eventupload: could not read a rule that decides whether a group rule collects allowed executions; not storing its executions from machine %s in this upload: %v", l.machineID, err)
				continue
			}
			if decides {
				return true
			}
		}
	}
	return false
}

func (l *collectionLookup) groupRuleDecides(ctx context.Context, group, sk string) (bool, error) {
	facts, err := l.rules.confirm(ctx, eventcollection.GroupIndexAttribute(group), ruleRowKey{pk: groupRulesPartPrefix + group, sk: sk})
	if err != nil {
		return false, err
	}
	if !facts.flagged || !groups.IsAllow(facts.rule.Policy) {
		return false, nil
	}
	machineRule, _, err := l.rules.row(ctx, ruleRowKey{pk: machineRulesPartPrefix + l.machineID, sk: sk})
	if err != nil {
		return false, err
	}
	if machineRule.exists && machineRule.applies {
		return false, nil
	}
	for _, other := range l.effective {
		if other == group {
			continue
		}
		rule, _, err := l.rules.row(ctx, ruleRowKey{pk: groupRulesPartPrefix + other, sk: sk})
		if err != nil {
			return false, err
		}
		if rule.exists && groups.BeatsAllow(rule.rule) {
			return false, nil
		}
	}
	return true, nil
}
