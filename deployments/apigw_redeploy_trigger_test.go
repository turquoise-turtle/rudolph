package deployments

// The API Gateway deployment is replaced whenever its triggers change. Found in the first production deployment:
// hashing whole resource objects (jsonencode(aws_api_gateway_rest_api.api_gateway)) includes attributes AWS fills in
// after the first apply, so every plan replaced the deployment even when no route had changed. The triggers must hash
// only configuration inputs, and must still cover every setting that needs a redeployment to take effect.

import (
	"regexp"
	"strings"
	"testing"
)

var wholeResourceHash = regexp.MustCompile(`jsonencode\(\s*(data\.)?aws_[a-z0-9_]+\.[a-z0-9_]+(\[[^\]]*\])?\s*\)`)

func TestAPIGW_RedeploymentTriggersDoNotHashWholeResources(t *testing.T) {
	files := loadTerraform(t, "terraform_modules")
	for _, f := range files {
		for _, m := range wholeResourceHash.FindAll(f.src, -1) {
			t.Errorf("%s: %s hashes a whole resource, including attributes AWS computes after apply; hash its configuration inputs instead", f.path, m)
		}
	}
}

// outputSource returns the source of an output's value expression in the given module directory.
func outputSource(t *testing.T, files []tfFile, dirSuffix, name string) string {
	t.Helper()
	for _, f := range files {
		if !strings.Contains(f.path, dirSuffix) {
			continue
		}
		for _, b := range blocks([]tfFile{f}, "output", name) {
			if attr, ok := b.Body.Attributes["value"]; ok {
				return sourceOf(t, files, attr.Expr)
			}
		}
	}
	t.Fatalf("output %q not found under %s", name, dirSuffix)
	return ""
}

func requireAll(t *testing.T, what, src string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(src, want) {
			t.Errorf("%s does not include %s, so changing it would not redeploy the API:\n%s", what, want, src)
		}
	}
}

func TestAPIGW_RedeploymentTriggersCoverSettingsThatNeedARedeploy(t *testing.T) {
	files := loadTerraform(t, "terraform_modules")

	var triggers string
	for _, b := range blocks(files, "resource", "aws_api_gateway_deployment", "api_deployment") {
		attr, ok := b.Body.Attributes["triggers"]
		if !ok {
			t.Fatal("aws_api_gateway_deployment.api_deployment has no triggers")
		}
		triggers = sourceOf(t, files, attr.Expr)
	}
	if triggers == "" {
		t.Fatal("aws_api_gateway_deployment.api_deployment not found")
	}
	// REST API settings that only reach clients through a new deployment.
	requireAll(t, "the deployment's triggers", triggers,
		"binary_media_types", "policy", "api_key_source", "api_gateway_authorizer_sha", "integration_shas")

	// Every gateway response, by its configured attributes.
	for _, b := range blocks(files, "resource", "aws_api_gateway_gateway_response") {
		requireAll(t, "the deployment's triggers", triggers, "aws_api_gateway_gateway_response."+b.Labels[1]+",")
	}
	requireAll(t, "the deployment's triggers", triggers, "response_type", "status_code", "response_templates")

	requireAll(t, "the integration hash (api_resource integration_sha)",
		outputSource(t, files, "api_resource", "integration_sha"),
		"var.gateway_resource_id", "var.http_method", "var.lambda_invocation_arn", "var.authorizer_id", "var.request_model")

	requireAll(t, "the authoriser hash (api_gateway_authorizer_sha)",
		outputSource(t, files, "authorizer", "api_gateway_authorizer_sha"),
		"authorizer_uri", "identity_source", "authorizer_result_ttl_in_seconds")
}
