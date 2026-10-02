package dynamodb

import (
	"context"
	"errors"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// ClockSkewAPI measures how far the local clock is from DynamoDB's
type ClockSkewAPI interface {
	ClockSkew() (time.Duration, error)
}

// ClockSkew returns how far the local clock is ahead of the clock of the DynamoDB endpoint (negative when it is
// behind), from the Date header of a response. The precision is about a second.
//
// It reads a single item that does not exist, which only needs the dynamodb:GetItem permission.
func (dbc concreteDynamoDBClient) ClockSkew() (time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.TODO(), dbc.timeout)
	defer cancel()
	output, err := dbc.awsclient.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(dbc.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: "ClockCheck"},
			"SK": &types.AttributeValueMemberS{Value: "ClockCheck"},
		},
	})
	if err != nil {
		return 0, err
	}
	serverTime, ok := awsmiddleware.GetServerTime(output.ResultMetadata)
	if !ok {
		return 0, errors.New("the DynamoDB response has no Date header")
	}
	responseAt, ok := awsmiddleware.GetResponseAt(output.ResultMetadata)
	if !ok {
		responseAt = time.Now()
	}
	return responseAt.Sub(serverTime), nil
}
