package deployments

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// DynamoDB Local does not enforce IAM, so no integration test can notice that the Lambdas are not allowed to do
// something the code does. These checks tie the store policy to the code instead.

// Operations the Lambdas never perform: only the CLI (which runs with the operator's own credentials) uses them.
// Adding an operation here is a decision: say why in the comment.
var cliOnlyDynamoDBOperations = map[string]string{
	"TransactWriteItems": "global rule add/update/remove (CLI only)",
	"Scan":               "CLI lookups and exports",
}

type storeStatement struct {
	actions     map[string]bool
	leadingKeys []string // empty when the statement has no LeadingKeys condition
}

func storePolicyStatements(t *testing.T) []storeStatement {
	t.Helper()
	files := loadTerraform(t, "terraform_modules/santa_api/modules/store")
	docs := blocks(files, "data", "aws_iam_policy_document", "lambda_permissions")
	if len(docs) != 1 {
		t.Fatalf("expected one lambda_permissions policy document, found %d", len(docs))
	}
	var out []storeStatement
	for _, st := range nested(docs[0], "statement") {
		s := storeStatement{actions: map[string]bool{}}
		if attr, ok := st.Body.Attributes["actions"]; ok {
			v, diags := attr.Expr.Value(nil)
			if diags.HasErrors() {
				t.Fatalf("actions must be a literal list: %s", diags.Error())
			}
			for it := v.ElementIterator(); it.Next(); {
				_, e := it.Element()
				s.actions[strings.TrimPrefix(e.AsString(), "dynamodb:")] = true
			}
		}
		for _, cond := range nested(st, "condition") {
			variable, _ := literal(cond.Body.Attributes["variable"])
			if variable != "dynamodb:LeadingKeys" {
				continue
			}
			v, diags := cond.Body.Attributes["values"].Expr.Value(nil)
			if diags.HasErrors() {
				t.Fatalf("LeadingKeys values must be a literal list: %s", diags.Error())
			}
			for it := v.ElementIterator(); it.Next(); {
				_, e := it.Element()
				s.leadingKeys = append(s.leadingKeys, e.AsString())
			}
		}
		out = append(out, s)
	}
	return out
}

// Every DynamoDB operation the shared client can issue must be allowed for the Lambdas, unless it is CLI-only.
func TestStoreIAMPolicy_AllowsEveryOperationTheCodeUses(t *testing.T) {
	call := regexp.MustCompile(`\b(?:api|awsclient|dbc\.awsclient)\.([A-Z][A-Za-z]+)\(ctx`)
	used := map[string]string{}
	err := filepath.Walk("../pkg/dynamodb", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, _ := os.ReadFile(path)
		for _, m := range call.FindAllStringSubmatch(string(src), -1) {
			used[m[1]] = path
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(used) == 0 {
		t.Fatal("found no DynamoDB calls in pkg/dynamodb; the pattern is out of date")
	}

	allowed := map[string]bool{}
	for _, s := range storePolicyStatements(t) {
		for a := range s.actions {
			allowed[a] = true
		}
	}
	var missing []string
	for op, path := range used {
		if allowed[op] {
			continue
		}
		if _, cliOnly := cliOnlyDynamoDBOperations[op]; cliOnly {
			continue
		}
		missing = append(missing, op+" (used in "+path+")")
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("the Lambda store policy does not allow DynamoDB operations the code uses:\n  %s", strings.Join(missing, "\n  "))
	}
}

// Per-machine partitions (key prefixes ending in "#") are written by the sync Lambdas, so every such prefix must be
// covered by the LeadingKeys condition of the write statement, and every write operation must be in that statement.
func TestStoreIAMPolicy_WriteStatementCoversEveryMachinePartition(t *testing.T) {
	prefixConst := regexp.MustCompile(`(?m)^\s*(?:const\s+)?\w*PK\w*\s*(?:string\s*)?=\s*"([A-Za-z]+#)"`)
	prefixes := map[string]string{}
	err := filepath.Walk("../pkg/model", func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		src, _ := os.ReadFile(path)
		for _, m := range prefixConst.FindAllStringSubmatch(string(src), -1) {
			prefixes[m[1]] = path
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(prefixes) == 0 {
		t.Fatal("found no per-machine partition key prefixes in pkg/model; the pattern is out of date")
	}

	var write *storeStatement
	statements := storePolicyStatements(t)
	for i := range statements {
		if statements[i].actions["PutItem"] {
			write = &statements[i]
		}
	}
	if write == nil {
		t.Fatal("no statement allows PutItem")
	}
	for _, op := range []string{"PutItem", "UpdateItem", "DeleteItem", "BatchWriteItem"} {
		if !write.actions[op] {
			t.Errorf("the write statement does not allow %s", op)
		}
	}
	for prefix, path := range prefixes {
		covered := false
		for _, pattern := range write.leadingKeys {
			if pattern == prefix+"*" {
				covered = true
			}
		}
		if !covered {
			t.Errorf("partition prefix %q (%s) is not covered by the write statement's dynamodb:LeadingKeys %v", prefix, path, write.leadingKeys)
		}
	}
}
