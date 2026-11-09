//go:build integration

package machine

// machine show prints a temporary mode's end in UTC and local time on every line, including when the stored mode has
// ended or is ignored (owner feedback from the first production deployment).

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

func TestMachineShow_TemporaryModeEndInLocalTime(t *testing.T) {
	old := time.Local
	time.Local = time.FixedZone("TST", 11*3600)
	t.Cleanup(func() { time.Local = old })

	for name, end := range map[string]time.Time{
		"active":  time.Now().Add(2 * time.Hour),
		"ended":   time.Now().Add(-3 * time.Hour),
		"ignored": time.Now().Add(60 * 24 * time.Hour),
	} {
		t.Run(name, func(t *testing.T) {
			ddblocal.Reset(t)
			seedMac(t, macA, "lab-mac-01", "C02ABC123", time.Now())
			if _, err := ddblocal.Raw(t).PutItem(context.Background(), &awsdynamodb.PutItemInput{
				TableName: aws.String(ddblocal.TableName),
				Item: map[string]awstypes.AttributeValue{
					"PK":                           &awstypes.AttributeValueMemberS{Value: "Machine#" + macA},
					"SK":                           &awstypes.AttributeValueMemberS{Value: "Config"},
					"ClientMode":                   &awstypes.AttributeValueMemberN{Value: "1"},
					"TemporaryClientMode":          &awstypes.AttributeValueMemberN{Value: "2"},
					"TemporaryClientModeExpiresAt": &awstypes.AttributeValueMemberN{Value: strconv.FormatInt(end.Unix(), 10)},
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
			if !strings.Contains(line, end.UTC().Format(time.RFC3339)) || !strings.Contains(line, end.In(time.Local).Format("2006-01-02 15:04")+" TST local") {
				t.Errorf("temporary mode line %q; want the end in UTC and as \"%s TST local\"", line, end.In(time.Local).Format("2006-01-02 15:04"))
			}
		})
	}
}
