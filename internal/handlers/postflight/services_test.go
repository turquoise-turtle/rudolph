package postflight

import (
	"testing"

	"github.com/airbnb/rudolph/pkg/clock"
	"github.com/airbnb/rudolph/pkg/dynamodb"
	"github.com/airbnb/rudolph/pkg/model/syncstate"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/stretchr/testify/assert"
)

type mockUpdater func(key dynamodb.PrimaryKey, item interface{}) (*awsdynamodb.UpdateItemOutput, error)

func (m mockUpdater) UpdateItem(key dynamodb.PrimaryKey, item interface{}) (*awsdynamodb.UpdateItemOutput, error) {
	return m(key, item)
}

var _ dynamodb.UpdateItemAPI = mockUpdater(nil)

func Test_ConcreteSyncStateUpdater_OK(t *testing.T) {
	cur, _ := clock.ParseRFC3339("2000-01-01T00:00:00Z")
	updater := concreteSyncStateUpdater{
		updater: mockUpdater(
			func(key dynamodb.PrimaryKey, item interface{}) (*awsdynamodb.UpdateItemOutput, error) {
				assert.Equal(t, "Machine#AAAA-BBBB-CCCC", key.PartitionKey)
				assert.Equal(t, "SyncState", key.SortKey)

				thisItem := item.(syncstate.UpdatePostflightItem)
				assert.Equal(t, "2000-01-01T00:00:00Z", thisItem.PostflightAt)
				assert.Equal(t, cur.AddDate(0, 0, 90).Unix(), thisItem.ExpiresAfter)

				return &awsdynamodb.UpdateItemOutput{}, nil
			},
		),
		timeProvider: clock.FrozenTimeProvider{Current: cur},
	}

	err := updater.updatePostflightDate("AAAA-BBBB-CCCC")
	assert.Empty(t, err)
}
