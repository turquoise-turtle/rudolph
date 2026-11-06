package globalrules

import (
	"fmt"

	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/types"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/expression"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// PolicyIndexBackfillAPI is what BackfillPolicyIndex needs
type PolicyIndexBackfillAPI interface {
	dynamodb.QueryAPI
	dynamodb.UpdateItemIfAPI
}

type policyIndexBackfillRow struct {
	dynamodb.PrimaryKey
	Policy types.Policy `dynamodbav:"Policy"`
}

type policyIndexUpdate struct {
	GlobalRulePolicy types.Policy `dynamodbav:"GlobalRulePolicy"`
}

// BackfillPolicyIndex sets GlobalRulePolicy to Policy on every global rule row that does not have it yet, i.e. rows
// written before the GlobalRulePolicy_SK index existed, so that the web view's policy filter finds them. Nothing else is
// changed, and the rules feed is not touched: Santa sees no difference. It returns how many rows it updated; running it
// again updates none.
func BackfillPolicyIndex(client PolicyIndexBackfillAPI) (updated int, err error) {
	keyCond := expression.KeyEqual(expression.Key("PK"), expression.Value(globalRulesPK))
	filter := expression.AttributeNotExists(expression.Name("GlobalRulePolicy"))
	projection := expression.NamesList(expression.Name("PK"), expression.Name("SK"), expression.Name("Policy"))
	expr, err := expression.NewBuilder().
		WithKeyCondition(keyCond).
		WithFilter(filter).
		WithProjection(projection).
		Build()
	if err != nil {
		return 0, err
	}

	var startKey map[string]awstypes.AttributeValue
	for {
		result, err := client.Query(&awsdynamodb.QueryInput{
			ConsistentRead:            aws.Bool(true),
			ExpressionAttributeNames:  expr.Names(),
			ExpressionAttributeValues: expr.Values(),
			KeyConditionExpression:    expr.KeyCondition(),
			FilterExpression:          expr.Filter(),
			ProjectionExpression:      expr.Projection(),
			ExclusiveStartKey:         startKey,
		})
		if err != nil {
			return updated, fmt.Errorf("failed to read global rules: %w", err)
		}

		var rows []policyIndexBackfillRow
		if err := attributevalue.UnmarshalListOfMaps(result.Items, &rows); err != nil {
			return updated, fmt.Errorf("failed to unmarshal global rules: %w", err)
		}
		for _, row := range rows {
			// A row without a policy is not a valid rule; there is nothing to mirror
			if row.Policy == 0 {
				continue
			}
			// The condition keeps a policy set by a concurrent add or update; the update's own PK condition stops a
			// rule removed since the query from being recreated as a partial row.
			_, err := client.UpdateItemIf(row.PrimaryKey, policyIndexUpdate{GlobalRulePolicy: row.Policy}, filter)
			if dynamodb.IsConditionalCheckFailed(err) {
				continue
			}
			if err != nil {
				return updated, fmt.Errorf("failed to backfill GlobalRulePolicy on %s: %w", row.SortKey, err)
			}
			updated++
		}

		if len(result.LastEvaluatedKey) == 0 {
			return updated, nil
		}
		startKey = result.LastEvaluatedKey
	}
}
