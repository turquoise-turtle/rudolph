package feedrules

import (
	"fmt"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/rules"
)

// ConstructFeedRuleFromBaseRule builds the feed row for a rule change. It returns an error when the rule is not valid,
// so that callers never write (or dereference) an invalid feed row.
func ConstructFeedRuleFromBaseRule(
	timeProvider clock.TimeProvider,
	rule rules.SantaRule,
) (*FeedRuleRow, error) {
	var identifier string
	// Support backwards compatibility with legacy SHA256 identifier
	if rule.SHA256 != "" && rule.Identifier == "" {
		identifier = rule.SHA256
	} else {
		identifier = rule.Identifier
	}

	// Morph the identifier back into the rule to start a slow migration
	rule.Identifier = identifier

	feedRuleRow := &FeedRuleRow{
		PrimaryKey: dynamodb.PrimaryKey{
			PartitionKey: feedRulesPK,
			// With this sort key, all rules will be ordered by the date they are created,
			// but also uniqified by the specific binary. This means that you can seek all rules
			// over time, kind of like picking up diffs through VCS changes.
			SortKey: feedRulesSK(timeProvider, identifier, rule.RuleType),
		},
		SantaRule:    rule,
		ExpiresAfter: GetSyncStateExpiresAfter(timeProvider),
		DataType:     GetDataType(),
	}

	isValid, err := feedRuleRow.feedRuleRowValidation()
	if err != nil {
		return nil, fmt.Errorf("invalid feed rule for %q: %w", identifier, err)
	}
	if !isValid {
		return nil, fmt.Errorf("invalid feed rule: identifier %q is not valid for its rule type", identifier)
	}

	return feedRuleRow, nil
}

func ReconstructFeedSyncLastEvaluatedKeyFromDate(feedSyncCursor string) dynamodb.PrimaryKey {
	return dynamodb.PrimaryKey{
		PartitionKey: feedRulesPK,
		SortKey:      feedSyncCursor,
	}
}
