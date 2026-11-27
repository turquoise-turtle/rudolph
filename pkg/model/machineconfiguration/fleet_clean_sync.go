package machineconfiguration

import (
	"context"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"

	"github.com/airbnb/rudolph/pkg/types"
)

const (
	autoCleanSyncForAttribute     = "AutoCleanSyncFor"
	autoCleanSyncAtAttribute      = "AutoCleanSyncAt"
	autoCleanSyncRequestAttribute = "AutoCleanSyncRequest"
)

// FleetCleanSyncWriter is the UpdateItem of the AWS SDK's DynamoDB client (*dynamodb.Client), which is what the fleet
// job holds.
type FleetCleanSyncWriter interface {
	UpdateItem(ctx context.Context, in *awsdynamodb.UpdateItemInput, optFns ...func(*awsdynamodb.Options)) (*awsdynamodb.UpdateItemOutput, error)
}

// RequestFleetCleanSync requests one clean sync of a Mac for the fleet job, which makes one for a Mac whose rules hash
// differs from that of the Macs sent the same rules (internal/fleet). changeKey names the rule change the request is
// for, and now is when it is made. It returns the CleanSyncRequest written.
//
// It is a single UpdateItem of the Mac's config item, created if absent, and it names exactly the attributes the fleet
// role's policy allows (deployments/terraform_modules/santa_api/fleet.tf: PK, SK, DataType, CleanSync,
// CleanSyncRequest and the three AutoCleanSync attributes). Anything added here, a condition or a returned value
// included, has to be allowed there first, or every request is denied. An item it creates holds nothing else, so the
// Mac keeps inheriting every setting, as after `rudolph config update --machine <id> --clean-sync`.
//
// Like that command it replaces a request still pending: the new identifier keeps a clean sync that started before it
// from clearing the flag.
func RequestFleetCleanSync(ctx context.Context, client FleetCleanSyncWriter, table, machineID, changeKey string, now time.Time) (request string, err error) {
	request = newCleanSyncRequest()
	_, err = client.UpdateItem(ctx, &awsdynamodb.UpdateItemInput{
		TableName: aws.String(table),
		Key: map[string]awstypes.AttributeValue{
			"PK": &awstypes.AttributeValueMemberS{Value: machineConfigurationPK(machineID)},
			"SK": &awstypes.AttributeValueMemberS{Value: machineConfigurationSK()},
		},
		UpdateExpression: aws.String("SET CleanSync = :yes, CleanSyncRequest = :request, AutoCleanSyncFor = :for, AutoCleanSyncAt = :at, AutoCleanSyncRequest = :request, DataType = :type"),
		ExpressionAttributeValues: map[string]awstypes.AttributeValue{
			":yes":     &awstypes.AttributeValueMemberBOOL{Value: true},
			":request": &awstypes.AttributeValueMemberS{Value: request},
			":for":     &awstypes.AttributeValueMemberS{Value: changeKey},
			":at":      &awstypes.AttributeValueMemberS{Value: now.UTC().Format(time.RFC3339)},
			":type":    &awstypes.AttributeValueMemberS{Value: string(types.DataTypeMachineConfig)},
		},
	})
	if err != nil {
		return "", fmt.Errorf("requesting a clean sync of machine %s failed: %w", machineID, err)
	}
	return request, nil
}

// CleanSyncRequestedByFleet reports whether the clean sync requested now is the fleet job's. A manual request made
// after it has its own CleanSyncRequest, so it is then false; so is a flag stored without an identifier.
func (c MachineConfiguration) CleanSyncRequestedByFleet() bool {
	return c.CleanSync && c.CleanSyncRequest != "" && c.CleanSyncRequest == c.AutoCleanSyncRequest
}

// withoutFleetCleanSync returns a config without the record of a fleet job request, which belongs to one Mac: on the
// global config it would otherwise be inherited by every Mac without a record of its own.
func withoutFleetCleanSync(config MachineConfiguration) MachineConfiguration {
	config.AutoCleanSyncFor, config.AutoCleanSyncAt, config.AutoCleanSyncRequest = "", "", ""
	return config
}
