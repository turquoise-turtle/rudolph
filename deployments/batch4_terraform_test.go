package deployments

// Batch 4 static checks on the Terraform configuration: the store policy is scoped to this deployment's table (B8),
// and the API is throttled and monitored (OPS).

import (
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// B8: the Lambdas' store policy must only grant access to this deployment's table and its indexes. A pattern such as
// table/*_rudolph_store lets every Rudolph deployment in the account write to every other one's table.
func TestB8_StorePolicyOnlyCoversThisDeploymentsTable(t *testing.T) {
	files := loadTerraform(t, "terraform_modules/santa_api/modules/store")
	docs := blocks(files, "data", "aws_iam_policy_document", "lambda_permissions")
	if len(docs) != 1 {
		t.Fatalf("expected one lambda_permissions policy document, found %d", len(docs))
	}
	for i, st := range nested(docs[0], "statement") {
		attr, ok := st.Body.Attributes["resources"]
		if !ok {
			t.Errorf("statement %d has no resources", i)
			continue
		}
		tuple, ok := attr.Expr.(*hclsyntax.TupleConsExpr)
		if !ok {
			t.Errorf("statement %d: resources must be a list of this table's ARNs, got %s", i, sourceOf(t, files, attr.Expr))
			continue
		}
		for _, element := range tuple.Exprs {
			src := sourceOf(t, files, element)
			if !strings.Contains(src, "aws_dynamodb_table.store.arn") {
				t.Errorf("statement %d grants access to %s, which is not this deployment's table (aws_dynamodb_table.store) or one of its indexes", i, src)
			}
			rest := strings.ReplaceAll(src, "aws_dynamodb_table.store.arn", "")
			if strings.Contains(rest, "*") && !strings.Contains(rest, "/index/") {
				t.Errorf("statement %d: wildcard resource %s", i, src)
			}
		}
	}
}

// OPS: API Gateway stage throttling, with rate and burst limits set by variables that have defaults.
func TestOPS_APIStageIsThrottledByVariables(t *testing.T) {
	files := loadTerraform(t, "terraform_modules/santa_api")
	variables := map[string]*hclsyntax.Block{}
	for _, v := range blocks(files, "variable") {
		variables[v.Labels[0]] = v
	}
	found := false
	for _, ms := range blocks(files, "resource", "aws_api_gateway_method_settings") {
		path, _ := literal(ms.Body.Attributes["method_path"])
		if path != "*/*" {
			continue
		}
		for _, settings := range nested(ms, "settings") {
			for _, name := range []string{"throttling_burst_limit", "throttling_rate_limit"} {
				attr, ok := settings.Body.Attributes[name]
				if !ok {
					t.Errorf("method settings %q do not set %s", ms.Labels[1], name)
					continue
				}
				src := strings.TrimSpace(sourceOf(t, files, attr.Expr))
				if !strings.HasPrefix(src, "var.") {
					t.Errorf("%s = %s must be set by a variable", name, src)
					continue
				}
				v := variables[strings.TrimPrefix(src, "var.")]
				if v == nil {
					t.Errorf("%s uses undeclared %s", name, src)
					continue
				}
				def, ok := v.Body.Attributes["default"]
				if !ok {
					t.Errorf("variable %s has no default", src)
					continue
				}
				value, diags := def.Expr.Value(nil)
				if diags.HasErrors() || value.IsNull() || !value.Type().Equals(cty.Number) {
					t.Errorf("variable %s must have a literal default", src)
					continue
				}
				if f, _ := value.AsBigFloat().Float64(); f <= 0 {
					t.Errorf("variable %s must default to a positive limit, got %v", src, f)
				}
			}
			found = true
		}
	}
	if !found {
		t.Error(`no aws_api_gateway_method_settings with method_path "*/*" throttles the stage`)
	}
}

type alarm struct {
	name      string
	signature string // namespace, metric names, dimensions and the patterns of the metric filters it alarms on
	actions   string
}

func alarms(t *testing.T) ([]alarm, []tfFile) {
	t.Helper()
	files := loadTerraform(t, "terraform_modules")
	filters := map[string]string{} // metric name source -> pattern source
	for _, f := range blocks(files, "resource", "aws_cloudwatch_log_metric_filter") {
		pattern := ""
		if attr, ok := f.Body.Attributes["pattern"]; ok {
			pattern = sourceOf(t, files, attr.Expr)
		}
		for _, mt := range nested(f, "metric_transformation") {
			if attr, ok := mt.Body.Attributes["name"]; ok {
				filters[strings.TrimSpace(sourceOf(t, files, attr.Expr))] += " " + pattern + " " + f.Labels[1]
			}
		}
	}
	var out []alarm
	for _, a := range blocks(files, "resource", "aws_cloudwatch_metric_alarm") {
		rng := a.Range()
		var src string
		for _, f := range files {
			if f.path == rng.Filename {
				src = string(rng.SliceBytes(f.src))
			}
		}
		sig := a.Labels[1] + " " + src
		for metric, pattern := range filters {
			if strings.Contains(src, metric) {
				sig += " " + pattern
			}
		}
		actions := ""
		if attr, ok := a.Body.Attributes["alarm_actions"]; ok {
			actions = sourceOf(t, files, attr.Expr)
		}
		out = append(out, alarm{name: a.Labels[1], signature: strings.ToLower(sig), actions: actions})
	}
	return out, files
}

// OPS: CloudWatch alarms for what breaks syncing or loses events, each sent to an SNS topic given by a variable.
func TestOPS_AlarmsCoverAPIErrorsLambdaDynamoDBAndSinks(t *testing.T) {
	all, _ := alarms(t)
	has := func(words ...string) func(string) bool {
		return func(sig string) bool {
			for _, w := range words {
				if !strings.Contains(sig, strings.ToLower(w)) {
					return false
				}
			}
			return true
		}
	}
	anyOf := func(checks ...func(string) bool) func(string) bool {
		return func(sig string) bool {
			for _, c := range checks {
				if c(sig) {
					return true
				}
			}
			return false
		}
	}
	for what, match := range map[string]func(string) bool{
		"API Gateway 5xx responses":                has("AWS/ApiGateway", "5XXError"),
		"413 responses (bodies above 6 MiB)":       has("413"),
		"Lambda errors":                            has("AWS/Lambda", "Errors"),
		"Lambda timeouts":                          anyOf(has("timed out"), has("timeout"), has("AWS/Lambda", "Duration")),
		"DynamoDB throttling":                      anyOf(has("AWS/DynamoDB", "ThrottledRequests"), has("AWS/DynamoDB", "ThrottleEvents")),
		"event sink failures (and dropped events)": anyOf(has("firehose"), has("kinesis"), has("sink"), has("dropped")),
	} {
		var matched []alarm
		for _, a := range all {
			if match(a.signature) {
				matched = append(matched, a)
			}
		}
		if len(matched) == 0 {
			t.Errorf("no CloudWatch alarm for %s", what)
			continue
		}
		for _, a := range matched {
			if !strings.Contains(a.actions, "var.") && !strings.Contains(a.actions, "local.") {
				t.Errorf("alarm %s (%s) must notify the SNS topic given by a variable, alarm_actions = %q", a.name, what, a.actions)
			}
		}
	}

	snsVariable := false
	for _, v := range blocks(loadTerraform(t, "terraform_modules/santa_api"), "variable") {
		if strings.Contains(strings.ToLower(v.Labels[0]), "sns") {
			snsVariable = true
		}
	}
	if !snsVariable {
		t.Error("the santa_api module has no variable for the alarm SNS topic")
	}
}
