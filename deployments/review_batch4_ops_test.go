package deployments

// Regression tests for the independent review of batch 4's OPS changes.

import (
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"
)

// Review finding 3: the default stage throttle must not reject a large fleet's normal load after an upgrade. Santa
// syncs about every 10 minutes with about 5 requests per sync, so 100,000 Macs average about 830 requests a second,
// with peaks well above. The defaults must leave room for that.
func TestReview_DefaultThrottleFitsALargeFleet(t *testing.T) {
	files := loadTerraform(t, "terraform_modules/santa_api")
	defaults := map[string]float64{}
	for _, v := range blocks(files, "variable") {
		def, ok := v.Body.Attributes["default"]
		if !ok {
			continue
		}
		value, diags := def.Expr.Value(nil)
		if diags.HasErrors() || value.IsNull() || !value.Type().Equals(cty.Number) {
			continue
		}
		f, _ := value.AsBigFloat().Float64()
		defaults[v.Labels[0]] = f
	}
	for _, ms := range blocks(files, "resource", "aws_api_gateway_method_settings") {
		for _, settings := range nested(ms, "settings") {
			limit := func(name string) float64 {
				attr, ok := settings.Body.Attributes[name]
				if !ok {
					return 0
				}
				return defaults[strings.TrimPrefix(strings.TrimSpace(sourceOf(t, files, attr.Expr)), "var.")]
			}
			rate, burst := limit("throttling_rate_limit"), limit("throttling_burst_limit")
			if rate < 2000 {
				t.Errorf("default throttling rate limit %v requests/s would reject a 100,000-Mac fleet's normal load", rate)
			}
			if burst < rate {
				t.Errorf("default burst limit %v is below the rate limit %v", burst, rate)
			}
		}
	}
}

// Review finding 3: throttled requests (429, counted in API Gateway's 4XXError) must raise an alarm.
func TestReview_ThrottledRequestsRaiseAnAlarm(t *testing.T) {
	all, _ := alarms(t)
	for _, a := range all {
		if (strings.Contains(a.signature, "aws/apigateway") && strings.Contains(a.signature, "4xxerror")) || strings.Contains(a.signature, "status=429") {
			if !strings.Contains(a.actions, "var.") && !strings.Contains(a.actions, "local.") {
				t.Errorf("alarm %s must notify the SNS topic given by a variable", a.name)
			}
			return
		}
	}
	t.Error("no alarm for throttled (429) or other 4xx responses")
}
