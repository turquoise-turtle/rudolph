// Package deployments holds static checks on the Terraform configuration. They catch deployment breakage
// that `go test` would otherwise never see. See docs/audit/shortcomings.md for the IDs.
package deployments

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

type tfFile struct {
	path string
	body *hclsyntax.Body
	src  []byte
}

func loadTerraform(t *testing.T, root string) []tfFile {
	t.Helper()
	var files []tfFile
	parser := hclparse.NewParser()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() && info.Name() == ".terraform" {
			return filepath.SkipDir
		}
		if info.IsDir() || !strings.HasSuffix(path, ".tf") {
			return nil
		}
		f, diags := parser.ParseHCLFile(path)
		if diags.HasErrors() {
			t.Fatalf("parse %s: %s", path, diags.Error())
		}
		src, _ := os.ReadFile(path)
		files = append(files, tfFile{path: path, body: f.Body.(*hclsyntax.Body), src: src})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func blocks(files []tfFile, typ string, labels ...string) []*hclsyntax.Block {
	var out []*hclsyntax.Block
	for _, f := range files {
		for _, b := range f.body.Blocks {
			if b.Type != typ || len(b.Labels) < len(labels) {
				continue
			}
			match := true
			for i, l := range labels {
				if b.Labels[i] != l {
					match = false
				}
			}
			if match {
				out = append(out, b)
			}
		}
	}
	return out
}

func literal(attr *hclsyntax.Attribute) (string, bool) {
	v, diags := attr.Expr.Value(nil)
	if diags.HasErrors() || !v.IsKnown() || v.IsNull() {
		return "", false
	}
	if v.Type().FriendlyName() != "string" {
		return "", false
	}
	return v.AsString(), true
}

func nested(b *hclsyntax.Block, typ string) []*hclsyntax.Block {
	var out []*hclsyntax.Block
	for _, nb := range b.Body.Blocks {
		if nb.Type == typ {
			out = append(out, nb)
		}
	}
	return out
}

// D2: an environment must deploy the modules in this checkout, not whatever is on a remote default branch.
func TestD2_ExampleEnvironmentUsesLocalModules(t *testing.T) {
	for _, dir := range []string{"environments/example", "terraform_modules"} {
		for _, m := range blocks(loadTerraform(t, dir), "module") {
			src, ok := literal(m.Body.Attributes["source"])
			if !ok {
				t.Fatalf("module %v has a non-literal source", m.Labels)
			}
			if !strings.HasPrefix(src, "./") && !strings.HasPrefix(src, "../") {
				t.Errorf("%s: module %q sources %q; deployments must use the local modules", dir, m.Labels[0], src)
			}
		}
	}
}

// D1: provider constraints that can never be satisfied together make `terraform init` fail.
func TestD1_AWSProviderConstraintsAreConsistent(t *testing.T) {
	constraints := map[string][]string{}
	for _, f := range loadTerraform(t, ".") {
		for _, tb := range f.body.Blocks {
			if tb.Type != "terraform" {
				continue
			}
			for _, rp := range nested(tb, "required_providers") {
				aws, ok := rp.Body.Attributes["aws"]
				if !ok {
					continue
				}
				obj, ok := aws.Expr.(*hclsyntax.ObjectConsExpr)
				if !ok {
					continue
				}
				for _, item := range obj.Items {
					key, _ := item.KeyExpr.Value(nil)
					if key.AsString() == "version" {
						val, _ := item.ValueExpr.Value(nil)
						constraints[val.AsString()] = append(constraints[val.AsString()], f.path)
					}
				}
			}
		}
	}
	if len(constraints) > 1 {
		var parts []string
		for c, files := range constraints {
			parts = append(parts, c+" in "+strings.Join(files, ", "))
		}
		sort.Strings(parts)
		t.Errorf("conflicting hashicorp/aws version constraints:\n  %s", strings.Join(parts, "\n  "))
	}
}

// L13: every DynamoDB index the Go code queries must exist in the table definition.
func TestL13_IndexesUsedByCodeExistInTerraform(t *testing.T) {
	tfIndexes := map[string]bool{}
	for _, table := range blocks(loadTerraform(t, "terraform_modules"), "resource", "aws_dynamodb_table") {
		for _, gsi := range nested(table, "global_secondary_index") {
			if name, ok := literal(gsi.Body.Attributes["name"]); ok {
				tfIndexes[name] = true
			}
		}
	}

	indexConst := regexp.MustCompile(`(?m)^\s*\w+_GSI\s+string\s*=\s*"([^"]+)"`)
	err := filepath.Walk("../pkg", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, _ := os.ReadFile(path)
		for _, m := range indexConst.FindAllStringSubmatch(string(src), -1) {
			if !tfIndexes[m[1]] {
				t.Errorf("%s queries index %q, which Terraform never creates (have %v)", path, m[1], tfIndexes)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// D3: new S3 buckets have ACLs disabled (BucketOwnerEnforced); setting an ACL fails unless ownership is changed.
func TestD3_S3ACLsRequireOwnershipControls(t *testing.T) {
	for _, f := range loadTerraform(t, "terraform_modules") {
		usesACL := len(blocks([]tfFile{f}, "resource", "aws_s3_bucket_acl")) > 0
		for _, b := range blocks([]tfFile{f}, "resource", "aws_s3_bucket") {
			if _, ok := b.Body.Attributes["acl"]; ok {
				usesACL = true
			}
		}
		if usesACL && len(blocks([]tfFile{f}, "resource", "aws_s3_bucket_ownership_controls")) == 0 {
			t.Errorf("%s sets S3 ACLs without aws_s3_bucket_ownership_controls; apply fails on new buckets", f.path)
		}
	}
}

// C4: Lambda log groups default to never expiring.
func TestC4_LambdaLogGroupsHaveRetention(t *testing.T) {
	modules := map[string][]tfFile{}
	for _, f := range loadTerraform(t, "terraform_modules") {
		modules[filepath.Dir(f.path)] = append(modules[filepath.Dir(f.path)], f)
	}
	for dir, files := range modules {
		if len(blocks(files, "resource", "aws_lambda_function")) == 0 {
			continue
		}
		ok := false
		for _, lg := range blocks(files, "resource", "aws_cloudwatch_log_group") {
			if _, has := lg.Body.Attributes["retention_in_days"]; has {
				ok = true
			}
		}
		if !ok {
			t.Errorf("%s defines Lambda functions without a log group that sets retention_in_days", dir)
		}
	}
}

// P1: Santa deflates request bodies by default. With a REST API proxy integration the body only reaches
// Lambda intact if the payload is treated as binary (base64 encoded); HTTP APIs do this automatically.
func TestP1_APIGatewayDeliversCompressedBodiesIntact(t *testing.T) {
	files := loadTerraform(t, "terraform_modules")
	if len(blocks(files, "resource", "aws_apigatewayv2_api")) > 0 {
		return
	}
	for _, api := range blocks(files, "resource", "aws_api_gateway_rest_api") {
		attr, ok := api.Body.Attributes["binary_media_types"]
		if !ok {
			t.Errorf("REST API %q has no binary_media_types; deflated Santa requests are corrupted", api.Labels[1])
			continue
		}
		rng := attr.Expr.Range()
		var src []byte
		for _, f := range files {
			if f.path == rng.Filename {
				src = rng.SliceBytes(f.src)
			}
		}
		if !strings.Contains(string(src), "application/json") && !strings.Contains(string(src), "*/*") {
			t.Errorf("REST API binary_media_types %s must cover application/json", src)
		}
	}
}

func sourceOf(t *testing.T, files []tfFile, expr hclsyntax.Expression) string {
	rng := expr.Range()
	for _, f := range files {
		if f.path == rng.Filename {
			return string(rng.SliceBytes(f.src))
		}
	}
	t.Fatalf("source for %s not found", rng)
	return ""
}

// S1/C1: the authorizer is keyed on the Authorization header and its result is cached, so one warm policy
// serves a host's whole sync instead of one extra Lambda invocation per request.
func TestS1_AuthorizerUsesTokenHeaderAndCaches(t *testing.T) {
	files := loadTerraform(t, "terraform_modules")
	auths := blocks(files, "resource", "aws_api_gateway_authorizer")
	if len(auths) == 0 {
		t.Fatal("no API Gateway authorizer defined")
	}
	for _, a := range auths {
		src, _ := literal(a.Body.Attributes["identity_source"])
		if !strings.Contains(src, "method.request.header.Authorization") {
			t.Errorf("authorizer %q identity_source = %q, want the Authorization header", a.Labels[1], src)
		}
		ttl := a.Body.Attributes["authorizer_result_ttl_in_seconds"]
		if ttl == nil || strings.TrimSpace(sourceOf(t, files, ttl.Expr)) == "0" {
			t.Errorf("authorizer %q does not cache results", a.Labels[1])
		}
	}
	found := false
	for _, m := range blocks(files, "module", "rudolph_api_authorizer") {
		if env, ok := m.Body.Attributes["env_vars"]; ok && strings.Contains(sourceOf(t, files, env.Expr), "SYNC_TOKEN_SHA256") {
			found = true
		}
	}
	if !found {
		t.Error("authorizer Lambda does not receive SYNC_TOKEN_SHA256")
	}
}

// S1: optional mTLS on the custom domain; the default execute-api endpoint must be disableable or mTLS is bypassable.
func TestS1_OptionalMutualTLS(t *testing.T) {
	files := loadTerraform(t, "terraform_modules")
	for _, d := range blocks(files, "resource", "aws_api_gateway_domain_name") {
		ok := false
		for _, db := range nested(d, "dynamic") {
			if len(db.Labels) > 0 && db.Labels[0] == "mutual_tls_authentication" {
				ok = true
			}
		}
		if !ok {
			t.Errorf("custom domain %q has no optional mutual_tls_authentication block", d.Labels[1])
		}
	}
	for _, api := range blocks(files, "resource", "aws_api_gateway_rest_api") {
		if _, ok := api.Body.Attributes["disable_execute_api_endpoint"]; !ok {
			t.Errorf("REST API %q cannot disable its default endpoint, which would bypass mTLS", api.Labels[1])
		}
	}
}
