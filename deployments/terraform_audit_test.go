package deployments

// Terraform security audit (owner approved, 2026-11): an opt-in API Gateway access log and an opt-in WAF, both off by
// default so nothing changes for a deployment that does not ask for them; S3 state locking; a committed provider lock
// file; and expiry on the S3 server access-log buckets, which otherwise grow for ever.

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

var localRef = regexp.MustCompile(`local\.([A-Za-z0-9_]+)`)

// expandLocals appends the definitions of the locals src refers to (and theirs, a few levels deep), so a check on a
// count or format sees through `local.enabled = var.enable_x`.
func expandLocals(t *testing.T, files []tfFile, dir, src string) string {
	t.Helper()
	seen := map[string]bool{}
	out := src
	for range 4 {
		grew := false
		for _, m := range localRef.FindAllStringSubmatch(out, -1) {
			if seen[m[1]] {
				continue
			}
			seen[m[1]] = true
			for _, f := range files {
				if filepath.Dir(f.path) != filepath.Clean(dir) {
					continue
				}
				for _, l := range blocks([]tfFile{f}, "locals") {
					if a, ok := l.Body.Attributes[m[1]]; ok {
						out += "\n" + sourceOf(t, files, a.Expr)
						grew = true
					}
				}
			}
		}
		if !grew {
			break
		}
	}
	return out
}

// countText is the source of a block's count or for_each, with the locals it uses, or "" when it has neither.
func countText(t *testing.T, files []tfFile, b *hclsyntax.Block) string {
	t.Helper()
	for _, name := range []string{"count", "for_each"} {
		if a, ok := b.Body.Attributes[name]; ok {
			return expandLocals(t, files, filepath.Dir(b.Range().Filename), sourceOf(t, files, a.Expr))
		}
	}
	return ""
}

// gatedOn reports whether a resource only exists when var.<name> asks for it: its own count or for_each, or that of
// the module call that instantiates its directory, depends on the variable.
func gatedOn(t *testing.T, files []tfFile, b *hclsyntax.Block, name string) bool {
	t.Helper()
	ref := regexp.MustCompile(`\bvar\.` + regexp.QuoteMeta(name) + `\b`)
	if ref.MatchString(countText(t, files, b)) {
		return true
	}
	dir := filepath.Dir(b.Range().Filename)
	for _, m := range blocks(files, "module") {
		src, ok := literal(m.Body.Attributes["source"])
		if !ok || filepath.Join(filepath.Dir(m.Range().Filename), src) != dir {
			continue
		}
		if ref.MatchString(countText(t, files, m)) {
			return true
		}
	}
	return false
}

// variableType returns the source of a variable's type, or "" when it is not declared.
func variableType(t *testing.T, files []tfFile, name string) string {
	t.Helper()
	for _, v := range blocks(files, "variable", name) {
		if a, ok := v.Body.Attributes["type"]; ok {
			return strings.TrimSpace(sourceOf(t, files, a.Expr))
		}
	}
	return ""
}

// requireModuleVariable checks a santa_api module variable's type and default.
func requireModuleVariable(t *testing.T, name, typ, def string) {
	t.Helper()
	module := filesIn(loadTerraform(t, santaAPI), santaAPI)
	if len(blocks(module, "variable", name)) == 0 {
		t.Errorf("the santa_api module has no variable %q (%s, default %s)", name, typ, def)
		return
	}
	if got := variableType(t, module, name); got != typ {
		t.Errorf("variable %s has type %q, want %s", name, got, typ)
	}
	if got, ok := variableDefault(t, module, name); !ok || got != def {
		t.Errorf("variable %s defaults to %q, want %s", name, got, def)
	}
}

// requireExamplePasses checks that the example environment declares a variable and hands it to the module, so an
// environment made by make init-env can set it in config.auto.tfvars.json.
func requireExamplePasses(t *testing.T, name string) {
	t.Helper()
	example := loadTerraform(t, filepath.Join("environments", "example"))
	if len(blocks(example, "variable", name)) != 1 {
		t.Errorf("the example environment does not declare variable %s", name)
	}
	mods := blocks(example, "module", "santa_api")
	if len(mods) != 1 {
		t.Fatalf("the example environment has %d module \"santa_api\" blocks, want 1", len(mods))
	}
	a, ok := mods[0].Body.Attributes[name]
	if !ok || strings.TrimSpace(sourceOf(t, example, a.Expr)) != "var."+name {
		t.Errorf("the example environment does not pass %s = var.%s to the santa_api module", name, name)
	}
}

func boolTrue(t *testing.T, files []tfFile, b *hclsyntax.Block, attr string) bool {
	t.Helper()
	a, ok := b.Body.Attributes[attr]
	return ok && strings.TrimSpace(sourceOf(t, files, a.Expr)) == "true"
}

// path follows nested blocks of the given types and returns the first match, or nil.
func path(b *hclsyntax.Block, types ...string) *hclsyntax.Block {
	for _, typ := range types {
		next := nested(b, typ)
		if len(next) == 0 {
			return nil
		}
		b = next[0]
	}
	return b
}

// rulesOf returns a resource's rule blocks, static or the content of a dynamic "rule".
func rulesOf(b *hclsyntax.Block) []*hclsyntax.Block {
	out := nested(b, "rule")
	for _, d := range nested(b, "dynamic") {
		if len(d.Labels) > 0 && d.Labels[0] == "rule" {
			out = append(out, nested(d, "content")...)
		}
	}
	return out
}

func readDoc(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

//
// 1. API Gateway access log
//

func TestTFA1_APIAccessLogsAreOptIn(t *testing.T) {
	requireModuleVariable(t, "enable_api_access_logs", "bool", "false")
	requireExamplePasses(t, "enable_api_access_logs")
}

// Without an access log, a request refused by the authorizer or by throttling leaves no trace: the Lambda never runs.
func TestTFA1_StageWritesAccessLogWhenEnabled(t *testing.T) {
	all := loadTerraform(t, santaAPI)
	stages := blocks(all, "resource", "aws_api_gateway_stage", "stage")
	if len(stages) != 1 {
		t.Fatalf("found %d aws_api_gateway_stage.stage, want 1", len(stages))
	}
	stage := stages[0]
	if len(nested(stage, "access_log_settings")) != 0 {
		t.Error("aws_api_gateway_stage.stage has a static access_log_settings block: with enable_api_access_logs off the stage must not change")
	}
	var settings *hclsyntax.Block
	for _, d := range nested(stage, "dynamic") {
		if len(d.Labels) > 0 && d.Labels[0] == "access_log_settings" {
			if fe, ok := d.Body.Attributes["for_each"]; !ok ||
				!strings.Contains(expandLocals(t, all, filepath.Dir(d.Range().Filename), sourceOf(t, all, fe.Expr)), "var.enable_api_access_logs") {
				t.Error("the stage's dynamic access_log_settings is not driven by var.enable_api_access_logs")
			}
			settings = path(d, "content")
		}
	}
	if settings == nil {
		t.Fatal("aws_api_gateway_stage.stage has no dynamic \"access_log_settings\" block, so it never writes an access log")
	}

	dest, ok := settings.Body.Attributes["destination_arn"]
	if !ok {
		t.Fatal("access_log_settings has no destination_arn")
	}
	m := regexp.MustCompile(`aws_cloudwatch_log_group\.([A-Za-z0-9_]+)`).FindStringSubmatch(sourceOf(t, all, dest.Expr))
	if m == nil {
		t.Fatalf("access_log_settings destination_arn = %s, want a CloudWatch log group of the module", sourceOf(t, all, dest.Expr))
	}
	groups := blocks(all, "resource", "aws_cloudwatch_log_group", m[1])
	if len(groups) != 1 {
		t.Fatalf("access log destination aws_cloudwatch_log_group.%s not found", m[1])
	}
	if !gatedOn(t, all, groups[0], "enable_api_access_logs") {
		t.Errorf("aws_cloudwatch_log_group.%s exists even with enable_api_access_logs off; it must be the access log's own group", m[1])
	}
	if r, ok := groups[0].Body.Attributes["retention_in_days"]; !ok || strings.TrimSpace(sourceOf(t, all, r.Expr)) != "var.log_retention_in_days" {
		t.Errorf("aws_cloudwatch_log_group.%s must keep logs for var.log_retention_in_days, not for ever", m[1])
	}

	f, ok := settings.Body.Attributes["format"]
	if !ok {
		t.Fatal("access_log_settings has no format")
	}
	format := expandLocals(t, all, filepath.Dir(settings.Range().Filename), sourceOf(t, all, f.Expr))
	for _, want := range [][]string{
		{"$context.requestId"},
		{"$context.identity.sourceIp"},
		{"$context.requestTime"},
		{"$context.httpMethod"},
		{"$context.resourcePath"},
		{"$context.status"},
		{"$context.authorizer.error"},
		{"$context.integration.error", "$context.integrationErrorMessage"},
	} {
		found := false
		for _, w := range want {
			found = found || strings.Contains(format, w)
		}
		if !found {
			t.Errorf("the access log format has no %s", strings.Join(want, " or "))
		}
	}
}

// aws_api_gateway_account sets the one CloudWatch role that every API Gateway API in the account and region uses.
// Creating it unconditionally would replace another stack's role, so the module may only manage it when asked to by a
// variable of its own, off by default.
func TestTFA1_APIGatewayAccountRoleIsExplicit(t *testing.T) {
	all := loadTerraform(t, ".")
	for _, b := range blocks(all, "resource", "aws_api_gateway_account") {
		where := b.Range().String()
		ctl := countText(t, all, b)
		if ctl == "" {
			t.Errorf("%s: aws_api_gateway_account.%s is created unconditionally and would override the account's API Gateway CloudWatch role", where, b.Labels[1])
			continue
		}
		var own []string
		for _, m := range regexp.MustCompile(`\bvar\.([A-Za-z0-9_]+)`).FindAllStringSubmatch(ctl, -1) {
			if m[1] != "enable_api_access_logs" {
				own = append(own, m[1])
			}
		}
		if len(own) == 0 {
			t.Errorf("%s: aws_api_gateway_account.%s follows enable_api_access_logs alone; managing the account-wide role needs a separate variable", where, b.Labels[1])
		}
		dirFiles := filesIn(all, filepath.Dir(b.Range().Filename))
		for _, v := range own {
			if d, ok := variableDefault(t, dirFiles, v); ok && d == "true" {
				t.Errorf("%s: var.%s defaults to true, so the account's API Gateway CloudWatch role is replaced unless someone opts out", where, v)
			}
		}
	}

	doc := readDoc(t, "docs/deploy.md")
	if !strings.Contains(doc, "enable_api_access_logs") {
		t.Error("docs/deploy.md does not document enable_api_access_logs")
	}
	lower := strings.ToLower(doc)
	found := false
	for _, w := range []string{"amazonapigatewaypushtocloudwatchlogs", "cloudwatchrolearn", "cloudwatch_role_arn", "aws_api_gateway_account"} {
		found = found || strings.Contains(lower, w)
	}
	if !found {
		t.Error("docs/deploy.md does not explain that API Gateway access logging needs the account's API Gateway CloudWatch role (cloudwatchRoleArn, an account-wide setting per region)")
	}
}

//
// 2. State locking
//

// Without a lock, two applies at once (two people, or CI and a person) both write the state and one loses its changes.
func TestTFA2_StateIsLockedInS3(t *testing.T) {
	example := loadTerraform(t, filepath.Join("environments", "example"))
	var backend *hclsyntax.Block
	for _, tb := range blocks(example, "terraform") {
		for _, b := range nested(tb, "backend") {
			if len(b.Labels) > 0 && b.Labels[0] == "s3" {
				backend = b
			}
		}
	}
	if backend == nil {
		t.Fatal("the example environment has no backend \"s3\"")
	}
	if !boolTrue(t, example, backend, "use_lockfile") {
		t.Error("the example's S3 backend does not set use_lockfile = true, so concurrent applies are not locked out")
	}
	if _, ok := backend.Body.Attributes["dynamodb_table"]; ok {
		t.Error("the example's S3 backend uses dynamodb_table, which is deprecated; S3 native locking (use_lockfile) replaces it")
	}

	// use_lockfile needs Terraform 1.10.
	for _, tb := range blocks(example, "terraform") {
		if a, ok := tb.Body.Attributes["required_version"]; ok {
			v, _ := literal(a)
			m := regexp.MustCompile(`^>=\s*1\.(\d+)`).FindStringSubmatch(v)
			if m == nil {
				t.Errorf("required_version %q is not a >= 1.x constraint", v)
			} else if n, _ := strconv.Atoi(m[1]); n < 10 {
				t.Errorf("required_version %q allows Terraform before 1.10, which has no S3 lock file", v)
			}
		}
	}

	// The lock is a second object, <key>.tflock, that Terraform creates and deletes: a state bucket policy that only
	// allows reading and writing the state key breaks every plan.
	found := false
	for _, rel := range []string{"docs/deploy.md", "docs/smoke-test.md", "deployments/environments/example/README.md"} {
		for _, para := range regexp.MustCompile(`\n\s*\n`).Split(readDoc(t, rel), -1) {
			if strings.Contains(para, ".tflock") && strings.Contains(para, "s3:DeleteObject") {
				found = true
			}
		}
	}
	if !found {
		t.Error("no doc (deploy.md, smoke-test.md or the example README) explains, in one paragraph, that the state bucket must allow s3:DeleteObject on the <key>.tflock lock object")
	}
	if readme := readDoc(t, "deployments/environments/example/README.md"); !strings.Contains(readme, "use_lockfile") {
		t.Error("the example README, which explains each backend setting, does not explain use_lockfile")
	}
}

//
// 3. Provider lock file
//

const exampleLock = "environments/example/.terraform.lock.hcl"

// Without a committed lock file every init picks the newest matching provider and trusts whatever checksums it
// downloads; with it, a changed or substituted provider package fails init.
func TestTFA3_ExampleProviderLockFileIsCommitted(t *testing.T) {
	for p, ignored := range map[string]bool{
		"deployments/" + exampleLock:                        false,
		"deployments/environments/prod/.terraform.lock.hcl": true,
	} {
		err := exec.Command("git", "-C", "..", "check-ignore", "-q", "--no-index", p).Run()
		if got := err == nil; got != ignored {
			t.Errorf("%s: ignored by .gitignore %v, want %v", p, got, ignored)
		}
	}
	if err := exec.Command("git", "-C", "..", "ls-files", "--error-unmatch", "deployments/"+exampleLock).Run(); err != nil {
		t.Errorf("deployments/%s is not tracked by Git", exampleLock)
	}

	src, err := os.ReadFile(exampleLock)
	if err != nil {
		t.Fatalf("no provider lock file: %v (create it with terraform providers lock -platform=linux_amd64 -platform=darwin_arm64 -platform=darwin_amd64)", err)
	}
	f, diags := hclparse.NewParser().ParseHCL(src, exampleLock)
	if diags.HasErrors() {
		t.Fatalf("parse %s: %s", exampleLock, diags.Error())
	}
	lock := []tfFile{{path: exampleLock, body: f.Body.(*hclsyntax.Body), src: src}}
	providers := blocks(lock, "provider", "registry.terraform.io/hashicorp/aws")
	if len(providers) != 1 {
		t.Fatalf("%s locks hashicorp/aws %d times, want once", exampleLock, len(providers))
	}
	p := providers[0]
	version, _ := literal(p.Body.Attributes["version"])
	constraints, _ := literal(p.Body.Attributes["constraints"])

	want := ""
	example := loadTerraform(t, filepath.Join("environments", "example"))
	for _, tb := range blocks(example, "terraform") {
		for _, rp := range nested(tb, "required_providers") {
			if a, ok := rp.Body.Attributes["aws"]; ok {
				if m := regexp.MustCompile(`version\s*=\s*"([^"]+)"`).FindStringSubmatch(sourceOf(t, example, a.Expr)); m != nil {
					want = m[1]
				}
			}
		}
	}
	if want == "" {
		t.Fatal("versions.tf has no hashicorp/aws version constraint")
	}
	if constraints != want {
		t.Errorf("%s records constraints %q, but versions.tf requires %q: run terraform init again", exampleLock, constraints, want)
	}
	if !pessimisticMatch(t, want, version) {
		t.Errorf("%s pins hashicorp/aws %s, outside versions.tf's %q", exampleLock, version, want)
	}

	// One h1: hash per platform the lock was made for (zh: hashes cover the registry's zips for every platform but
	// only the platform that ran init gets h1:), so a lock from a plain init fails on the other platforms' computers.
	h1 := 0
	if a, ok := p.Body.Attributes["hashes"]; ok {
		if tuple, ok := a.Expr.(*hclsyntax.TupleConsExpr); ok {
			for _, e := range tuple.Exprs {
				if v, ok := literal(&hclsyntax.Attribute{Expr: e}); ok && strings.HasPrefix(v, "h1:") {
					h1++
				}
			}
		}
	}
	if h1 < 3 {
		t.Errorf("%s has %d h1: hashes for hashicorp/aws, want at least 3 (linux_amd64, darwin_arm64, darwin_amd64)", exampleLock, h1)
	}
}

// pessimisticMatch handles the "~> X.Y" and "~> X.Y.Z" constraints the environments use.
func pessimisticMatch(t *testing.T, constraint, version string) bool {
	t.Helper()
	m := regexp.MustCompile(`^~>\s*(\d+(?:\.\d+)+)$`).FindStringSubmatch(strings.TrimSpace(constraint))
	if m == nil {
		t.Fatalf("constraint %q is not a ~> constraint; extend pessimisticMatch", constraint)
	}
	parse := func(s string) []int {
		var out []int
		for _, p := range strings.Split(s, ".") {
			n, err := strconv.Atoi(p)
			if err != nil {
				return nil
			}
			out = append(out, n)
		}
		return out
	}
	min, got := parse(m[1]), parse(version)
	if got == nil || len(got) != 3 {
		return false
	}
	for i := 0; i < len(min)-1; i++ {
		if got[i] != min[i] {
			return false
		}
	}
	for i := range min {
		if got[i] != min[i] {
			return got[i] > min[i]
		}
	}
	return true
}

// make init-env copies the example by name; a lock file left out would leave every real environment unlocked.
func TestTFA3_NewEnvironmentsGetTheLockFile(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found")
	}
	tmp := t.TempDir()
	script := filepath.Join(tmp, "scripts", "new_env.sh")
	exampleDir := filepath.Join(tmp, "deployments", "environments", "example")
	for _, d := range []string{filepath.Dir(script), exampleDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	copyFile := func(from, to string) {
		b, err := os.ReadFile(from)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(to, b, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	copyFile(filepath.Join("..", "scripts", "new_env.sh"), script)
	entries, err := os.ReadDir(filepath.Join("environments", "example"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			copyFile(filepath.Join("environments", "example", e.Name()), filepath.Join(exampleDir, e.Name()))
		}
	}
	// A marker of our own, so the check is on the script whether or not the example's lock file exists yet.
	marker := []byte("# lock file copied by new_env.sh\n")
	if err := os.WriteFile(filepath.Join(exampleDir, ".terraform.lock.hcl"), marker, 0o644); err != nil {
		t.Fatal(err)
	}

	if out, err := exec.Command(bash, script, "lockcheck").CombinedOutput(); err != nil {
		t.Fatalf("new_env.sh failed: %v\n%s", err, out)
	}
	got, err := os.ReadFile(filepath.Join(tmp, "deployments", "environments", "lockcheck", ".terraform.lock.hcl"))
	if err != nil {
		t.Fatalf("make init-env does not copy the example's .terraform.lock.hcl into the new environment: %v", err)
	}
	if !bytes.Equal(got, marker) {
		t.Error("the new environment's .terraform.lock.hcl differs from the example's")
	}
}

//
// 4. WAF
//

func wafResources(t *testing.T) ([]*hclsyntax.Block, []tfFile) {
	t.Helper()
	all := loadTerraform(t, santaAPI)
	var out []*hclsyntax.Block
	for _, b := range blocks(all, "resource") {
		if len(b.Labels) == 2 && strings.HasPrefix(b.Labels[0], "aws_wafv2_") {
			out = append(out, b)
		}
	}
	return out, all
}

func TestTFA4_WAFIsOptIn(t *testing.T) {
	requireModuleVariable(t, "enable_waf", "bool", "false")
	requireExamplePasses(t, "enable_waf")

	waf, all := wafResources(t)
	if len(waf) == 0 {
		t.Fatal("the santa_api module has no aws_wafv2_* resources; enable_waf must create a web ACL for the stage")
	}
	for _, b := range waf {
		if !gatedOn(t, all, b, "enable_waf") {
			t.Errorf("%s.%s is created even with enable_waf off: give it a count or for_each on var.enable_waf", b.Labels[0], b.Labels[1])
		}
	}
}

// wafRateLimitVariable is the variable that sets the rate-based rule's limit, or "".
func wafRateLimitVariable(t *testing.T) string {
	t.Helper()
	waf, all := wafResources(t)
	for _, b := range waf {
		for _, r := range rulesOf(b) {
			if rb := path(r, "statement", "rate_based_statement"); rb != nil {
				if a, ok := rb.Body.Attributes["limit"]; ok {
					if m := regexp.MustCompile(`^var\.([A-Za-z0-9_]+)$`).FindStringSubmatch(strings.TrimSpace(sourceOf(t, all, a.Expr))); m != nil {
						return m[1]
					}
				}
			}
		}
	}
	return ""
}

// Santa sends large deflated event uploads as binary bodies. The core rule set's body size rule (8 KB) and any body
// size constraint would block them, and its other rules read compressed bytes as text, so they are left out; the IP
// reputation list and a per-IP rate limit look only at the source.
func TestTFA4_WAFRulesSuitSanta(t *testing.T) {
	waf, all := wafResources(t)
	acls := blocks(all, "resource", "aws_wafv2_web_acl")
	if len(acls) != 1 {
		t.Fatalf("found %d aws_wafv2_web_acl resources, want 1", len(acls))
	}
	acl := acls[0]
	if v, _ := literal(acl.Body.Attributes["scope"]); v != "REGIONAL" {
		t.Errorf("the web ACL's scope is %q, want REGIONAL (a regional API Gateway stage)", v)
	}
	if path(acl, "default_action", "allow") == nil {
		t.Error("the web ACL's default_action is not allow: every Santa request would be blocked")
	}
	if vc := path(acl, "visibility_config"); vc == nil || !boolTrue(t, all, vc, "cloudwatch_metrics_enabled") {
		t.Error("the web ACL's visibility_config does not turn cloudwatch_metrics_enabled on")
	}

	var text strings.Builder
	for _, b := range waf {
		text.WriteString(blockText(t, all, b))
	}
	for _, bad := range []string{"AWSManagedRulesCommonRuleSet", "size_constraint_statement"} {
		if strings.Contains(text.String(), bad) {
			t.Errorf("the WAF uses %s, which blocks Santa's large compressed event uploads", bad)
		}
	}

	rate, reputation := false, false
	for _, b := range waf {
		for _, r := range rulesOf(b) {
			name, _ := literal(r.Body.Attributes["name"])
			if vc := path(r, "visibility_config"); vc == nil || !boolTrue(t, all, vc, "cloudwatch_metrics_enabled") {
				t.Errorf("WAF rule %q does not turn cloudwatch_metrics_enabled on", name)
			}
			if path(r, "statement", "rate_based_statement") != nil {
				rate = true
				if path(r, "action", "block") == nil {
					t.Errorf("rate-based rule %q does not block", name)
				}
			}
			if mg := path(r, "statement", "managed_rule_group_statement"); mg != nil {
				group, _ := literal(mg.Body.Attributes["name"])
				vendor, _ := literal(mg.Body.Attributes["vendor_name"])
				if group == "AWSManagedRulesAmazonIpReputationList" && vendor == "AWS" {
					reputation = true
					if path(r, "override_action", "none") == nil {
						t.Errorf("rule %q only counts the IP reputation list's matches: override_action must be none", name)
					}
				}
			}
		}
	}
	if !rate {
		t.Error("the WAF has no rate-based rule")
	}
	if !reputation {
		t.Error("the WAF does not use the AWS managed rule group AWSManagedRulesAmazonIpReputationList")
	}

	v := wafRateLimitVariable(t)
	if v == "" {
		t.Fatal("the rate-based rule's limit is not set by a module variable")
	}
	module := filesIn(all, santaAPI)
	d, ok := variableDefault(t, module, v)
	if n, err := strconv.Atoi(d); !ok || err != nil || n < 10 {
		t.Errorf("var.%s defaults to %q, want a whole number of requests of at least 10 (the WAF minimum)", v, d)
	}
	requireExamplePasses(t, v)
}

func TestTFA4_WAFProtectsTheStage(t *testing.T) {
	_, all := wafResources(t)
	assocs := blocks(all, "resource", "aws_wafv2_web_acl_association")
	if len(assocs) != 1 {
		t.Fatalf("found %d aws_wafv2_web_acl_association resources, want 1 for the stage", len(assocs))
	}
	a := assocs[0]
	if r, ok := a.Body.Attributes["resource_arn"]; !ok || !strings.Contains(sourceOf(t, all, r.Expr), "aws_api_gateway_stage.stage.arn") {
		t.Error("the web ACL association's resource_arn is not aws_api_gateway_stage.stage.arn")
	}
	if w, ok := a.Body.Attributes["web_acl_arn"]; !ok || !strings.Contains(sourceOf(t, all, w.Expr), "aws_wafv2_web_acl.") {
		t.Error("the web ACL association's web_acl_arn is not the module's aws_wafv2_web_acl")
	}
}

//
// 5. Access-log bucket expiry
//

func TestTFA5_AccessLogRetentionVariable(t *testing.T) {
	requireModuleVariable(t, "access_log_retention_days", "number", "180")
	requireExamplePasses(t, "access_log_retention_days")

	module := filesIn(loadTerraform(t, santaAPI), santaAPI)
	vs := blocks(module, "variable", "access_log_retention_days")
	if len(vs) != 1 {
		return
	}
	validations := nested(vs[0], "validation")
	if len(validations) == 0 {
		t.Fatal("variable access_log_retention_days has no validation")
	}
	for days, valid := range map[int64]bool{0: false, -1: false, 1: true, 180: true} {
		ctx := &hcl.EvalContext{Variables: map[string]cty.Value{
			"var": cty.ObjectVal(map[string]cty.Value{"access_log_retention_days": cty.NumberIntVal(days)}),
		}}
		ok := true
		for _, v := range validations {
			cond, has := v.Body.Attributes["condition"]
			if !has {
				continue
			}
			val, diags := cond.Expr.Value(ctx)
			if diags.HasErrors() || !val.Type().Equals(cty.Bool) {
				t.Fatalf("cannot evaluate access_log_retention_days validation %s: %s", sourceOf(t, module, cond.Expr), diags.Error())
			}
			ok = ok && val.True()
		}
		if ok != valid {
			t.Errorf("access_log_retention_days = %d: validation accepts %v, want %v", days, ok, valid)
		}
	}
}

// Both access-log buckets are versioned, so without expiry they keep every log object and every replaced version for
// ever. The Firehose events bucket is deliberately not covered here: its retention is separate work.
func TestTFA5_AccessLogBucketsExpireObjects(t *testing.T) {
	all := loadTerraform(t, santaAPI)
	for _, call := range []string{"firehose", "lambda_source"} {
		mods := blocks(all, "module", call)
		if len(mods) != 1 {
			t.Errorf("module %q not found in the santa_api module", call)
			continue
		}
		src, _ := literal(mods[0].Body.Attributes["source"])
		dir := filepath.Join(filepath.Dir(mods[0].Range().Filename), src)
		arg := ""
		for name, a := range mods[0].Body.Attributes {
			if regexp.MustCompile(`\bvar\.access_log_retention_days\b`).MatchString(sourceOf(t, all, a.Expr)) {
				arg = name
			}
		}
		if arg == "" {
			t.Errorf("module %q is not given var.access_log_retention_days", call)
		}
		sub := filesIn(all, dir)
		if arg != "" && len(blocks(sub, "variable", arg)) != 1 {
			t.Errorf("%s does not declare variable %s", dir, arg)
		}

		buckets := blocks(sub, "resource", "aws_s3_bucket", "s3_logging")
		if len(buckets) != 1 {
			t.Errorf("%s: no aws_s3_bucket.s3_logging", dir)
			continue
		}
		var lifecycle *hclsyntax.Block
		for _, lc := range blocks(sub, "resource", "aws_s3_bucket_lifecycle_configuration") {
			if b, ok := lc.Body.Attributes["bucket"]; ok && strings.Contains(sourceOf(t, sub, b.Expr), "aws_s3_bucket.s3_logging") {
				lifecycle = lc
			}
		}
		if lifecycle == nil {
			t.Errorf("%s: the access-log bucket aws_s3_bucket.s3_logging has no aws_s3_bucket_lifecycle_configuration, so its logs are kept for ever", dir)
			continue
		}
		// The lifecycle must exist exactly when the bucket does (not with existing_logging_bucket_name, which this
		// module does not manage).
		bucketCount := squash(countText(t, sub, buckets[0]))
		lcCount := squash(countText(t, sub, lifecycle))
		if lcCount != bucketCount && !strings.Contains(lcCount, "aws_s3_bucket.s3_logging") {
			t.Errorf("%s: the lifecycle's count %q does not follow the bucket's %q", dir, lcCount, bucketCount)
		}

		days := "var." + arg
		current, noncurrent, multipart := false, false, false
		for _, r := range rulesOf(lifecycle) {
			if s, _ := literal(r.Body.Attributes["status"]); s != "Enabled" {
				continue
			}
			if e := path(r, "expiration"); e != nil {
				if a, ok := e.Body.Attributes["days"]; ok && strings.TrimSpace(sourceOf(t, sub, a.Expr)) == days {
					current = true
				}
			}
			if e := path(r, "noncurrent_version_expiration"); e != nil {
				if a, ok := e.Body.Attributes["noncurrent_days"]; ok && strings.TrimSpace(sourceOf(t, sub, a.Expr)) == days {
					noncurrent = true
				}
			}
			if e := path(r, "abort_incomplete_multipart_upload"); e != nil {
				if _, ok := e.Body.Attributes["days_after_initiation"]; ok {
					multipart = true
				}
			}
		}
		if !current {
			t.Errorf("%s: no enabled lifecycle rule expires current log objects after %s days", dir, days)
		}
		if !noncurrent {
			t.Errorf("%s: no enabled lifecycle rule expires noncurrent versions after %s days (the bucket is versioned)", dir, days)
		}
		if !multipart {
			t.Errorf("%s: no enabled lifecycle rule aborts incomplete multipart uploads", dir)
		}
	}
}

//
// 6. Docs
//

func TestTFA6_DeployDocsCoverTheAudit(t *testing.T) {
	doc := readDoc(t, "docs/deploy.md")

	vars := []string{"enable_api_access_logs", "enable_waf", "access_log_retention_days"}
	if v := wafRateLimitVariable(t); v != "" {
		vars = append(vars, v)
	}
	for _, v := range vars {
		if !regexp.MustCompile("(?m)^\\| `" + regexp.QuoteMeta(v) + "` \\|").MatchString(doc) {
			t.Errorf("docs/deploy.md's Deployment Variables table has no row for `%s`", v)
		}
	}

	start := strings.Index(doc, "## Upgrading an Existing Deployment")
	if start < 0 {
		t.Fatal("docs/deploy.md has no \"Upgrading an Existing Deployment\" section")
	}
	upgrade := doc[start+1:]
	if end := strings.Index(upgrade, "\n## "); end >= 0 {
		upgrade = upgrade[:end]
	}
	heading := regexp.MustCompile(`(?m)^### (\d+)\. `)
	locs := heading.FindAllStringSubmatchIndex(upgrade, -1)
	var numbers []int
	var after22 strings.Builder
	for i, loc := range locs {
		n, _ := strconv.Atoi(upgrade[loc[2]:loc[3]])
		numbers = append(numbers, n)
		end := len(upgrade)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		// Step 22 is the last one before this audit.
		if n > 22 {
			after22.WriteString(upgrade[loc[0]:end])
		}
	}
	for i := 1; i < len(numbers); i++ {
		if numbers[i] != numbers[i-1]+1 {
			t.Errorf("upgrade steps go from %d to %d", numbers[i-1], numbers[i])
		}
	}
	if after22.Len() == 0 {
		t.Fatal("docs/deploy.md has no upgrade step after 22 for the Terraform audit")
	}
	for _, want := range []string{"enable_api_access_logs", "enable_waf", "access_log_retention_days", "use_lockfile", ".terraform.lock.hcl"} {
		if !strings.Contains(after22.String(), want) {
			t.Errorf("the new upgrade step does not mention %s", want)
		}
	}
}
