package deployments

// Data exposure review (2026-11-18, owner decisions 2026-11-19). Events carry user names, hostnames and file paths;
// preflight carries serial numbers, hostnames, primary users and their groups.
//   - S4: the Firehose events bucket must not keep events forever. The events table expires them after 90 days, and
//     the bucket is versioned, so without a lifecycle rule every object, and every overwritten or deleted version,
//     stays readable indefinitely. The owner chose 180 days by default, set by a santa_api variable passed down to
//     the Firehose module.
//   - S5: the Firehose delivery stream encrypts the records it buffers with the module's own key, as the bucket does.
//   - S7: the store key lets every principal of the account use it through DynamoDB, so any role with DynamoDB read
//     access (AWS ReadOnlyAccess, say) reads all telemetry. The owner chose an optional restriction: santa_api
//     variable store_kms_key_user_arns, empty by default (the policy is unchanged); when set, "*" may only use the key
//     or create grants when narrowed to Rudolph's own Lambda roles and the listed ARNs.
//   - S8: docs/security-risks.md says what a rogue server receives and where each copy of the telemetry is kept, and
//     for how long.
//
// The key policy is a data source with dynamic blocks, and rendering it needs terraform init with the AWS provider,
// so the S7 check reads the HCL. It accepts either shape of fix: the "*" statements in a dynamic block that only
// exists while the variable is empty, or "*" statements narrowed by an aws:PrincipalArn condition (static or in a
// dynamic block on the variable).

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2/hclsyntax"
)

const (
	firehoseModule   = santaAPI + "/modules/firehose"
	storeModule      = santaAPI + "/modules/store"
	storeKeyUsersVar = "store_kms_key_user_arns"
	// The owner's choice (2026-11-19): twice the events table's 90 days, so the bucket stays the longer record.
	firehoseRetentionDefaultDays = 180
)

var tfVarRef = regexp.MustCompile(`^var\.([A-Za-z0-9_]+)$`)

// varOf returns the variable an attribute is set from, failing the test when it is a literal or anything else.
func varOf(t *testing.T, files []tfFile, b *hclsyntax.Block, attr, what string) string {
	t.Helper()
	a, ok := b.Body.Attributes[attr]
	if !ok {
		t.Errorf("%s: no %s", what, attr)
		return ""
	}
	src := strings.TrimSpace(sourceOf(t, files, a.Expr))
	m := tfVarRef.FindStringSubmatch(src)
	if m == nil {
		t.Errorf("%s: %s = %s, want it set from a variable so operators choose the retention", what, attr, src)
		return ""
	}
	return m[1]
}

// firehoseRetentionVars returns the santa_api variables that set the Firehose bucket's expiry of current objects and
// of noncurrent versions, reporting what is missing. Either is "" when it cannot be found.
func firehoseRetentionVars(t *testing.T, report func(format string, args ...any)) (current, noncurrent string) {
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
		return "", ""
	}

	var currentVar, noncurrentVar string
	for _, rule := range nested(lifecycle, "rule") {
		if s, ok := rule.Body.Attributes["status"]; !ok {
			continue
		} else if v, _ := literal(s); v != "Enabled" {
			continue
		}
		for _, e := range nested(rule, "expiration") {
			currentVar = varOf(t, files, e, "days", "lifecycle expiration")
		}
		for _, e := range nested(rule, "noncurrent_version_expiration") {
			noncurrentVar = varOf(t, files, e, "noncurrent_days", "lifecycle noncurrent_version_expiration")
		}
	}
	if currentVar == "" {
		report("no enabled lifecycle rule expires current objects (expiration { days = var.… })")
	}
	if noncurrentVar == "" {
		report("no enabled lifecycle rule expires noncurrent versions (noncurrent_version_expiration { noncurrent_days = var.… }); " +
			"the bucket is versioned, so expired or overwritten events would otherwise survive as old versions")
	}

	// Each module variable must be given a santa_api variable through module "firehose".
	parent := filesIn(loadTerraform(t, santaAPI), santaAPI)
	calls := blocks(parent, "module", "firehose")
	if len(calls) != 1 {
		t.Fatalf("module \"firehose\" called %d times in %s, want once", len(calls), santaAPI)
	}
	resolve := func(v string) string {
		if v == "" {
			return ""
		}
		if len(blocks(files, "variable", v)) != 1 {
			report("%s does not declare variable %q", filepath.Base(firehoseModule), v)
		}
		arg, ok := calls[0].Body.Attributes[v]
		if !ok {
			report("module \"firehose\" is not given %s, so the retention cannot be set from the deployment", v)
			return ""
		}
		m := tfVarRef.FindStringSubmatch(strings.TrimSpace(sourceOf(t, parent, arg.Expr)))
		if m == nil {
			report("module \"firehose\" %s = %s, want a santa_api variable", v, sourceOf(t, parent, arg.Expr))
			return ""
		}
		return m[1]
	}
	return resolve(currentVar), resolve(noncurrentVar)
}

// S4
func TestDataExposure_FirehoseEventsBucketExpiresEvents(t *testing.T) {
	current, noncurrent := firehoseRetentionVars(t, t.Errorf)
	parent := filesIn(loadTerraform(t, santaAPI), santaAPI)
	seen := map[string]bool{}
	for _, v := range []string{current, noncurrent} {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		def, ok := variableDefault(t, parent, v)
		if !ok {
			t.Errorf("santa_api variable %s has no default; the owner chose a default of %d days", v, firehoseRetentionDefaultDays)
			continue
		}
		if days, err := strconv.Atoi(def); err != nil || days != firehoseRetentionDefaultDays {
			t.Errorf("santa_api variable %s defaults to %s, want %d (days)", v, def, firehoseRetentionDefaultDays)
		}
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

// tableRoles are the roles of every Lambda that reads or writes the store or events table, as santa_api refers to
// them. The api-handler module's outputs are not pinned (it has lambda_role_name today; a fix may add an ARN). The
// ranker's role has path /rudolph/, so an ARN built from its name as role/<name> would not match it: its .arn is
// required.
var tableRoles = map[string]*regexp.Regexp{
	"health":       regexp.MustCompile(`module\.health_function\.\w+`),
	"preflight":    regexp.MustCompile(`module\.preflight_function\.\w+`),
	"ruledownload": regexp.MustCompile(`module\.ruledownload_function\.\w+`),
	"postflight":   regexp.MustCompile(`module\.postflight_function\.\w+`),
	"eventupload":  regexp.MustCompile(`module\.eventupload_function\.\w+`),
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
			t.Errorf("module \"rule_store\" is not given the %s Lambda's role; it reads or writes the tables, so the key must keep allowing it", role)
		}
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
	if current, noncurrent := firehoseRetentionVars(t, func(string, ...any) {}); current != "" {
		firehose = append(firehose, regexp.QuoteMeta(current))
		if noncurrent != "" && noncurrent != current {
			firehose = append(firehose, regexp.QuoteMeta(noncurrent))
		}
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
