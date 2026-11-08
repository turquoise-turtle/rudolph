//go:build integration

package machine

// From the Fable review: forgetting a Mac whose groups are only reported (none assigned) deletes its Groups row; the
// registry's MembershipVersion must change too, so readers that cache membership see the Mac leave.

import (
	"context"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/cli/group"
	"github.com/airbnb/rudolph/internal/testutil/clitest"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/airbnb/rudolph/pkg/model/groups"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestMachineForget_BumpsMembershipVersionForReportedGroups(t *testing.T) {
	ddblocal.Reset(t)
	if out, err := clitest.Run(t, env(), group.GroupCmd, "", "create", "eng"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	seedMac(t, macA, "lab-mac-01", "C02ABC123", time.Now())
	if _, err := ddblocal.Raw(t).PutItem(context.Background(), &awsdynamodb.PutItemInput{
		TableName: aws.String(ddblocal.TableName),
		Item: map[string]awstypes.AttributeValue{
			"PK":       &awstypes.AttributeValueMemberS{Value: "Machine#" + macA},
			"SK":       &awstypes.AttributeValueMemberS{Value: "Groups"},
			"Reported": &awstypes.AttributeValueMemberSS{Value: []string{"eng"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	before, err := groups.GetRegistry(ddblocal.Client(t), true)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := clitest.Run(t, env(), MachineCmd, "", "forget", "-m", macA, "--yes"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	after, err := groups.GetRegistry(ddblocal.Client(t), true)
	if err != nil {
		t.Fatal(err)
	}
	if after.MembershipVersion <= before.MembershipVersion {
		t.Errorf("MembershipVersion %d after forgetting a Mac with reported groups, was %d; want it bumped", after.MembershipVersion, before.MembershipVersion)
	}
}
