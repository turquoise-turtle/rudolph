package feedrules

import (
	"fmt"
	"strings"
	"time"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/rules"
	"github.com/airbnb/rudolph/pkg/types"
)

const (
	feedRulesPK                     = "RulesFeed"
	feedRulesExpiresAfterInDays int = 90
)

type FeedRuleRow struct {
	dynamodb.PrimaryKey
	rules.SantaRule
	ExpiresAfter int64          `dynamodbav:"ExpiresAfter,omitempty"`
	DataType     types.DataType `dynamodbav:"DataType"`
}

func GetSyncStateExpiresAfter(timeProvider clock.TimeProvider) int64 {
	return clock.Unixtimestamp(timeProvider.Now().UTC().AddDate(0, 0, feedRulesExpiresAfterInDays))
}

func GetDataType() types.DataType {
	return types.DataTypeRulesFeed
}

// TimeOfSortKey returns the time a feed sort key is ordered by, the time its writer stamped the row with. It also
// accepts a bare time, as in a feed cursor (see ReconstructFeedSyncLastEvaluatedKeyFromDate).
func TimeOfSortKey(sortKey string) (time.Time, error) {
	stamp, _, _ := strings.Cut(sortKey, "#")
	return clock.ParseRFC3339(stamp)
}

func feedRulesSK(
	timeProvider clock.TimeProvider,
	identifier string,
	ruleType types.RuleType,
) string {
	return fmt.Sprintf(
		"%s#%s",
		clock.RFC3339(timeProvider.Now()),
		rules.RuleSortKeyFromTypeIdentifier(identifier, ruleType),
	)
}
