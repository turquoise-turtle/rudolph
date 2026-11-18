//go:build integration

package machine

// From the stored-XSS review (2026-11-19): when forget cannot remove an assigned group, its error names the stored
// group with Go's %q, which leaves variation selectors raw (strconv.IsPrint accepts them). Groups registered before
// 2026-11-19 may have such names, so the error must show them as escapes, as the plan printed before it does.

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/airbnb/rudolph/internal/testutil/clitest"
	"github.com/airbnb/rudolph/internal/testutil/ddblocal"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsdynamodb "github.com/aws/aws-sdk-go-v2/service/dynamodb"
	awstypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func TestMachineForget_ErrorEscapesAHiddenAssignedGroupName(t *testing.T) {
	ddblocal.Reset(t)
	seedMac(t, macA, "lab-mac-01", "C02ABC123", time.Now())
	payload, escapes := selectorBytes("rm -rf ~")
	// The removal fails because the name makes the member row's partition key longer than DynamoDB's 2048 bytes;
	// that is the simplest way to reach the error path against DynamoDB Local.
	name := "e" + payload + "ng-" + strings.Repeat("x", 2100)
	if _, err := ddblocal.Raw(t).PutItem(context.Background(), &awsdynamodb.PutItemInput{
		TableName: aws.String(ddblocal.TableName),
		Item: map[string]awstypes.AttributeValue{
			"PK":       &awstypes.AttributeValueMemberS{Value: "Machine#" + macA},
			"SK":       &awstypes.AttributeValueMemberS{Value: "Groups"},
			"Assigned": &awstypes.AttributeValueMemberSS{Value: []string{name}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	out, err := clitest.Run(t, env(), MachineCmd, "", "forget", "-m", macA, "--yes")
	if err == nil {
		t.Fatalf("forget removed an assigned group whose member key is too long; the test no longer reaches the error path:\n%+q", out)
	}
	msg := err.Error()
	// The name pads the message with 2,100 bytes of x; the start is enough to read a failure.
	shown := msg
	if len(shown) > 300 {
		shown = shown[:300] + "..."
	}
	for _, r := range msg {
		if unicode.Is(unicode.Variation_Selector, r) {
			t.Errorf("forget's error has raw selector U+%04X:\n%+q", r, shown)
			break
		}
	}
	if !strings.Contains(msg, "e"+escapes+"ng-") {
		t.Errorf("forget's error does not show the group name's selectors as %+q:\n%+q", escapes, shown)
	}
}
