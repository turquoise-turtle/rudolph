//go:build integration

package machine

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/airbnb/rudolph/internal/testutil/clitest"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// A stored temporary mode Rudolph ignores because its end is more than 30 days away (written by other tooling, or in
// milliseconds by mistake) is not "ended": machine show says why it is ignored (review of fix/cli-machines).
func TestMachineShow_TemporaryModeTooFarAheadIsNotEnded(t *testing.T) {
	ddblocal.Reset(t)
	seedMac(t, macA, "lab-mac-01", "C02ABC123", time.Now())
	far := time.Now().Add(60 * 24 * time.Hour).Unix()
	if _, err := ddblocal.Raw(t).PutItem(context.Background(), &awsdynamodb.PutItemInput{
		TableName: aws.String(ddblocal.TableName),
		Item: map[string]awstypes.AttributeValue{
			"PK":                           &awstypes.AttributeValueMemberS{Value: "Machine#" + macA},
			"SK":                           &awstypes.AttributeValueMemberS{Value: "Config"},
			"ClientMode":                   &awstypes.AttributeValueMemberN{Value: "1"},
			"TemporaryClientMode":          &awstypes.AttributeValueMemberN{Value: "2"},
			"TemporaryClientModeExpiresAt": &awstypes.AttributeValueMemberN{Value: strconv.FormatInt(far, 10)},
		},
	}); err != nil {
		t.Fatal(err)
	}
	out, err := clitest.Run(t, env(), MachineCmd, "", "show", "-m", macA)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	line := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "Temporary mode:") {
			line = l
		}
	}
	if strings.Contains(line, "ended") || !strings.Contains(line, "ignored") || !strings.Contains(line, "30 days") {
		t.Errorf("temporary mode line %q; want it ignored because its end is more than 30 days away, not ended", line)
	}
}
