package deployments

// IAM review (2026-11-18, owner approved fixing every finding). DynamoDB Local and the unit tests never enforce IAM,
// so these checks pin the least-privilege shapes in the Terraform itself:
//   - Firehose sink: the eventupload Lambda may only PutRecordBatch (the only call in pkg/firehose); the Firehose
//     role writes logs only to its own log group, by reference, with no account-wide DescribeLogStreams and no
//     CreateLogGroup; the Firehose module receives eventupload_firehose_name and kms_key_administrators_arns; the
//     Firehose role trusts firehose.amazonaws.com only for this account (aws:SourceAccount).
//   - KMS: no key policy statement lets every principal of the account ("*") use a key unless it is limited to use
//     through an AWS service (kms:ViaService); CreateGrant for "*" also needs kms:GrantIsForAWSResource; key
//     administrators manage keys but cannot encrypt, decrypt or make data keys.
//   - Store table: the health Lambda only Queries GlobalRules (a policy of its own, not the shared store policy); the
//     shared Lambda store policy grants no Scan or DescribeTable, no index ARN and no legacy MachineInfo# partition.
//   - Events table: eventupload writes only Exec#, FileAccess#, BinaryDay#, BinarySeen# and (Lockdown readiness,
//     2026-11-22) UnknownSeen#.
//   - Every ForAllValues condition on dynamodb:LeadingKeys has a Null check beside it, as ForAllValues is true when
//     the request carries no key at all (a Scan added to the statement later would otherwise be allowed).
//   - Logs: no Lambda gets AWSLambdaBasicExecutionRole (logs on every group of the account); each writes its own.
//   - The scheduler role trusts scheduler.amazonaws.com only for the default schedule group (aws:SourceArn).
//   - S3: every bucket has a public access block.
//   - The API Gateway resource policy has no statement that changes nothing (the aws:ViaAWSService Allow is a subset
//     of the unconditional Allow), and no comment calls /health public (it needs the token).
//   - docs/cli-iam.md offers example policies: a read-only operator and a rule administrator for the CLI, and says
//     what iam:PassRole a deployer needs.

import (
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2/hclsyntax"
)

const santaAPI = "terraform_modules/santa_api"

func blockText(t *testing.T, files []tfFile, b *hclsyntax.Block) string {
	t.Helper()
	r := b.Range()
	for _, f := range files {
		if f.path == r.Filename {
			return stripTFComments(string(f.src[r.Start.Byte:r.End.Byte]))
		}
	}
	t.Fatalf("source of %s not found", r)
	return ""
}

var tfComment = regexp.MustCompile(`(?m)^\s*(#|//).*$`)

func stripTFComments(s string) string { return tfComment.ReplaceAllString(s, "") }

var spaces = regexp.MustCompile(`\s+`)

func squash(s string) string { return spaces.ReplaceAllString(s, " ") }

func allText(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	for _, f := range loadTerraform(t, root) {
		b.WriteString(stripTFComments(string(f.src)))
		b.WriteString("\n")
	}
	return b.String()
}

// policyDocs returns every aws_iam_policy_document under root, keyed by name.
func policyDocs(t *testing.T, root string) (map[string]*hclsyntax.Block, []tfFile) {
	t.Helper()
	files := loadTerraform(t, root)
	out := map[string]*hclsyntax.Block{}
	for _, b := range blocks(files, "data", "aws_iam_policy_document") {
		out[b.Labels[1]] = b
	}
	return out, files
}

// statementsOf returns the statements of a policy document, including dynamic ones (their content block).
func statementsOf(b *hclsyntax.Block) []*hclsyntax.Block {
	out := nested(b, "statement")
	for _, d := range nested(b, "dynamic") {
		for _, c := range nested(d, "content") {
			out = append(out, c)
		}
	}
	return out
}

func TestIAMReview_FirehoseLambdaMayOnlyPutRecords(t *testing.T) {
	docs, files := policyDocs(t, santaAPI+"/modules/firehose")
	doc := docs["rudolph_firehose_eventupload"]
	if doc == nil {
		t.Fatal("no rudolph_firehose_eventupload policy document")
	}
	text := blockText(t, files, doc)
	actions := regexp.MustCompile(`"firehose:[A-Za-z]+"`).FindAllString(text, -1)
	if strings.Join(actions, ",") != `"firehose:PutRecordBatch"` {
		t.Errorf("the eventupload Lambda's Firehose actions are %v, want only firehose:PutRecordBatch", actions)
	}
}

func TestIAMReview_FirehoseLogsGoToItsOwnGroup(t *testing.T) {
	text := allText(t, santaAPI+"/modules/firehose")
	for _, bad := range []string{`"arn:aws:logs:*:*:*"`, "logs:DescribeLogStreams", "logs:CreateLogGroup", "-rudolph-eventsupload-firehose"} {
		if strings.Contains(text, bad) {
			t.Errorf("the Firehose module still grants or names %s", bad)
		}
	}
	if !strings.Contains(text, "aws_cloudwatch_log_group.eventsupload_firehose.arn") {
		t.Error("the Firehose role's log grant does not refer to its log group resource")
	}
}

func TestIAMReview_FirehoseModuleGetsItsSettings(t *testing.T) {
	files := loadTerraform(t, santaAPI)
	var mod *hclsyntax.Block
	for _, m := range blocks(files, "module", "firehose") {
		if strings.HasSuffix(m.Range().Filename, "santa_api/firehose.tf") {
			mod = m
		}
	}
	if mod == nil {
		t.Fatal(`no module "firehose" in santa_api/firehose.tf`)
	}
	for _, attr := range []string{"eventupload_firehose_name", "kms_key_administrators_arns"} {
		if _, ok := mod.Body.Attributes[attr]; !ok {
			t.Errorf("the firehose module is not given %s", attr)
		}
	}
}

func TestIAMReview_ServiceRoleTrustIsPinned(t *testing.T) {
	firehose, ffiles := policyDocs(t, santaAPI+"/modules/firehose")
	doc := firehose["firehose_assume_role_policy"]
	if doc == nil || !strings.Contains(blockText(t, ffiles, doc), "aws:SourceAccount") {
		t.Error("the Firehose role trusts firehose.amazonaws.com without aws:SourceAccount")
	}
	top, tfiles := policyDocs(t, santaAPI)
	doc = top["ranker_scheduler_assume_role"]
	if doc == nil || !strings.Contains(blockText(t, tfiles, doc), "aws:SourceArn") {
		t.Error("the scheduler role trusts scheduler.amazonaws.com without aws:SourceArn (the default schedule group)")
	}
}

func TestIAMReview_KeyPolicies(t *testing.T) {
	for _, dir := range []string{"/modules/store", "/modules/firehose"} {
		docs, files := policyDocs(t, santaAPI+dir)
		found := false
		for name, doc := range docs {
			for _, st := range statementsOf(doc) {
				text := squash(blockText(t, files, st))
				if !strings.Contains(text, "kms:") {
					continue
				}
				found = true
				everyone := strings.Contains(text, `identifiers = ["*"]`)
				if everyone && !strings.Contains(text, "kms:ViaService") {
					t.Errorf("%s %s: a statement lets every principal of the account use the key directly (no kms:ViaService)", dir, name)
				}
				if everyone && strings.Contains(text, "kms:CreateGrant") && !strings.Contains(text, "kms:GrantIsForAWSResource") {
					t.Errorf("%s %s: CreateGrant for every principal without kms:GrantIsForAWSResource", dir, name)
				}
				if strings.Contains(text, "var.kms_key_administrators_arns") {
					for _, use := range []string{`"kms:Encrypt"`, `"kms:Decrypt"`, `"kms:ReEncrypt*"`, `"kms:GenerateDataKey*"`} {
						if strings.Contains(text, use) {
							t.Errorf("%s %s: key administrators may also %s", dir, name, use)
						}
					}
				}
			}
		}
		if !found {
			t.Errorf("%s: no KMS key policy statements found; the check is out of date", dir)
		}
	}
}

func TestIAMReview_HealthOnlyQueriesGlobalRules(t *testing.T) {
	files := loadTerraform(t, santaAPI)
	store := blocks(files, "module", "rule_store")
	if len(store) != 1 {
		t.Fatalf("%d rule_store modules", len(store))
	}
	// Only the policy arguments: the store key's optional restriction (S7) passes the health Lambda's role to the
	// module too, so the key keeps allowing it.
	for _, arg := range []string{"read_lambda_role_names", "events_write_lambda_role_names"} {
		if a, ok := store[0].Body.Attributes[arg]; ok && strings.Contains(sourceOf(t, files, a.Expr), "health_function") {
			t.Errorf("the health Lambda still gets the shared store policy (%s)", arg)
		}
	}
	text := allText(t, santaAPI)
	if !strings.Contains(text, "module.health_function.lambda_role_name") {
		t.Fatal("the health Lambda's role is given no policy")
	}
	docs, dfiles := policyDocs(t, santaAPI)
	ok := false
	for _, doc := range docs {
		d := squash(blockText(t, dfiles, doc))
		var actions []string
		for _, st := range statementsOf(doc) {
			if a, has := st.Body.Attributes["actions"]; has {
				actions = append(actions, regexp.MustCompile(`"[a-z0-9]+:[A-Za-z*]+"`).FindAllString(sourceOf(t, dfiles, a.Expr), -1)...)
			}
		}
		if strings.Join(actions, ",") == `"dynamodb:Query"` && strings.Contains(d, `"GlobalRules"`) && strings.Contains(d, "dynamodb:LeadingKeys") {
			ok = true
		}
	}
	if !ok {
		t.Error("no policy document allows only dynamodb:Query on the GlobalRules partition")
	}
}

func TestIAMReview_SharedStorePolicyLeavesOutCLIOnlyAccess(t *testing.T) {
	for _, s := range storePolicyStatements(t) {
		for _, a := range []string{"Scan", "DescribeTable"} {
			if s.actions[a] {
				t.Errorf("the Lambda store policy grants dynamodb:%s, which no Lambda calls", a)
			}
		}
		for _, k := range s.leadingKeys {
			if strings.HasPrefix(k, "MachineInfo") {
				t.Errorf("the Lambda store policy still names the legacy %s partition", k)
			}
		}
	}
	docs, files := policyDocs(t, santaAPI+"/modules/store")
	if strings.Contains(blockText(t, files, docs["lambda_permissions"]), "/index/") {
		t.Error("the Lambda store policy grants the table's indexes, which no sync Lambda queries")
	}
}

func TestIAMReview_EventuploadWritesOnlyItsPartitions(t *testing.T) {
	docs, files := policyDocs(t, santaAPI+"/modules/store")
	doc := docs["events_write"]
	if doc == nil {
		t.Fatal("no events_write policy document")
	}
	want := []string{"BinaryDay#*", "BinarySeen#*", "Exec#*", "FileAccess#*", "UnknownSeen#*"}
	for _, st := range statementsOf(doc) {
		text := squash(blockText(t, files, st))
		if !strings.Contains(text, `"dynamodb:PutItem"`) || !strings.Contains(text, "aws_dynamodb_table.events.arn") {
			continue
		}
		var keys []string
		for _, cond := range nested(st, "condition") {
			test, _ := literal(cond.Body.Attributes["test"])
			if v, _ := literal(cond.Body.Attributes["variable"]); v == "dynamodb:LeadingKeys" && strings.HasPrefix(test, "ForAllValues:") {
				val, diags := cond.Body.Attributes["values"].Expr.Value(nil)
				if diags.HasErrors() {
					t.Fatal(diags.Error())
				}
				for it := val.ElementIterator(); it.Next(); {
					_, e := it.Element()
					keys = append(keys, e.AsString())
				}
			}
		}
		sort.Strings(keys)
		if strings.Join(keys, ",") != strings.Join(want, ",") {
			t.Errorf("events table writes are limited to %v, want %v", keys, want)
		}
		return
	}
	t.Error("no events_write statement grants PutItem on the events table")
}

func TestIAMReview_LeadingKeysConditionsRequireTheKey(t *testing.T) {
	docs, files := policyDocs(t, santaAPI)
	checked := 0
	for name, doc := range docs {
		for _, st := range statementsOf(doc) {
			var forAll, null bool
			for _, cond := range nested(st, "condition") {
				variable, _ := literal(cond.Body.Attributes["variable"])
				test, _ := literal(cond.Body.Attributes["test"])
				if variable != "dynamodb:LeadingKeys" {
					continue
				}
				if strings.HasPrefix(test, "ForAllValues:") {
					forAll = true
				}
				if test == "Null" && strings.Contains(squash(blockText(t, files, cond)), `"false"`) {
					null = true
				}
			}
			if forAll {
				checked++
				if !null {
					t.Errorf("%s: a ForAllValues LeadingKeys condition without a Null check beside it", name)
				}
			}
		}
	}
	if checked == 0 {
		t.Error("found no LeadingKeys conditions; the check is out of date")
	}
}

func TestIAMReview_LambdasWriteOnlyTheirOwnLogs(t *testing.T) {
	text := allText(t, santaAPI)
	if strings.Contains(text, "AWSLambdaBasicExecutionRole") {
		t.Error("a Lambda role still has AWSLambdaBasicExecutionRole (logs on every group of the account)")
	}
	if regexp.MustCompile(`arn:aws:logs:[^"]*\*:\*`).MatchString(text) {
		t.Error("a logs grant covers every log group")
	}
	for _, want := range []string{"aws_cloudwatch_log_group.api_handler.arn", "aws_cloudwatch_log_group.ranker.arn"} {
		if !strings.Contains(text, want) {
			t.Errorf("no log grant refers to %s", want)
		}
	}
}

func TestIAMReview_BucketsBlockPublicAccess(t *testing.T) {
	files := loadTerraform(t, santaAPI)
	text := allText(t, santaAPI)
	for _, b := range blocks(files, "resource", "aws_s3_bucket") {
		name := b.Labels[1]
		ok := false
		for _, pab := range blocks(files, "resource", "aws_s3_bucket_public_access_block") {
			if pab.Range().Filename == b.Range().Filename && strings.Contains(blockText(t, files, pab), "aws_s3_bucket."+name) {
				ok = true
			}
		}
		if !ok {
			t.Errorf("bucket %s (%s) has no public access block", name, b.Range().Filename)
		}
	}
	for _, setting := range []string{"block_public_acls", "block_public_policy", "ignore_public_acls", "restrict_public_buckets"} {
		if strings.Count(text, setting) < len(blocks(files, "resource", "aws_s3_bucket")) {
			t.Errorf("not every public access block sets %s", setting)
		}
	}
}

func TestIAMReview_APIResourcePolicyIsTidy(t *testing.T) {
	text := allText(t, santaAPI)
	if strings.Contains(text, "aws:ViaAWSService") {
		t.Error("the API Gateway resource policy still has the aws:ViaAWSService Allow, a subset of the unconditional Allow")
	}
	src, err := os.ReadFile(santaAPI + "/iam.tf")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(src)), "public health") {
		t.Error("iam.tf still calls /health public; it needs the token")
	}
}

func TestIAMReview_CLIPolicyExamples(t *testing.T) {
	src, err := os.ReadFile("../docs/cli-iam.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(src)
	var policies []map[string]any
	for _, m := range regexp.MustCompile("(?s)```json\n(.*?)```").FindAllStringSubmatch(doc, -1) {
		var p map[string]any
		if err := json.Unmarshal([]byte(m[1]), &p); err != nil {
			t.Errorf("a JSON block in docs/cli-iam.md does not parse: %v\n%s", err, m[1])
			continue
		}
		if _, ok := p["Statement"]; ok {
			policies = append(policies, p)
		}
	}
	if len(policies) < 2 {
		t.Fatalf("%d example policies in docs/cli-iam.md, want a read-only one and a rule administrator one", len(policies))
	}
	actionsOf := func(p map[string]any) string {
		raw, _ := json.Marshal(p["Statement"])
		return string(raw)
	}
	readOnly, admin := actionsOf(policies[0]), actionsOf(policies[1])
	for _, write := range []string{"PutItem", "UpdateItem", "DeleteItem", "BatchWriteItem"} {
		if strings.Contains(readOnly, write) {
			t.Errorf("the first (read-only) example grants %s", write)
		}
	}
	for _, need := range []string{"dynamodb:Scan", "dynamodb:Query", "dynamodb:PutItem", "dynamodb:UpdateItem", "dynamodb:DeleteItem", "dynamodb:BatchWriteItem", "Audit#"} {
		if !strings.Contains(admin, need) {
			t.Errorf("the rule administrator example lacks %s", need)
		}
	}
	if !strings.Contains(doc, "iam:PassRole") {
		t.Error("docs/cli-iam.md does not say what iam:PassRole a deployer needs")
	}
}
