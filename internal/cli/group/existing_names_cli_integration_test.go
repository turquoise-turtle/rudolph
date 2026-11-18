//go:build integration

package group

import (
	"context"
	"strings"
	"testing"

	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// A group registered before group names refused invisible format characters (2026-11-18) must stay manageable: an
// admin can still show it and delete it by the name `group list` shows, and is told about it rather than locked out.
func TestGroupCLI_ExistingGroupWithAFormatCharacterCanStillBeManaged(t *testing.T) {
	ddblocal.Reset(t)
	mustGroup(t, "create", "legacy", "--description", "made before the rule")
	old := "ops\u202e"
	_, err := ddblocal.Raw(t).UpdateItem(context.Background(), &awsdynamodb.UpdateItemInput{
		TableName:                aws.String(ddblocal.TableName),
		Key:                      map[string]awstypes.AttributeValue{"PK": &awstypes.AttributeValueMemberS{Value: "GroupRegistry"}, "SK": &awstypes.AttributeValueMemberS{Value: "Current"}},
		UpdateExpression:         aws.String("SET Groups.#new = Groups.#old REMOVE Groups.#old"),
		ExpressionAttributeNames: map[string]string{"#new": old, "#old": "legacy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := mustGroup(t, "list")
	if !strings.Contains(out, `ops\u202e`) {
		t.Errorf("group list does not show the name escaped:\n%s", out)
	}
	mustGroup(t, "get", old)
	mustGroup(t, "delete", old)
	if out := mustGroup(t, "list"); strings.Contains(out, "ops") {
		t.Errorf("group still listed after delete:\n%s", out)
	}
	// New groups with such a name are still refused.
	if out, err := group(t, "create", "new\u202e"); err == nil {
		t.Errorf("created a group with a bidi override:\n%s", out)
	}
}
