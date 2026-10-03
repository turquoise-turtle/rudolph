package machinerules

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Delivery tracking records which machine-specific rules a Mac may hold: one small item per rule, in the
// MachineRuleDelivery#<machine id> partition, with the rule's sort key as sort key. Ruledownload writes an item when
// it first sends a rule, marks it when it first sends the rule's removal (or the global rule replacing it), and
// deletes it once that has been delivered. Items are only written when this changes, and only read when the digest
// on the sync state says something changed (see DeliveryDigest), so a steady-state sync reads or writes none.
//
// Items expire (ExpiresAfter, the table's TTL attribute) DeliveryExpiry after they were last written, and
// ruledownload rewrites all of a machine's items at least every DeliveryRefreshInterval, so they outlive the
// machine's sync state (which expires 90 days after its last sync) while the Mac keeps syncing, and disappear once it
// stops. A Mac whose sync state expired is told to clean sync, after which it holds no machine rule from Rudolph but
// those the clean sync sent; until then its items may stand for rules it still holds, so the recreated sync state
// makes the next ruledownload extend every item kept (see syncstate.SyncStateRow.ContinueFrom). An expired item thus
// never hides a rule the Mac still has.
const deliveryPKPrefix = "MachineRuleDelivery#"

const (
	// DeliveryRefreshInterval is how often ruledownload extends the expiry of a machine's tracking items
	DeliveryRefreshInterval = 30 * 24 * time.Hour
	// DeliveryExpiry is how long after it was last written a tracking item expires: longer than
	// DeliveryRefreshInterval plus the 90 days a sync state lives after the last sync
	DeliveryExpiry = 120 * 24 * time.Hour
)

// Delivery is what is tracked about a machine-specific rule the Mac may hold
type Delivery struct {
	// RemovalFirstSentInSync is the number (syncstate.SyncState.SyncSeq) of the first sync that sent the rule's
	// removal or replacement, or 0 while the rule applies.
	RemovalFirstSentInSync int
}

type deliveryRow struct {
	dynamodb.PrimaryKey
	RemovalFirstSentInSync int   `dynamodbav:"RemovalFirstSentInSync,omitempty"`
	ExpiresAfter           int64 `dynamodbav:"ExpiresAfter,omitempty"`
}

func deliveryPK(machineID string) string {
	return deliveryPKPrefix + machineID
}

// GetDeliveries returns the machine-specific rules tracked as possibly held by the Mac, by rule sort key. The reads
// are strongly consistent, as ruledownload may have updated them moments ago in the previous sync.
func GetDeliveries(client dynamodb.QueryAPI, machineID string) (map[string]Delivery, error) {
	deliveries := map[string]Delivery{}
	var exclusiveStartKey map[string]types.AttributeValue
	for {
		result, err := client.Query(&awsdynamodb.QueryInput{
			ConsistentRead:         aws.Bool(true),
			KeyConditionExpression: aws.String("PK = :pk"),
			ExpressionAttributeValues: map[string]types.AttributeValue{
				":pk": &types.AttributeValueMemberS{Value: deliveryPK(machineID)},
			},
			ExclusiveStartKey: exclusiveStartKey,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to read the machine rules delivered: %w", err)
		}
		var rows []deliveryRow
		if err := attributevalue.UnmarshalListOfMaps(result.Items, &rows); err != nil {
			return nil, fmt.Errorf("failed to unmarshal the machine rules delivered: %w", err)
		}
		for _, row := range rows {
			deliveries[row.SortKey] = Delivery{RemovalFirstSentInSync: row.RemovalFirstSentInSync}
		}
		if len(result.LastEvaluatedKey) == 0 {
			return deliveries, nil
		}
		exclusiveStartKey = result.LastEvaluatedKey
	}
}

// WriteDeliveries records the given deliveries and forgets the given rules, by rule sort key. The items written expire
// DeliveryExpiry after now.
func WriteDeliveries(client dynamodb.BatchWriteItemAPI, machineID string, set map[string]Delivery, forget []string, now time.Time) error {
	expiresAfter := now.Add(DeliveryExpiry).Unix()
	puts := make([]interface{}, 0, len(set))
	for sortKey, delivery := range set {
		puts = append(puts, deliveryRow{
			PrimaryKey:             dynamodb.PrimaryKey{PartitionKey: deliveryPK(machineID), SortKey: sortKey},
			RemovalFirstSentInSync: delivery.RemovalFirstSentInSync,
			ExpiresAfter:           expiresAfter,
		})
	}
	deletes := make([]dynamodb.PrimaryKey, len(forget))
	for i, sortKey := range forget {
		deletes[i] = dynamodb.PrimaryKey{PartitionKey: deliveryPK(machineID), SortKey: sortKey}
	}
	if err := client.BatchWriteItems(puts, deletes); err != nil {
		return fmt.Errorf("failed to record the machine rules delivered: %w", err)
	}
	return nil
}

// DeliveryDigest returns a fixed-size digest of tracked deliveries. The digest of the rules that apply now (all with
// a zero Delivery) equals the digest of the tracked deliveries exactly when every applying rule is tracked, nothing
// else is, and no removal is pending.
func DeliveryDigest(deliveries map[string]Delivery) string {
	entries := make([]string, 0, len(deliveries))
	for sortKey, delivery := range deliveries {
		entry := sortKey
		if delivery.RemovalFirstSentInSync != 0 {
			entry = fmt.Sprintf("%s\x00removal:%d", sortKey, delivery.RemovalFirstSentInSync)
		}
		entries = append(entries, entry)
	}
	sort.Strings(entries)
	sum := sha256.Sum256([]byte(strings.Join(entries, "\n")))
	return fmt.Sprintf("%d:%s", len(entries), hex.EncodeToString(sum[:16]))
}
