package deployments

// Data exposure review (2026-11-18, owner decisions 2026-11-19). Events carry user names, hostnames and file paths;
// preflight carries serial numbers, hostnames, primary users and their groups.
//   - S4: the Firehose events bucket must not keep events forever. The events table expires them after 90 days, and
//     the bucket is versioned, so without a lifecycle rule every object, and every overwritten or deleted version,
//     stays readable indefinitely. The owner chose 180 days in total, the default of a santa_api variable passed down
//     to the Firehose module: the current-object expiry plus the noncurrent expiry fit in it (evaluated with the
//     defaults); the rule covers the whole bucket, removes expired delete markers and incomplete multipart uploads.
//   - S5: the Firehose delivery stream encrypts the records it buffers with the module's own key, as the bucket does.
//     The eventupload Lambda may then use that key (GenerateDataKey, Decrypt) for PutRecordBatch, with no
//     kms:ViaService condition, which Firehose's check of its caller may not carry.
//   - S7: the store key lets every principal of the account use it through DynamoDB, so any role with DynamoDB read
//     access (AWS ReadOnlyAccess, say) reads all telemetry. The owner chose an optional restriction: santa_api
//     variable store_kms_key_user_arns, empty by default (the policy is unchanged); when set, "*" may only use the key
//     or create grants when narrowed to Rudolph's own Lambda roles and the listed ARNs.
//     Round 1: the roles are passed as ARNs (lambda_role_arn, the role's .arn); with the list empty the policy renders
//     as before; the list refuses entries that are not IAM principal ARNs or that re-open the key to everyone.
//   - S8: docs/security-risks.md says what a rogue server receives and where each copy of the telemetry is kept, and
//     for how long.
//     With the list set, the docs name the deployer and the key administrators as the exceptions to it.
//     Round 2: those docs also say other accounts' ARNs never match, whole-segment wildcards are refused, and that
//     passing a /rudolph/ role or changing Rudolph's code bypasses the list; deploy.md step 23 says 180 days in total
//     and does not claim Firehose checks the producer's key on every PutRecordBatch.
//
// The key policy is a data source with dynamic blocks, and rendering it needs the AWS provider, which does not run
// offline here, so the S7 checks read the HCL and evaluate its for_each and attributes with go-cty. They accept either
// shape of fix: the "*" statements in a dynamic block that only exists while the variable is empty, or "*"
// statements narrowed by an aws:PrincipalArn condition in a dynamic block that only exists while it is set.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/ext/tryfunc"
	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
	"github.com/zclconf/go-cty/cty/function/stdlib"
)

const (
	firehoseModule   = santaAPI + "/modules/firehose"
	storeModule      = santaAPI + "/modules/store"
	storeKeyUsersVar = "store_kms_key_user_arns"
	// The owner's choice (2026-11-19): twice the events table's 90 days, so the bucket stays the longer record.
	firehoseRetentionDefaultDays = 180
)

var tfVarRef = regexp.MustCompile(`^var\.([A-Za-z0-9_]+)$`)

// The longest an expired event may stay as a noncurrent version. The owner chose 180 days in total, and the bucket is
// versioned: expiring an object only makes it noncurrent, so a noncurrent expiry of another 180 days kept it 360.
const firehoseNoncurrentMaxDays = 7

// firehoseLifecycle returns the enabled rules of the Firehose events bucket's lifecycle configuration, reporting
// when there are none.
func firehoseLifecycle(t *testing.T, report func(format string, args ...any)) ([]tfFile, []*hclsyntax.Block) {
	t.Helper()
	files := loadTerraform(t, firehoseModule)
	var lifecycle *hclsyntax.Block
	for _, b := range blocks(files, "resource", "aws_s3_bucket_lifecycle_configuration") {
		if bucket, ok := b.Body.Attributes["bucket"]; ok && strings.Contains(sourceOf(t, files, bucket.Expr), "aws_s3_bucket.rudolph_eventsupload_firehose.") {
			lifecycle = b
		}
	}
	if lifecycle == nil {
		report("the Firehose events bucket (aws_s3_bucket.rudolph_eventsupload_firehose) has no lifecycle configuration: " +
			"its events, with user names, hostnames and paths, are kept forever while the events table keeps them 90 days")
		return files, nil
	}
	var rules []*hclsyntax.Block
	for _, rule := range nested(lifecycle, "rule") {
		if s, ok := rule.Body.Attributes["status"]; ok {
			if v, _ := literal(s); v == "Enabled" {
				rules = append(rules, rule)
			}
		}
	}
	if len(rules) == 0 {
		report("the Firehose events bucket's lifecycle configuration has no enabled rule")
	}
	return files, rules
}

// firehoseModuleArg returns the source of the argument module "firehose" gives a variable of the Firehose module.
func firehoseModuleArg(t *testing.T, parent []tfFile, name string) (string, bool) {
	t.Helper()
	calls := blocks(parent, "module", "firehose")
	if len(calls) != 1 {
		t.Fatalf("module \"firehose\" called %d times in %s, want once", len(calls), santaAPI)
	}
	arg, ok := calls[0].Body.Attributes[name]
	if !ok {
		return "", false
	}
	return strings.TrimSpace(sourceOf(t, parent, arg.Expr)), true
}

// firehoseExpiry is how the Firehose bucket's lifecycle sets the expiry of current objects: the expression of
// expiration.days, the Firehose module variable it uses and the santa_api variable module "firehose" gives it.
type firehoseExpiry struct {
	files     []tfFile
	rules     []*hclsyntax.Block
	days      hclsyntax.Expression
	moduleVar string
	santaVar  string
}

// firehoseRetention finds the expiry of current objects, reporting what is missing. expiration.days may be any
// expression of one module variable (var.x, var.x - 1, …); its fields are empty when it cannot be found.
func firehoseRetention(t *testing.T, report func(format string, args ...any)) firehoseExpiry {
	t.Helper()
	files, rules := firehoseLifecycle(t, report)
	out := firehoseExpiry{files: files, rules: rules}
	for _, rule := range rules {
		for _, e := range nested(rule, "expiration") {
			if a, ok := e.Body.Attributes["days"]; ok {
				out.days = a.Expr
			}
		}
	}
	if out.days == nil {
		if len(rules) > 0 {
			report("no enabled lifecycle rule expires current objects (expiration { days = … var.… })")
		}
		return out
	}
	vars := map[string]bool{}
	for _, tr := range out.days.Variables() {
		if tr.RootName() == "var" && len(tr) > 1 {
			if a, ok := tr[1].(hcl.TraverseAttr); ok {
				vars[a.Name] = true
			}
		}
	}
	if len(vars) != 1 {
		report("lifecycle expiration days = %s, want an expression of one variable so operators choose the retention",
			squash(sourceOf(t, files, out.days)))
		return out
	}
	for v := range vars {
		out.moduleVar = v
	}
	if len(blocks(files, "variable", out.moduleVar)) != 1 {
		report("%s does not declare variable %q", filepath.Base(firehoseModule), out.moduleVar)
	}
	parent := filesIn(loadTerraform(t, santaAPI), santaAPI)
	src, ok := firehoseModuleArg(t, parent, out.moduleVar)
	if !ok {
		report("module \"firehose\" is not given %s, so the retention cannot be set from the deployment", out.moduleVar)
		return out
	}
	m := tfVarRef.FindStringSubmatch(src)
	if m == nil {
		report("module \"firehose\" %s = %s, want a santa_api variable", out.moduleVar, src)
		return out
	}
	out.santaVar = m[1]
	return out
}

// firehoseRetentionVar returns the santa_api variable that sets the Firehose bucket's retention; "" when it cannot be
// found.
func firehoseRetentionVar(t *testing.T, report func(format string, args ...any)) string {
	t.Helper()
	return firehoseRetention(t, report).santaVar
}

// numberCtx is an evaluation context in which var.<name> is n, with the number functions Terraform has.
func numberCtx(name string, n int) *hcl.EvalContext {
	return &hcl.EvalContext{
		Variables: map[string]cty.Value{"var": cty.ObjectVal(map[string]cty.Value{name: cty.NumberIntVal(int64(n))})},
		Functions: map[string]function.Function{
			"floor": stdlib.FloorFunc,
			"ceil":  stdlib.CeilFunc,
			"abs":   stdlib.AbsoluteFunc,
			"max":   stdlib.MaxFunc,
			"min":   stdlib.MinFunc,
			"can":   tryfunc.CanFunc,
			"try":   tryfunc.TryFunc,
		},
	}
}

// wholeDays evaluates an expression to a whole number of days.
func wholeDays(t *testing.T, files []tfFile, expr hclsyntax.Expression, ctx *hcl.EvalContext) (int, bool) {
	t.Helper()
	v, diags := expr.Value(ctx)
	if diags.HasErrors() || !v.IsKnown() || v.IsNull() || v.Type() != cty.Number {
		t.Errorf("cannot evaluate %s to a number of days: %s", squash(sourceOf(t, files, expr)), diags.Error())
		return 0, false
	}
	bf := v.AsBigFloat()
	if !bf.IsInt() {
		t.Errorf("%s is not a whole number of days", squash(sourceOf(t, files, expr)))
		return 0, false
	}
	i, _ := bf.Int64()
	return int(i), true
}

// S4: the owner chose 180 days in total. The bucket is versioned, so expiring an object only makes it noncurrent:
// the current-object expiry plus the noncurrent expiry is how long an event stays, and must fit in the retention.
func TestDataExposure_FirehoseEventsBucketExpiresEvents(t *testing.T) {
	parent := filesIn(loadTerraform(t, santaAPI), santaAPI)
	exp := firehoseRetention(t, t.Errorf)
	retention := 0
	if exp.santaVar != "" {
		def, ok := variableDefault(t, parent, exp.santaVar)
		if !ok {
			t.Errorf("santa_api variable %s has no default; the owner chose a default of %d days", exp.santaVar, firehoseRetentionDefaultDays)
		} else if days, err := strconv.Atoi(def); err != nil || days != firehoseRetentionDefaultDays {
			t.Errorf("santa_api variable %s defaults to %s, want %d (days)", exp.santaVar, def, firehoseRetentionDefaultDays)
		} else {
			retention = days
		}
	}
	files, rules := exp.files, exp.rules
	if len(rules) == 0 {
		return
	}

	// The module variable: never null, and at least 2 days, so that an expiry of a day less still leaves a day.
	if exp.moduleVar != "" {
		if vs := blocks(files, "variable", exp.moduleVar); len(vs) == 1 {
			v := vs[0]
			if a, ok := v.Body.Attributes["nullable"]; !ok || strings.TrimSpace(sourceOf(t, files, a.Expr)) != "false" {
				t.Errorf("Firehose module variable %s: want nullable = false", exp.moduleVar)
			}
			accepts := func(n int) bool {
				for _, val := range nested(v, "validation") {
					c, ok := val.Body.Attributes["condition"]
					if !ok {
						continue
					}
					r, diags := c.Expr.Value(numberCtx(exp.moduleVar, n))
					if diags.HasErrors() || !r.IsKnown() || r.Type() != cty.Bool {
						t.Errorf("cannot evaluate the validation of %s for %d: %s", exp.moduleVar, n, diags.Error())
						return false
					}
					if r.False() {
						return false
					}
				}
				return true
			}
			if accepts(1) || accepts(0) {
				t.Errorf("Firehose module variable %s accepts 1 day: want a validation of at least 2, so the current-object expiry "+
					"and the noncurrent expiry both get a day within the retention", exp.moduleVar)
			}
			for _, n := range []int{2, 30, firehoseRetentionDefaultDays} {
				if !accepts(n) {
					t.Errorf("Firehose module variable %s refuses %d days", exp.moduleVar, n)
				}
			}
		}
	}

	// Current objects plus noncurrent versions within the retention, with the default and with another value.
	var noncurrent []hclsyntax.Expression
	for _, rule := range rules {
		for _, e := range nested(rule, "noncurrent_version_expiration") {
			if a, ok := e.Body.Attributes["noncurrent_days"]; ok {
				noncurrent = append(noncurrent, a.Expr)
			} else {
				t.Error("lifecycle noncurrent_version_expiration: no noncurrent_days")
			}
		}
	}
	if len(noncurrent) == 0 {
		t.Error("no enabled lifecycle rule expires noncurrent versions (noncurrent_version_expiration); " +
			"the bucket is versioned, so expired or overwritten events would otherwise survive as old versions")
	}
	if exp.days != nil && exp.moduleVar != "" && retention > 0 {
		for _, r := range []int{retention, 30} {
			ctx := numberCtx(exp.moduleVar, r)
			current, ok := wholeDays(t, files, exp.days, ctx)
			if !ok {
				continue
			}
			if current < 1 {
				t.Errorf("with a retention of %d days, current objects expire after %d days", r, current)
			}
			for _, n := range noncurrent {
				nc, ok := wholeDays(t, files, n, ctx)
				if !ok {
					continue
				}
				if nc < 1 || nc > firehoseNoncurrentMaxDays {
					t.Errorf("with a retention of %d days, noncurrent versions expire after %d days, want 1 to %d", r, nc, firehoseNoncurrentMaxDays)
				}
				if current+nc > r {
					t.Errorf("with a retention of %d days, an event stays %d days as the current object (expiration days = %s) and %d more "+
						"as a noncurrent version: %d days in total, want at most %d (the owner chose the retention as the total, "+
						"e.g. days = var.%s - 1 and noncurrent_days = 1)", r, current, squash(sourceOf(t, files, exp.days)), nc, current+nc, r, exp.moduleVar)
				}
			}
		}
	}

	// Delete markers left once every version of an object has expired: removed by their own expiration block.
	markers := false
	for _, rule := range rules {
		for _, e := range nested(rule, "expiration") {
			if a, ok := e.Body.Attributes["expired_object_delete_marker"]; ok {
				if v, diags := a.Expr.Value(nil); !diags.HasErrors() && v.Type() == cty.Bool && v.True() {
					markers = true
				}
			}
		}
	}
	if !markers {
		t.Error("no enabled lifecycle rule has expiration { expired_object_delete_marker = true }: the delete marker of every " +
			"expired event stays in the bucket for ever (it cannot share an expiration block with days, so it needs its own rule)")
	}

	// The whole bucket: an empty filter, no prefix or tag that leaves objects out.
	for _, rule := range rules {
		filters := nested(rule, "filter")
		if len(filters) != 1 {
			t.Errorf("lifecycle rule has %d filter blocks, want one empty filter {} so it covers every object of the bucket", len(filters))
			continue
		}
		if len(filters[0].Body.Attributes) > 0 || len(filters[0].Body.Blocks) > 0 {
			t.Errorf("lifecycle rule filter is %s, want filter {} so it covers every object of the bucket (no prefix or tag)",
				squash(blockText(t, files, filters[0])))
		}
	}

	// Parts of uploads Firehose never completed hold events too and do not expire with the objects.
	abort := false
	for _, rule := range rules {
		for _, a := range nested(rule, "abort_incomplete_multipart_upload") {
			if _, ok := a.Body.Attributes["days_after_initiation"]; ok {
				abort = true
			}
		}
	}
	if !abort {
		t.Error("no enabled lifecycle rule has abort_incomplete_multipart_upload { days_after_initiation = … }: " +
			"parts of uploads Firehose never completed hold events, are invisible to listings and never expire")
	}
}

// S5
func TestDataExposure_FirehoseStreamEncryptsBufferedRecords(t *testing.T) {
	files := loadTerraform(t, firehoseModule)
	streams := blocks(files, "resource", "aws_kinesis_firehose_delivery_stream", "eventsupload_firehose")
	if len(streams) != 1 {
		t.Fatalf("found %d aws_kinesis_firehose_delivery_stream.eventsupload_firehose, want 1", len(streams))
	}
	sse := nested(streams[0], "server_side_encryption")
	if len(sse) != 1 {
		t.Fatal("the Firehose delivery stream has no server_side_encryption block: records it buffers before delivery " +
			"(user names, hostnames, paths) are not under the module's key")
	}
	attr := func(name string) string {
		a, ok := sse[0].Body.Attributes[name]
		if !ok {
			return ""
		}
		return strings.TrimSpace(sourceOf(t, files, a.Expr))
	}
	if got := attr("enabled"); got != "true" {
		t.Errorf("server_side_encryption enabled = %q, want true", got)
	}
	if got := attr("key_type"); got != `"CUSTOMER_MANAGED_CMK"` {
		t.Errorf("server_side_encryption key_type = %s, want \"CUSTOMER_MANAGED_CMK\" (the module's own key, as on the bucket)", got)
	}
	if got := attr("key_arn"); !strings.Contains(got, "aws_kms_key.rudolph_eventsupload_kms_key.arn") {
		t.Errorf("server_side_encryption key_arn = %q, want aws_kms_key.rudolph_eventsupload_kms_key.arn", got)
	}
}

// conditionsOf returns the conditions of a statement, static or in dynamic "condition" blocks, with the for_each of
// the dynamic block ("" for a static one).
func conditionsOf(t *testing.T, files []tfFile, st *hclsyntax.Block) (conds []*hclsyntax.Block, forEach []string) {
	for _, c := range nested(st, "condition") {
		conds, forEach = append(conds, c), append(forEach, "")
	}
	for _, d := range nested(st, "dynamic") {
		if len(d.Labels) != 1 || d.Labels[0] != "condition" {
			continue
		}
		fe := ""
		if a, ok := d.Body.Attributes["for_each"]; ok {
			fe = sourceOf(t, files, a.Expr)
		}
		for _, c := range nested(d, "content") {
			conds, forEach = append(conds, c), append(forEach, fe)
		}
	}
	return conds, forEach
}

var (
	policyDocRef = regexp.MustCompile(`data\.aws_iam_policy_document\.([A-Za-z0-9_]+)\b`)
	iamPolicyRef = regexp.MustCompile(`aws_iam_policy\.([A-Za-z0-9_]+)\b`)
)

// producerPolicyDocs returns the policy documents of the Firehose module that reach the roles in
// var.firehose_upload_lambda_role_names (the eventupload Lambda), through an attached aws_iam_policy or an inline
// aws_iam_role_policy.
func producerPolicyDocs(t *testing.T, files []tfFile) map[string]bool {
	t.Helper()
	docs := map[string]bool{}
	policies := map[string]string{} // aws_iam_policy name -> its policy source
	for _, p := range blocks(files, "resource", "aws_iam_policy") {
		if a, ok := p.Body.Attributes["policy"]; ok {
			policies[p.Labels[1]] = sourceOf(t, files, a.Expr)
		}
	}
	for _, typ := range []string{"aws_iam_role_policy_attachment", "aws_iam_role_policy", "aws_iam_policy_attachment"} {
		for _, r := range blocks(files, "resource", typ) {
			text := blockText(t, files, r)
			if !strings.Contains(text, "var.firehose_upload_lambda_role_names") {
				continue
			}
			for _, m := range policyDocRef.FindAllStringSubmatch(text, -1) {
				docs[m[1]] = true
			}
			for _, m := range iamPolicyRef.FindAllStringSubmatch(text, -1) {
				for _, d := range policyDocRef.FindAllStringSubmatch(policies[m[1]], -1) {
					docs[d[1]] = true
				}
			}
		}
	}
	return docs
}

// S5: PutRecordBatch on a stream encrypted with a customer managed key needs the caller to be allowed
// kms:GenerateDataKey and kms:Decrypt on that key. No kms:ViaService condition: AWS does not document that the
// check Firehose makes of its caller carries it, and if it does not, every upload is refused.
func TestDataExposure_EventuploadMayUseTheStreamKey(t *testing.T) {
	docs, files := policyDocs(t, firehoseModule)
	names := producerPolicyDocs(t, files)
	if len(names) == 0 {
		t.Fatal("no policy document of the Firehose module is attached to var.firehose_upload_lambda_role_names; the check is out of date")
	}
	found := false
	for name := range names {
		doc, ok := docs[name]
		if !ok {
			continue
		}
		for _, st := range statementsOf(doc) {
			text := squash(blockText(t, files, st))
			if e, ok := st.Body.Attributes["effect"]; ok {
				if v, _ := literal(e); v == "Deny" {
					continue
				}
			}
			res, ok := st.Body.Attributes["resources"]
			if !ok || !strings.Contains(sourceOf(t, files, res.Expr), "aws_kms_key.rudolph_eventsupload_kms_key.arn") {
				continue
			}
			if !strings.Contains(text, `"kms:Decrypt"`) || !regexp.MustCompile(`"kms:GenerateDataKey\*?"`).MatchString(text) {
				continue
			}
			found = true
			conds, _ := conditionsOf(t, files, st)
			for _, c := range conds {
				if v, ok := c.Body.Attributes["variable"]; ok {
					if cv, _ := literal(v); strings.EqualFold(cv, "kms:ViaService") {
						t.Errorf("policy document %s: the statement allowing kms:GenerateDataKey and kms:Decrypt on the stream key has a "+
							"kms:ViaService condition; AWS does not document that Firehose's check of the PutRecordBatch caller carries it, "+
							"and if it does not, every upload is refused", name)
					}
				}
			}
		}
	}
	if !found {
		t.Error("the eventupload Lambda's Firehose policy does not allow kms:GenerateDataKey and kms:Decrypt on " +
			"aws_kms_key.rudolph_eventsupload_kms_key.arn: with the stream encrypted under that key, Firehose refuses its PutRecordBatch")
	}
}

// tableRoles are the roles of every Lambda that reads or writes the store or events table, as santa_api refers to
// them. ARNs, never names: aws:PrincipalArn and key policy principals compare ARNs, so a role name never matches,
// and an ARN built from a name as role/<name> leaves out the role's path (the ranker's is /rudolph/). The
// api-handler module's lambda_role_arn output must be its role's .arn (TestDataExposure_StoreKeyCanBeLimitedToNamedPrincipals).
var tableRoles = map[string]*regexp.Regexp{
	"health":       regexp.MustCompile(`module\.health_function\.lambda_role_arn\b`),
	"preflight":    regexp.MustCompile(`module\.preflight_function\.lambda_role_arn\b`),
	"ruledownload": regexp.MustCompile(`module\.ruledownload_function\.lambda_role_arn\b`),
	"postflight":   regexp.MustCompile(`module\.postflight_function\.lambda_role_arn\b`),
	"eventupload":  regexp.MustCompile(`module\.eventupload_function\.lambda_role_arn\b`),
	"ranker":       regexp.MustCompile(`aws_iam_role\.ranker\.arn\b`),
}

var localRef = regexp.MustCompile(`\blocal\.([A-Za-z0-9_]+)`)

// expandLocals replaces local.x references in text with the source of the local, a few levels deep.
func expandLocals(t *testing.T, files []tfFile, text string) string {
	t.Helper()
	defs := map[string]string{}
	for _, l := range blocks(files, "locals") {
		for name, a := range l.Body.Attributes {
			defs[name] = sourceOf(t, files, a.Expr)
		}
	}
	for i := 0; i < 5; i++ {
		text = localRef.ReplaceAllStringFunc(text, func(ref string) string {
			if d, ok := defs[localRef.FindStringSubmatch(ref)[1]]; ok {
				return "(" + d + ")"
			}
			return ref
		})
	}
	return stripTFComments(text)
}

// Every Lambda given a table name must be in tableRoles, so a new one cannot be locked out by the restriction
// unnoticed.
func checkTableRolesComplete(t *testing.T, parent []tfFile) {
	t.Helper()
	for _, m := range blocks(parent, "module") {
		if !strings.Contains(blockText(t, parent, m), "DYNAMODB_NAME") {
			continue
		}
		if _, ok := tableRoles[strings.TrimSuffix(m.Labels[0], "_function")]; !ok {
			t.Errorf("module %q is given a table name but its role is not in tableRoles; the check is out of date", m.Labels[0])
		}
	}
	for _, f := range blocks(parent, "resource", "aws_lambda_function") {
		if !strings.Contains(blockText(t, parent, f), "DYNAMODB_NAME") {
			continue
		}
		role, ok := f.Body.Attributes["role"]
		if !ok || !tableRoles["ranker"].MatchString(sourceOf(t, parent, role.Expr)) {
			t.Errorf("aws_lambda_function.%s is given a table name but its role is not in tableRoles; the check is out of date", f.Labels[1])
		}
	}
}

var (
	kmsUse   = regexp.MustCompile(`"kms:(Decrypt|Encrypt|GenerateDataKey[^"]*|ReEncrypt[^"]*|CreateGrant|\*)"`)
	varNames = regexp.MustCompile(`\bvar\.([A-Za-z0-9_]+)`)
)

// keyStatement is one statement of the store key policy, with the for_each of its dynamic block if it has one.
type keyStatement struct {
	body    *hclsyntax.Block
	forEach string
}

func keyStatements(t *testing.T, files []tfFile, doc *hclsyntax.Block) []keyStatement {
	var out []keyStatement
	for _, s := range nested(doc, "statement") {
		out = append(out, keyStatement{body: s})
	}
	for _, d := range nested(doc, "dynamic") {
		if len(d.Labels) != 1 || d.Labels[0] != "statement" {
			continue
		}
		fe := ""
		if a, ok := d.Body.Attributes["for_each"]; ok {
			fe = expandLocals(t, files, sourceOf(t, files, a.Expr))
		}
		for _, c := range nested(d, "content") {
			out = append(out, keyStatement{body: c, forEach: fe})
		}
	}
	return out
}

// principalArnValues returns the values of every aws:PrincipalArn condition of a statement, static or dynamic.
func principalArnValues(t *testing.T, files []tfFile, st *hclsyntax.Block) []string {
	conditions := nested(st, "condition")
	for _, d := range nested(st, "dynamic") {
		if len(d.Labels) == 1 && d.Labels[0] == "condition" {
			conditions = append(conditions, nested(d, "content")...)
		}
	}
	var out []string
	for _, c := range conditions {
		v, ok := c.Body.Attributes["variable"]
		if !ok {
			continue
		}
		if name, _ := literal(v); !strings.EqualFold(name, "aws:PrincipalArn") {
			continue
		}
		if vals, ok := c.Body.Attributes["values"]; ok {
			out = append(out, expandLocals(t, files, sourceOf(t, files, vals.Expr)))
		}
	}
	return out
}

// awsIdentifiers returns the identifiers of the AWS principals of a statement.
func awsIdentifiers(t *testing.T, files []tfFile, st *hclsyntax.Block) []string {
	var out []string
	for _, p := range nested(st, "principals") {
		if typ, ok := p.Body.Attributes["type"]; ok {
			if v, _ := literal(typ); v != "AWS" {
				continue
			}
		}
		if ids, ok := p.Body.Attributes["identifiers"]; ok {
			out = append(out, expandLocals(t, files, sourceOf(t, files, ids.Expr)))
		}
	}
	return out
}

// S7
func TestDataExposure_StoreKeyCanBeLimitedToNamedPrincipals(t *testing.T) {
	parent := filesIn(loadTerraform(t, santaAPI), santaAPI)
	checkTableRolesComplete(t, parent)

	// The santa_api variable: a list of ARNs, empty by default so existing deployments keep today's policy.
	if vs := blocks(parent, "variable", storeKeyUsersVar); len(vs) != 1 {
		t.Errorf("santa_api declares no variable %q (IAM principal ARNs that may use the store key besides Rudolph's "+
			"Lambdas; empty keeps today's policy, where every principal of the account may use it through DynamoDB)", storeKeyUsersVar)
	} else {
		if typ, ok := vs[0].Body.Attributes["type"]; !ok || squash(sourceOf(t, parent, typ.Expr)) != "list(string)" {
			t.Errorf("variable %s: want type = list(string)", storeKeyUsersVar)
		}
		if def, ok := variableDefault(t, parent, storeKeyUsersVar); !ok || squash(def) != "[]" {
			t.Errorf("variable %s: default = %q, want [] so the restriction is optional", storeKeyUsersVar, def)
		}
	}

	// What each argument of module "rule_store" carries: the users variable, and which Lambda roles.
	calls := blocks(parent, "module", "rule_store")
	if len(calls) != 1 {
		t.Fatalf("module \"rule_store\" called %d times, want once", len(calls))
	}
	carriesUsers := map[string]bool{}
	carriesRoles := map[string]map[string]bool{}
	for name, a := range calls[0].Body.Attributes {
		text := expandLocals(t, parent, sourceOf(t, parent, a.Expr))
		if strings.Contains(text, "var."+storeKeyUsersVar) {
			carriesUsers[name] = true
		}
		for role, re := range tableRoles {
			if re.MatchString(text) {
				if carriesRoles[name] == nil {
					carriesRoles[name] = map[string]bool{}
				}
				carriesRoles[name][role] = true
			}
		}
	}
	if len(carriesUsers) == 0 {
		t.Errorf("module \"rule_store\" is not given var.%s", storeKeyUsersVar)
	}
	passed := map[string]bool{}
	for _, roles := range carriesRoles {
		for r := range roles {
			passed[r] = true
		}
	}
	for role := range tableRoles {
		if !passed[role] {
			t.Errorf("module \"rule_store\" is not given the %s Lambda's role ARN (%s); it reads or writes the tables, so the key must keep allowing it", role, tableRoles[role])
		}
	}
	// The api-handler roles are passed as lambda_role_arn: it must be the role's own ARN, path included.
	apiHandler := santaAPI + "/modules/lambda/api-handler"
	apiFiles := loadTerraform(t, apiHandler)
	if outs := blocks(apiFiles, "output", "lambda_role_arn"); len(outs) != 1 {
		t.Errorf("%s has no output lambda_role_arn; the store key needs the Lambda roles' ARNs, which their names never match", apiHandler)
	} else if a, ok := outs[0].Body.Attributes["value"]; !ok || strings.TrimSpace(sourceOf(t, apiFiles, a.Expr)) != "aws_iam_role.api_handler_role.arn" {
		t.Errorf("%s output lambda_role_arn is not aws_iam_role.api_handler_role.arn", apiHandler)
	}

	// covers reports whether a principal expression names the users variable and every Lambda role, through the
	// store module's variables.
	covers := func(text string) (users bool, missing []string) {
		got := map[string]bool{}
		for _, m := range varNames.FindAllStringSubmatch(text, -1) {
			if carriesUsers[m[1]] {
				users = true
			}
			for r := range carriesRoles[m[1]] {
				got[r] = true
			}
		}
		for role := range tableRoles {
			if !got[role] {
				missing = append(missing, role)
			}
		}
		sort.Strings(missing)
		return users, missing
	}

	docs, files := policyDocs(t, storeModule)
	doc, ok := docs["store_sse_permissions"]
	if !ok {
		t.Fatal("no aws_iam_policy_document.store_sse_permissions in the store module; the check is out of date")
	}
	restrictedFor := map[string]bool{} // actions granted to the Lambdas and the listed ARNs, through DynamoDB only
	var everyoneUse, everyoneGrant bool
	for _, st := range keyStatements(t, files, doc) {
		text := squash(blockText(t, files, st.body))
		if !kmsUse.MatchString(text) {
			continue
		}
		ids := awsIdentifiers(t, files, st.body)
		everyone := false
		for _, id := range ids {
			if strings.Contains(id, `"*"`) {
				everyone = true
			}
		}
		var narrowedBy []string
		if everyone {
			narrowedBy = principalArnValues(t, files, st.body)
			onlyWhileEmpty := false
			for _, m := range varNames.FindAllStringSubmatch(st.forEach, -1) {
				if carriesUsers[m[1]] {
					onlyWhileEmpty = true
				}
			}
			viaDynamoDB := strings.Contains(text, "kms:ViaService")
			if viaDynamoDB && strings.Contains(text, `"kms:Decrypt"`) {
				everyoneUse = true
			}
			if viaDynamoDB && strings.Contains(text, `"kms:CreateGrant"`) && strings.Contains(text, "kms:GrantIsForAWSResource") {
				everyoneGrant = true
			}
			if len(narrowedBy) == 0 && !onlyWhileEmpty {
				sid := ""
				if a, ok := st.body.Body.Attributes["sid"]; ok {
					sid, _ = literal(a)
				}
				t.Errorf("store key statement %q lets every principal of the account (\"*\") %s, whatever %s is: "+
					"it needs an aws:PrincipalArn condition naming Rudolph's Lambda roles and the listed ARNs, or a dynamic "+
					"block that only exists while the list is empty", sid, strings.Join(kmsUse.FindAllString(text, -1), ", "), storeKeyUsersVar)
				continue
			}
		} else {
			narrowedBy = ids
		}
		if !strings.Contains(text, "kms:ViaService") {
			continue // the existing checks cover direct use; here only use through DynamoDB counts
		}
		for _, n := range narrowedBy {
			users, missing := covers(n)
			if !users {
				continue
			}
			if len(missing) > 0 {
				t.Errorf("a store key statement for %s leaves out the Lambda roles %v, so they could no longer read the tables", storeKeyUsersVar, missing)
				continue
			}
			for _, a := range []string{"kms:Decrypt", "kms:GenerateDataKey"} {
				if strings.Contains(text, `"`+a) {
					restrictedFor[a] = true
				}
			}
		}
	}
	for _, a := range []string{"kms:Decrypt", "kms:GenerateDataKey"} {
		if !restrictedFor[a] {
			t.Errorf("no store key statement grants %s through DynamoDB (kms:ViaService) to Rudolph's Lambda roles and %s together; "+
				"with the list set, the Lambdas and the listed principals must keep reading and writing the tables", a, storeKeyUsersVar)
		}
	}
	// With the list empty the policy is today's: "*" may use the key through DynamoDB and create grants for AWS
	// resources. Without these, the restriction would have removed rather than added an option.
	if !everyoneUse {
		t.Error("no store key statement lets \"*\" use the key through DynamoDB any more; with the list empty the policy must stay as it was")
	}
	if !everyoneGrant {
		t.Error("no store key statement lets \"*\" create grants for AWS resources through DynamoDB any more; with the list empty the policy must stay as it was")
	}
}

// docSection returns the body of the "## " section of a Markdown document whose heading matches re.
func docSection(doc string, re *regexp.Regexp) (heading, body string, ok bool) {
	parts := regexp.MustCompile(`(?m)^## `).Split(doc, -1)
	for _, p := range parts[1:] {
		h, b, _ := strings.Cut(p, "\n")
		if re.MatchString(h) {
			return h, b, true
		}
	}
	return "", "", false
}

// lineWith reports whether one line (a list item, a table row or a paragraph) matches every pattern.
func lineWith(body string, patterns ...string) bool {
	for _, line := range strings.Split(body, "\n") {
		all := true
		for _, p := range patterns {
			if !regexp.MustCompile(`(?i)` + p).MatchString(line) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// S8
func TestDataExposure_DocsDescribeTelemetryAndRetention(t *testing.T) {
	src, err := os.ReadFile("../docs/security-risks.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(src)

	_, rogue, ok := docSection(doc, regexp.MustCompile(`Trusting the Wrong Server`))
	if !ok {
		t.Fatal(`docs/security-risks.md has no "Trusting the Wrong Server" section`)
	}
	if regexp.MustCompile(`(?i)basic information`).MatchString(rogue) {
		t.Error(`"Trusting the Wrong Server" still calls what a rogue server receives "basic information"`)
	}
	for what, re := range map[string]string{
		"preflight":                         `preflight`,
		"the serial number":                 `serial`,
		"the hostname":                      `host ?name`,
		"the primary user":                  `primary user`,
		"the primary user's groups":         `groups`,
		"eventupload":                       `eventupload|event upload`,
		"user names in events":              `user ?names?`,
		"file paths in events":              `paths`,
		"what each step sends, by endpoint": `(?s)preflight.*(serial|host ?name).*(eventupload|event upload).*paths|(?s)(eventupload|event upload).*paths.*preflight.*(serial|host ?name)`,
	} {
		if !regexp.MustCompile(`(?i)` + re).MatchString(rogue) {
			t.Errorf(`"Trusting the Wrong Server" does not say what a rogue server receives: %s`, what)
		}
	}

	heading, inventory, ok := docSection(doc, regexp.MustCompile(`(?i)telemetry|retention`))
	if !ok {
		t.Fatal(`docs/security-risks.md has no section (a "## " heading with "telemetry" or "retention") listing where telemetry is copied and how long each copy is kept`)
	}
	firehose := []string{`firehose`, `180 days`}
	if current := firehoseRetentionVar(t, func(string, ...any) {}); current != "" {
		firehose = append(firehose, regexp.QuoteMeta(current))
	}
	for what, patterns := range map[string][]string{
		"the store table and its TTL":                                    {`store table`, `TTL|expire|ExpiresAfter`},
		"the events table, kept 90 days":                                 {`events table`, `90 days`},
		"point-in-time recovery, 35 days":                                {`point.in.time recovery`, `35 days`},
		"the Firehose bucket, 180 days, and its variable":                firehose,
		"the Kinesis stream: operator-owned, its own encryption":         {`kinesis`, `operator|your own|you own`, `encrypt`},
		"the Kinesis stream: its own retention":                          {`kinesis`, `retention|retain|kept|keep`},
		"the output Lambda: operator-owned, its own encryption":          {`output lambda|eventupload_output_lambda_name`, `operator|your own|you own`, `encrypt`},
		"the output Lambda: its own retention":                           {`output lambda|eventupload_output_lambda_name`, `retention|retain|kept|keep`},
		"S7: who can read the store tables through the key":              {`ReadOnlyAccess|DynamoDB read`},
		"S7: the variable that limits the store key to named principals": {regexp.QuoteMeta(storeKeyUsersVar)},
	} {
		if !lineWith(inventory, patterns...) {
			t.Errorf("section %q does not list %s (want one line or table row matching %q)", strings.TrimSpace(heading), what, patterns)
		}
	}
}

// S8, round 1: with store_kms_key_user_arns set, two kinds of principal outside the list still matter. The deployer:
// creating or replacing a table and restoring one from point-in-time recovery make DynamoDB create grants as the
// deployer, so it must be listed or given the key through IAM. Key administrators: they can change the key policy or
// create grants, so they can reach the data, an exception besides IAM policies through the root statement.
func TestDataExposure_DocsNameTheStoreKeyExceptions(t *testing.T) {
	var where []string
	var texts []string
	if src, err := os.ReadFile("../docs/security-risks.md"); err != nil {
		t.Fatal(err)
	} else if h, body, ok := docSection(string(src), regexp.MustCompile(`(?i)telemetry|retention`)); ok {
		where, texts = append(where, "security-risks.md "+strings.TrimSpace(h)), append(texts, body)
	}
	if src, err := os.ReadFile("../docs/deploy.md"); err != nil {
		t.Fatal(err)
	} else {
		for _, p := range regexp.MustCompile(`(?m)^### `).Split(string(src), -1)[1:] {
			h, body, _ := strings.Cut(p, "\n")
			if strings.HasPrefix(h, "23.") {
				body, _, _ = strings.Cut(body, "\n## ")
				where, texts = append(where, "deploy.md "+strings.TrimSpace(h)), append(texts, body)
			}
		}
	}
	if len(texts) == 0 {
		t.Fatal(`neither docs/security-risks.md (a "## " section on telemetry or retention) nor docs/deploy.md (upgrade step 23) has a section to check`)
	}
	users := regexp.QuoteMeta(storeKeyUsersVar)
	for what, patterns := range map[string][]string{
		"the deployer (the role that runs Terraform) must be listed or given the key through IAM, as table creation, " +
			"replacement and point-in-time restore run DynamoDB's CreateGrant as the deployer": {
			users, `deployer|runs Terraform|Terraform role|role that runs Terraform`, `CreateGrant`,
			`table creation|creat(e|es|ing) (a |the )?(new )?tables?|replac`, `restor`, `\blist|IAM`,
		},
		"key administrators (kms_key_administrators_arns) can reach the data by changing the key policy or creating grants": {
			users, `kms_key_administrators_arns`, `polic`, `grant`,
		},
		// Review round 2
		"ARNs of another account never match": {
			users, `(another|other|different) account|cross.account`, `never match|(do|does|cannot|can.t|don.t) (not )?match`,
		},
		"a wildcard covering a whole path segment is refused": {
			users, `wildcard|\*`, `(whole|entire|full) (path )?segment|path segment`, `refus|reject`,
		},
		"whoever can pass a /rudolph/ role to a Lambda, or change Rudolph's function code, runs as a listed role": {
			`/rudolph/`, `PassRole|\bpass`, `code`, `Lambda`,
		},
	} {
		found := false
		for _, text := range texts {
			if lineWith(text, patterns...) {
				found = true
			}
		}
		if !found {
			t.Errorf("%v: no line or table row says that %s (want one matching %q)", where, what, patterns)
		}
	}
}

// storeKeyArgs returns the store module variables that module "rule_store" fills with store_kms_key_user_arns, and
// those it fills with Lambda role ARNs.
func storeKeyArgs(t *testing.T, parent []tfFile) (users, roles map[string]bool) {
	t.Helper()
	users, roles = map[string]bool{}, map[string]bool{}
	calls := blocks(parent, "module", "rule_store")
	if len(calls) != 1 {
		t.Fatalf("module \"rule_store\" called %d times, want once", len(calls))
	}
	for name, a := range calls[0].Body.Attributes {
		text := expandLocals(t, parent, sourceOf(t, parent, a.Expr))
		if strings.Contains(text, "var."+storeKeyUsersVar) {
			users[name] = true
		}
		for _, re := range tableRoles {
			if re.MatchString(text) {
				roles[name] = true
			}
		}
	}
	return users, roles
}

const (
	testAccount   = "123456789012"
	testUserARN   = "arn:aws:iam::123456789012:role/rudolph-web"
	testAdminARN  = "arn:aws:iam::123456789012:role/key-admin"
	testLambdaARN = "arn:aws:iam::123456789012:role/rudolph/test_rudolph_preflight_role"
)

// storeKeyEvalContext gives every variable of the store module a value: the users list, the administrators list and
// the Lambda role ARNs as asked, the others their literal default or a placeholder, and evaluates its locals.
func storeKeyEvalContext(t *testing.T, files []tfFile, users, roles map[string]bool, userARNs, adminARNs []string) *hcl.EvalContext {
	t.Helper()
	list := func(ss []string) cty.Value {
		if len(ss) == 0 {
			return cty.ListValEmpty(cty.String)
		}
		var vs []cty.Value
		for _, s := range ss {
			vs = append(vs, cty.StringVal(s))
		}
		return cty.ListVal(vs)
	}
	vars := map[string]cty.Value{}
	for _, v := range blocks(files, "variable") {
		name := v.Labels[0]
		switch {
		case users[name]:
			vars[name] = list(userARNs)
		case name == "kms_key_administrators_arns":
			vars[name] = list(adminARNs)
		case roles[name]:
			vars[name] = list([]string{testLambdaARN})
		case name == "aws_account_id":
			vars[name] = cty.StringVal(testAccount)
		case name == "region":
			vars[name] = cty.StringVal("ap-southeast-2")
		default:
			if d, ok := v.Body.Attributes["default"]; ok {
				if val, diags := d.Expr.Value(nil); !diags.HasErrors() {
					vars[name] = val
					continue
				}
			}
			vars[name] = cty.StringVal("test")
		}
	}
	ctx := &hcl.EvalContext{
		Variables: map[string]cty.Value{"var": cty.ObjectVal(vars)},
		Functions: map[string]function.Function{
			"length":       stdlib.LengthFunc,
			"concat":       stdlib.ConcatFunc,
			"distinct":     stdlib.DistinctFunc,
			"compact":      stdlib.CompactFunc,
			"flatten":      stdlib.FlattenFunc,
			"coalescelist": stdlib.CoalesceListFunc,
			"contains":     stdlib.ContainsFunc,
			"format":       stdlib.FormatFunc,
			"join":         stdlib.JoinFunc,
			"toset":        stdlib.MakeToFunc(cty.Set(cty.DynamicPseudoType)),
			"tolist":       stdlib.MakeToFunc(cty.List(cty.DynamicPseudoType)),
		},
	}
	locals := map[string]cty.Value{}
	for pass := 0; pass < 3; pass++ {
		ctx.Variables["local"] = cty.ObjectVal(locals)
		for _, l := range blocks(files, "locals") {
			for name, a := range l.Body.Attributes {
				if val, diags := a.Expr.Value(ctx); !diags.HasErrors() {
					locals[name] = val
				}
			}
		}
	}
	ctx.Variables["local"] = cty.ObjectVal(locals)
	return ctx
}

// exists reports whether a dynamic block with this for_each renders any block; a static block (nil) always does.
func exists(t *testing.T, files []tfFile, forEach hclsyntax.Expression, ctx *hcl.EvalContext) bool {
	t.Helper()
	if forEach == nil {
		return true
	}
	v, diags := forEach.Value(ctx)
	if diags.HasErrors() || !v.IsKnown() || v.IsNull() {
		t.Errorf("cannot evaluate for_each = %s without AWS: %s", squash(sourceOf(t, files, forEach)), diags.Error())
		return false
	}
	return v.LengthInt() > 0
}

// renderValue evaluates an attribute, sorting lists of strings (their order means nothing in a policy); its source
// when it cannot be evaluated.
func renderValue(t *testing.T, files []tfFile, b *hclsyntax.Block, name string, ctx *hcl.EvalContext) string {
	a, ok := b.Body.Attributes[name]
	if !ok {
		return ""
	}
	v, diags := a.Expr.Value(ctx)
	if diags.HasErrors() || !v.IsWhollyKnown() || v.IsNull() {
		return "src:" + squash(sourceOf(t, files, a.Expr))
	}
	if v.Type() == cty.String {
		return v.AsString()
	}
	if v.CanIterateElements() {
		var ss []string
		for it := v.ElementIterator(); it.Next(); {
			_, e := it.Element()
			if e.Type() == cty.String {
				ss = append(ss, e.AsString())
			} else {
				ss = append(ss, e.GoString())
			}
		}
		sort.Strings(ss)
		return "[" + strings.Join(ss, ",") + "]"
	}
	return v.GoString()
}

type dynBlock struct {
	body    *hclsyntax.Block
	forEach hclsyntax.Expression // nil for a static block
}

// blocksOf returns a block's nested blocks of a type, static and dynamic (their content).
func blocksOf(b *hclsyntax.Block, typ string) []dynBlock {
	var out []dynBlock
	for _, s := range nested(b, typ) {
		out = append(out, dynBlock{body: s})
	}
	for _, d := range nested(b, "dynamic") {
		if len(d.Labels) != 1 || d.Labels[0] != typ {
			continue
		}
		var fe hclsyntax.Expression
		if a, ok := d.Body.Attributes["for_each"]; ok {
			fe = a.Expr
		}
		for _, c := range nested(d, "content") {
			out = append(out, dynBlock{body: c, forEach: fe})
		}
	}
	return out
}

// policyShape renders the statements a policy document has under ctx: sid, effect, actions, resources, principals
// and conditions, sorted, so that two documents with the same rendered policy compare equal.
func policyShape(t *testing.T, files []tfFile, doc *hclsyntax.Block, ctx *hcl.EvalContext) []string {
	t.Helper()
	var out []string
	for _, st := range blocksOf(doc, "statement") {
		if !exists(t, files, st.forEach, ctx) {
			continue
		}
		effect := renderValue(t, files, st.body, "effect", ctx)
		if effect == "" {
			effect = "Allow"
		}
		parts := []string{
			"sid=" + renderValue(t, files, st.body, "sid", ctx),
			"effect=" + effect,
			"actions=" + renderValue(t, files, st.body, "actions", ctx),
			"resources=" + renderValue(t, files, st.body, "resources", ctx),
		}
		var ps, cs []string
		for _, p := range blocksOf(st.body, "principals") {
			if exists(t, files, p.forEach, ctx) {
				ps = append(ps, renderValue(t, files, p.body, "type", ctx)+":"+renderValue(t, files, p.body, "identifiers", ctx))
			}
		}
		for _, c := range blocksOf(st.body, "condition") {
			if exists(t, files, c.forEach, ctx) {
				cs = append(cs, renderValue(t, files, c.body, "test", ctx)+" "+renderValue(t, files, c.body, "variable", ctx)+" "+
					renderValue(t, files, c.body, "values", ctx))
			}
		}
		sort.Strings(ps)
		sort.Strings(cs)
		parts = append(parts, "principals="+strings.Join(ps, ";"), "conditions="+strings.Join(cs, ";"))
		out = append(out, strings.Join(parts, " | "))
	}
	sort.Strings(out)
	return out
}

// S7: with store_kms_key_user_arns empty the store key policy is exactly what it was, so the CLI, rudolph-web and
// every other principal with table permissions keep reading the tables. An aws:PrincipalArn condition that is there
// whatever the list holds would lock them out of a deployment that never set it.
func TestDataExposure_StoreKeyUnchangedWhileTheListIsEmpty(t *testing.T) {
	parent := filesIn(loadTerraform(t, santaAPI), santaAPI)
	users, roles := storeKeyArgs(t, parent)
	if len(users) == 0 {
		t.Errorf("module \"rule_store\" is not given var.%s", storeKeyUsersVar)
	}
	docs, files := policyDocs(t, storeModule)
	doc, ok := docs["store_sse_permissions"]
	if !ok {
		t.Fatal("no aws_iam_policy_document.store_sse_permissions in the store module; the check is out of date")
	}
	storeFiles := filesIn(files, storeModule)
	empty := storeKeyEvalContext(t, storeFiles, users, roles, nil, nil)
	set := storeKeyEvalContext(t, storeFiles, users, roles, []string{testUserARN}, nil)

	// Every aws:PrincipalArn condition on a "*" statement exists only while the list is set.
	for _, st := range blocksOf(doc, "statement") {
		everyone := false
		for _, id := range awsIdentifiers(t, files, st.body) {
			if strings.Contains(id, `"*"`) {
				everyone = true
			}
		}
		if !everyone {
			continue
		}
		for _, c := range blocksOf(st.body, "condition") {
			v, ok := c.body.Body.Attributes["variable"]
			if !ok {
				continue
			}
			if name, _ := literal(v); !strings.EqualFold(name, "aws:PrincipalArn") {
				continue
			}
			sid := renderValue(t, files, st.body, "sid", empty)
			if c.forEach == nil && st.forEach == nil {
				t.Errorf("store key statement %q: its aws:PrincipalArn condition is there whatever %s holds; with the list empty "+
					"it locks the CLI and rudolph-web out. Put it in a dynamic \"condition\" whose for_each is empty while the list is", sid, storeKeyUsersVar)
				continue
			}
			if exists(t, files, st.forEach, empty) && exists(t, files, c.forEach, empty) {
				t.Errorf("store key statement %q: its aws:PrincipalArn condition exists while %s is empty; it must only exist while the list is set", sid, storeKeyUsersVar)
			}
			if !(exists(t, files, st.forEach, set) && exists(t, files, c.forEach, set)) {
				t.Errorf("store key statement %q: its aws:PrincipalArn condition does not exist while %s is set", sid, storeKeyUsersVar)
			}
		}
	}

	// The rendered shape with the list empty matches the policy as released, with and without key administrators.
	before, err := hclparse.NewParser().ParseHCL([]byte(storeKeyPolicyBefore), "store_kms_4644f87.tf")
	if err != nil {
		t.Fatalf("parse the earlier store key policy: %s", err.Error())
	}
	old := []tfFile{{path: "store_kms_4644f87.tf", body: before.Body.(*hclsyntax.Body), src: []byte(storeKeyPolicyBefore)}}
	oldDocs := blocks(old, "data", "aws_iam_policy_document", "store_sse_permissions")
	if len(oldDocs) != 1 {
		t.Fatal("the earlier store key policy has no store_sse_permissions")
	}
	for _, admins := range [][]string{nil, {testAdminARN}} {
		got := policyShape(t, files, doc, storeKeyEvalContext(t, storeFiles, users, roles, nil, admins))
		want := policyShape(t, old, oldDocs[0], storeKeyEvalContext(t, storeFiles, users, roles, nil, admins))
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("with %s empty and %d key administrators, the store key policy differs from the one released before the restriction:\n got:\n  %s\n want:\n  %s",
				storeKeyUsersVar, len(admins), strings.Join(got, "\n  "), strings.Join(want, "\n  "))
		}
	}
}

// storeKeyUserValues are entries of store_kms_key_user_arns that its validation must accept, and refuse.
var (
	storeKeyUsersGood = []string{
		"arn:aws:iam::123456789012:role/rudolph-web",
		"arn:aws:iam::123456789012:role/ops/rudolph-cli",
		"arn:aws:iam::123456789012:user/alice",
		"arn:aws:iam::123456789012:role/aws-reserved/sso.amazonaws.com/AWSReservedSSO_RudolphAdmin_*",
		"arn:aws:iam::123456789012:role/rudolph-web*",
		"arn:aws:iam::123456789012:role/aws-reserved/sso.amazonaws.com/ap-southeast-2/AWSReservedSSO_RudolphOperators_*",
		"arn:aws-us-gov:iam::123456789012:role/rudolph-web",
	}
	storeKeyUsersBad = []string{
		"*",
		"rudolph-web",
		"arn:aws:sts::123456789012:assumed-role/rudolph-web/session", // aws:PrincipalArn is the role's ARN, never this
		"arn:aws:iam::12345:role/rudolph-web",                        // not a 12-digit account
		"arn:aws:iam::*:role/rudolph-web",
		"arn:aws:iam::123456789012:*",
		"arn:aws:iam::123456789012:role/*",
		"arn:aws:iam::123456789012:user/*",
		// A wildcard that is a whole path segment lets in every role under it: every Identity Center permission set,
		// read-only ones included.
		"arn:aws:iam::123456789012:role/aws-reserved/*",
		"arn:aws:iam::123456789012:role/aws-reserved/sso.amazonaws.com/*",
		"arn:aws:iam::123456789012:role/rudolph-web,arn:aws:iam::123456789012:role/*",
	}
)

// S7: the list only narrows the key if each entry is an IAM principal ARN that can match aws:PrincipalArn, and none
// re-opens it to every principal.
func TestDataExposure_StoreKeyUsersAreIAMPrincipalARNs(t *testing.T) {
	parent := filesIn(loadTerraform(t, santaAPI), santaAPI)
	vs := blocks(parent, "variable", storeKeyUsersVar)
	if len(vs) != 1 {
		t.Fatalf("santa_api declares no variable %q", storeKeyUsersVar)
	}
	v := vs[0]
	if a, ok := v.Body.Attributes["nullable"]; !ok || strings.TrimSpace(sourceOf(t, parent, a.Expr)) != "false" {
		t.Errorf("variable %s: want nullable = false, so null means the empty default rather than an error in the key policy", storeKeyUsersVar)
	}
	if len(nested(v, "validation")) == 0 {
		t.Errorf("variable %s has no validation: an sts assumed-role ARN or a name never matches aws:PrincipalArn, and role/* or * "+
			"re-opens the key to every principal", storeKeyUsersVar)
	}

	tf, err := exec.LookPath("terraform")
	if err != nil {
		t.Skip("terraform not found: the validation was not exercised")
	}
	// The variable on its own, in a module of a scratch configuration: plan needs no provider, no network and no AWS.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "v"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "v", "main.tf"), []byte(blockText(t, parent, v)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := "variable \"in\" {\n  type = list(string)\n}\n\nmodule \"v\" {\n  source = \"./v\"\n  " + storeKeyUsersVar + " = var.in\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(root), 0o644); err != nil {
		t.Fatal(err)
	}
	// The child must not pick up a workspace, data directory or extra arguments from the caller's environment.
	var base []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if name == "TF_WORKSPACE" || name == "TF_DATA_DIR" || strings.HasPrefix(name, "TF_CLI_ARGS") {
			continue
		}
		base = append(base, kv)
	}
	base = append(base, "TF_IN_AUTOMATION=1")
	run := func(env []string, args ...string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, tf, args...)
		cmd.Dir = dir
		cmd.Env = append(append([]string{}, base...), env...)
		out, err := cmd.CombinedOutput()
		if ctx.Err() == context.DeadlineExceeded {
			t.Skipf("terraform %s did not finish within 60 seconds, so the validation was not exercised:\n%s", strings.Join(args, " "), out)
		}
		return string(out), err
	}
	if out, err := run(nil, "init", "-backend=false", "-input=false", "-no-color"); err != nil {
		t.Fatalf("terraform init of the scratch configuration: %v\n%s", err, out)
	}
	plan := func(entries []string) (string, error) {
		quoted := make([]string, len(entries))
		for i, e := range entries {
			quoted[i] = strconv.Quote(e)
		}
		return run([]string{"TF_VAR_in=[" + strings.Join(quoted, ",") + "]"}, "plan", "-input=false", "-no-color", "-lock=false")
	}
	if out, err := plan(nil); err != nil {
		t.Errorf("the validation refuses an empty list:\n%s", out)
	}
	for _, good := range storeKeyUsersGood {
		if out, err := plan([]string{good}); err != nil {
			t.Errorf("the validation refuses %q, an IAM principal ARN narrower than every principal:\n%s", good, out)
		}
	}
	for _, bad := range storeKeyUsersBad {
		entries := strings.Split(bad, ",")
		if out, err := plan(entries); err == nil {
			t.Errorf("the validation accepts %q", bad)
		} else if !strings.Contains(out, "Invalid value for variable") {
			t.Errorf("terraform plan with %q failed, but not on the variable's validation:\n%s", bad, out)
		}
	}
}

// The store key policy as released before the restriction (4644f87, modules/store/kms.tf): with the list empty the
// policy must render the same statements and conditions.
const storeKeyPolicyBefore = `// KMS Key for DynamoDB server-side encryption
resource "aws_kms_key" "store_sse_key" {
  enable_key_rotation = true
  description         = "Santa Rules Tables Server-Side Encryption"
  policy              = data.aws_iam_policy_document.store_sse_permissions.json
}

data "aws_iam_policy_document" "store_sse_permissions" {
  statement {
    sid       = "Enable IAM User Permissions"
    effect    = "Allow"
    actions   = ["kms:*"]
    resources = ["*"]
    principals {
      type = "AWS"
      identifiers = [
        "arn:aws:iam::${var.aws_account_id}:root",
      ]
    }
  }
  # Every principal of the account may use the key, but only through DynamoDB: reading or writing the tables is
  # governed by their IAM policies, so the Lambdas, the CLI and rudolph-web need no KMS grant of their own.
  statement {
    sid    = "Allow principals in the account to use the key"
    effect = "Allow"

    principals {
      type        = "AWS"
      identifiers = ["*"]
    }
    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["dynamodb.${var.region}.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "kms:CallerAccount"
      values   = [var.aws_account_id]
    }

    actions = [
      "kms:Decrypt",
      "kms:DescribeKey",
      "kms:Encrypt",
      "kms:GenerateDataKey*",
      "kms:ReEncrypt*",
    ]

    resources = ["*"]
  }

  # Grants in a statement of their own: kms:GrantIsForAWSResource is only present on grant requests, so in the
  # statement above it would deny every other action. DynamoDB creates grants on the key for the tables.
  statement {
    sid    = "Allow DynamoDB to create grants for principals in the account"
    effect = "Allow"

    principals {
      type        = "AWS"
      identifiers = ["*"]
    }
    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["dynamodb.${var.region}.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "kms:CallerAccount"
      values   = [var.aws_account_id]
    }
    condition {
      test     = "Bool"
      variable = "kms:GrantIsForAWSResource"
      values   = ["true"]
    }

    actions = [
      "kms:CreateGrant",
    ]

    resources = ["*"]
  }

  statement {
    sid    = "Allow DynamoDB to get information about the CMK"
    effect = "Allow"

    principals {
      type        = "Service"
      identifiers = ["dynamodb.amazonaws.com"]
    }

    actions = [
      "kms:Describe*",
      "kms:Get*",
      "kms:List*",
    ]

    resources = ["*"]
  }

  dynamic "statement" {
    for_each = length(var.kms_key_administrators_arns) == 0 ? [] : [1]

    content {
      sid    = "Allow access for Key Administrators"
      effect = "Allow"
      actions = [
        "kms:Create*",
        "kms:Describe*",
        "kms:Enable*",
        "kms:List*",
        "kms:Put*",
        "kms:Update*",
        "kms:Revoke*",
        "kms:Disable*",
        "kms:Get*",
        "kms:Delete*",
        "kms:TagResource",
        "kms:UntagResource",
        "kms:ScheduleKeyDeletion",
        "kms:CancelKeyDeletion",
        "kms:DescribeKey"
      ]
      resources = ["*"]

      # Administrators manage the key; they cannot encrypt, decrypt or make data keys with it
      principals {
        type        = "AWS"
        identifiers = var.kms_key_administrators_arns
      }
    }
  }
}

// KMS Alias for DynamoDB server-side encryption
resource "aws_kms_alias" "store_sse_key" {
  name          = "alias/${var.prefix}-santa-rules-tables-sse"
  target_key_id = aws_kms_key.store_sse_key.key_id
}
`

// S8, round 2: upgrade step 23 says what the lifecycle rule keeps (180 days in total, a noncurrent version 1 day of
// it), and deploy.md does not state as fact that Firehose checks the producer's key permissions on every
// PutRecordBatch, which AWS does not document.
func TestDataExposure_DeployDocsMatchTheLifecycleAndProducerKey(t *testing.T) {
	src, err := os.ReadFile("../docs/deploy.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(src)
	step := ""
	for _, p := range regexp.MustCompile(`(?m)^### `).Split(doc, -1)[1:] {
		h, body, _ := strings.Cut(p, "\n")
		if strings.HasPrefix(h, "23.") {
			step, _, _ = strings.Cut(body, "\n## ")
		}
	}
	if step == "" {
		t.Fatal("docs/deploy.md has no upgrade step 23")
	}
	total := []string{`180`, `in total|in all`, `noncurrent|old version`, `\b1 day\b|\bone day\b`}
	if !lineWith(step, total...) {
		t.Errorf("deploy.md step 23: no line says the Firehose bucket keeps events 180 days in total, with the noncurrent version "+
			"kept 1 day within it (want one matching %q)", total)
	}
	for _, line := range strings.Split(doc, "\n") {
		if lineWith(line, `\bchecks?\b`, `\bevery\b`, `PutRecordBatch`) {
			t.Errorf("deploy.md states as fact that Firehose checks the producer's key permissions on every PutRecordBatch, "+
				"which AWS does not document:\n%s", line)
		}
	}
}
