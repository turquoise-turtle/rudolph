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

// Deleting a group leaves its assignments on the Macs. A Mac assigned a group whose name now has a refused format
// character (registered before 2026-11-18, then deleted as docs/deploy.md describes) must still be able to have that
// stale assignment removed with `group remove`.
func TestGroupCLI_StaleAssignmentWithAFormatCharacterCanBeRemoved(t *testing.T) {
	ddblocal.Reset(t)
	mustGroup(t, "create", "legacy")
	mustGroup(t, "add", "-m", machine, "legacy")
	old := "ops" + string(rune(0x202e))
	_, err := ddblocal.Raw(t).UpdateItem(context.Background(), &awsdynamodb.UpdateItemInput{
		TableName:                 aws.String(ddblocal.TableName),
		Key:                       map[string]awstypes.AttributeValue{"PK": &awstypes.AttributeValueMemberS{Value: "Machine#" + machine}, "SK": &awstypes.AttributeValueMemberS{Value: "Groups"}},
		UpdateExpression:          aws.String("SET Assigned = :s"),
		ExpressionAttributeValues: map[string]awstypes.AttributeValue{":s": &awstypes.AttributeValueMemberSS{Value: []string{old}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	mustGroup(t, "remove", "-m", machine, old)
	out := mustGroup(t, "show", "--machine", machine)
	if strings.Contains(out, `ops\u202e`) || strings.Contains(out, old) {
		t.Errorf("stale assignment still shown after remove:\n%s", out)
	}
	// A name that is neither registered nor assigned is still refused.
	if out, err := group(t, "remove", "-m", machine, "other"+string(rune(0x202e))); err == nil {
		t.Errorf("remove accepted an unknown name with a bidi override:\n%s", out)
	}
}
