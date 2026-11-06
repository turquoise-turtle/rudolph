package deployments

// Fixes for problems found while writing the first real-AWS smoke test runbook (docs/smoke-test.md, "Doc issues
// found"), checked statically before anything is applied.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// resolveString gives a string attribute's value, following one level of local.<name> within the same directory.
func resolveString(t *testing.T, files []tfFile, attr *hclsyntax.Attribute) string {
	t.Helper()
	if v, ok := literal(attr); ok {
		return v
	}
	m := regexp.MustCompile(`^local\.([A-Za-z0-9_]+)$`).FindStringSubmatch(strings.TrimSpace(sourceOf(t, files, attr.Expr)))
	if m == nil {
		return ""
	}
	dir := filepath.Dir(attr.SrcRange.Filename)
	for _, f := range files {
		if filepath.Dir(f.path) != dir {
			continue
		}
		for _, l := range blocks([]tfFile{f}, "locals") {
			if a, ok := l.Body.Attributes[m[1]]; ok {
				v, _ := literal(a)
				return v
			}
		}
	}
	return ""
}

// Amazon Linux 2 reached end of support on 30 June 2026; new functions on provided.al2 may be refused.
func TestSmoke_LambdaFunctionsUseAL2023(t *testing.T) {
	files := loadTerraform(t, "terraform_modules")
	fns := blocks(files, "resource", "aws_lambda_function")
	if len(fns) == 0 {
		t.Fatal("no aws_lambda_function resources found")
	}
	for _, b := range fns {
		attr, ok := b.Body.Attributes["runtime"]
		if !ok {
			t.Errorf("aws_lambda_function.%s has no runtime", b.Labels[1])
			continue
		}
		if v := resolveString(t, files, attr); v != "provided.al2023" {
			t.Errorf("aws_lambda_function.%s runtime %q, want provided.al2023", b.Labels[1], v)
		}
	}
}

// S3 server access logs are only delivered to a destination bucket whose default encryption is SSE-S3. Each module
// that turns on access logging creates its destination as aws_s3_bucket.s3_logging.
func TestSmoke_AccessLogDestinationsUseSSES3(t *testing.T) {
	files := loadTerraform(t, "terraform_modules")
	byDir := map[string][]tfFile{}
	for _, f := range files {
		byDir[filepath.Dir(f.path)] = append(byDir[filepath.Dir(f.path)], f)
	}
	found := 0
	for dir, module := range byDir {
		if len(blocks(module, "resource", "aws_s3_bucket_logging")) == 0 {
			continue
		}
		found++
		if len(blocks(module, "resource", "aws_s3_bucket", "s3_logging")) == 0 {
			t.Errorf("%s: turns on access logging but has no aws_s3_bucket.s3_logging", dir)
			continue
		}
		encs := blocks(module, "resource", "aws_s3_bucket_server_side_encryption_configuration", "s3_logging")
		if len(encs) != 1 {
			t.Errorf("%s: %d encryption configurations for the access-log bucket, want 1", dir, len(encs))
			continue
		}
		for _, rule := range nested(encs[0], "rule") {
			for _, def := range nested(rule, "apply_server_side_encryption_by_default") {
				if v, _ := literal(def.Body.Attributes["sse_algorithm"]); v != "AES256" {
					t.Errorf("%s: the access-log bucket uses sse_algorithm %q; access logs are only delivered to SSE-S3 (AES256) buckets", dir, v)
				}
				if _, ok := def.Body.Attributes["kms_master_key_id"]; ok {
					t.Errorf("%s: the access-log bucket names a KMS key", dir)
				}
			}
		}
		for _, b := range blocks(module, "resource", "aws_kms_key", "s3_logging") {
			t.Errorf("%s: aws_kms_key.%s is left over from the access-log bucket (US$1 a month for nothing)", dir, b.Labels[1])
		}
	}
	if found == 0 {
		t.Fatal("no module turns on access logging")
	}
}

// The authoriser rejects a wrong token by returning the "Unauthorized" error, which Lambda counts in Errors. A
// scanner with a made-up token must not page whoever receives lambda_errors (api_4xx covers rejected tokens; an
// authoriser that really fails makes API Gateway answer 500, which api_5xx covers).
func TestSmoke_WrongTokenDoesNotFireLambdaErrors(t *testing.T) {
	files := loadTerraform(t, filepath.Join("terraform_modules", "santa_api"))
	var module []tfFile
	for _, f := range files {
		if filepath.Dir(f.path) == filepath.Join("terraform_modules", "santa_api") {
			module = append(module, f)
		}
	}
	alarms := blocks(module, "resource", "aws_cloudwatch_metric_alarm", "lambda_errors")
	if len(alarms) != 1 {
		t.Fatalf("found %d lambda_errors alarms, want 1", len(alarms))
	}
	var forEach string
	for _, d := range nested(alarms[0], "dynamic") {
		if attr, ok := d.Body.Attributes["for_each"]; ok {
			forEach = sourceOf(t, module, attr.Expr)
		}
	}
	m := regexp.MustCompile(`^local\.([A-Za-z0-9_]+)$`).FindStringSubmatch(strings.TrimSpace(forEach))
	if m == nil {
		t.Fatalf("lambda_errors for_each is %q, want a local listing the functions", forEach)
	}
	var def string
	for _, l := range blocks(module, "locals") {
		if attr, ok := l.Body.Attributes[m[1]]; ok {
			def = sourceOf(t, module, attr.Expr)
		}
	}
	if def == "" {
		t.Fatalf("local.%s not found", m[1])
	}
	if strings.Contains(def, "authorizer") {
		t.Errorf("local.%s, the functions lambda_errors watches, includes the authoriser:\n%s", m[1], def)
	}
	for _, fn := range []string{"preflight", "eventupload", "ruledownload", "postflight", "ranker"} {
		if !strings.Contains(def, fn) {
			t.Errorf("local.%s no longer includes %s", m[1], fn)
		}
	}
}

// Santa 2026.8 and newer are North Pole Security's: preferences under com.northpolesec.santa, signed by team ID
// ZMCG7MLDV9. The example profiles must work as they are, sync token header included.
func TestSmoke_ExampleProfilesAreForNorthPoleSecuritySanta(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "configs", "*"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no files in configs/: %v", err)
	}
	for _, p := range paths {
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		s := string(src)
		if strings.Contains(filepath.Base(p), "com.google") {
			t.Errorf("%s: file name still uses com.google", p)
		}
		if strings.Contains(s, "com.google.santa") {
			t.Errorf("%s still names com.google.santa", p)
		}
		if strings.Contains(s, "EQHXZ8M8AV") {
			t.Errorf("%s still names Google's team ID EQHXZ8M8AV; North Pole Security's is ZMCG7MLDV9", p)
		}
	}
	read := func(name string) string {
		src, err := os.ReadFile(filepath.Join("..", "configs", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(src)
	}
	cfg := read("santa-configuration.mobileconfig")
	for _, want := range []string{"com.northpolesec.santa", "SyncBaseURL", "SyncExtraHeaders", "Authorization", "Bearer ", "MachineIDPlist", "/Library/Preferences/com.northpolesec.santa.machine-mapping.plist"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("santa-configuration.mobileconfig does not contain %q", want)
		}
	}
	for _, name := range []string{"santa-sysext.mobileconfig", "santa-tcc.mobileconfig"} {
		if s := read(name); !strings.Contains(s, "ZMCG7MLDV9") || !strings.Contains(s, "com.northpolesec.santa") {
			t.Errorf("%s does not name North Pole Security's team ID ZMCG7MLDV9 and com.northpolesec.santa", name)
		}
	}
	if !strings.Contains(read("com.northpolesec.santa.machine-mapping.plist"), "MachineUUID") {
		t.Error("com.northpolesec.santa.machine-mapping.plist has no MachineUUID")
	}
}

// The example environment exposes the outputs the runbook and rudolph-web's deploy need.
func TestSmoke_ExampleEnvironmentExposesOutputs(t *testing.T) {
	files := loadTerraform(t, filepath.Join("environments", "example"))
	for _, name := range []string{"sync_base_url", "events_dynamodb_table_arn", "events_dynamodb_table_name", "store_kms_key_arn", "api_gateway_id"} {
		if len(blocks(files, "output", name)) != 1 {
			t.Errorf("the example environment has no output %q", name)
		}
	}
	src, err := os.ReadFile(filepath.Join("environments", "example", "config.auto.tfvars.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "[KINESIS|FIREHOSE]") {
		t.Error("config.auto.tfvars.json still has the [KINESIS|FIREHOSE] placeholder; NONE is valid and the simplest start")
	}
}
