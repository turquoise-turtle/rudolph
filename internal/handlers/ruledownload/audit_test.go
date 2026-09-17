package ruledownload

import (
	"fmt"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

// Shortcoming S3: ruledownload must validate the machine ID like the other sync stages, before touching storage.
func TestRuledownload_RejectsInvalidMachineID_S3(t *testing.T) {
	for _, id := range []string{"not-a-uuid", "../../GlobalRules", ""} {
		t.Run(fmt.Sprintf("%q", id), func(t *testing.T) {
			h := &PostRuledownloadHandler{booted: true} // no storage: any access would panic
			var resp *events.APIGatewayProxyResponse
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("handler reached storage for an invalid machine ID (panic: %v)", r)
					}
				}()
				resp, _ = h.Handle(events.APIGatewayProxyRequest{
					HTTPMethod: "POST", Resource: "/ruledownload/{machine_id}",
					PathParameters: map[string]string{"machine_id": id},
					Headers:        map[string]string{"Content-Type": "application/json"},
					Body:           `{}`,
				})
			}()
			if resp == nil || resp.StatusCode != 400 {
				t.Fatalf("expected HTTP 400, got %+v", resp)
			}
		})
	}
}
