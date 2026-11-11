package deployments

// Top unknowns (owner, 2026-11-11): the ranker marks the unknown binaries that a global rule now covers, by GetItem on
// the store table's GlobalRules partition. Its role gains exactly that: GetItem on the store table, leading key
// GlobalRules only (no other partition, no index, no Query or Scan of the store table, no writes), and its Lambda is
// told the store table's name as DYNAMODB_NAME, like the other Lambdas.

import (
	"regexp"

	"github.com/hashicorp/hcl/v2/hclsyntax"
	"strings"
	"testing"
)

func TestRanker_ReadsOnlyGlobalRulesFromTheStoreTable(t *testing.T) {
	module := filesIn(loadTerraform(t, santaAPIDir), santaAPIDir)

	var policy *hclsyntax.Block
	for _, d := range blocks(module, "data", "aws_iam_policy_document") {
		if len(d.Labels) == 2 && d.Labels[1] == "ranker" {
			policy = d
		}
	}
	if policy == nil {
		t.Fatal(`no data "aws_iam_policy_document" "ranker"`)
	}

	var storeStatements int
	for _, st := range nested(policy, "statement") {
		actions := sourceOf(t, module, st.Body.Attributes["actions"].Expr)
		resources := sourceOf(t, module, st.Body.Attributes["resources"].Expr)
		if strings.Contains(resources, "events_dynamodb_table_arn") {
			if strings.Contains(actions, "GetItem") {
				t.Error("the ranker may GetItem on the events table; it needs only Query, PutItem and BatchWriteItem there")
			}
			continue
		}
		storeStatements++
		if got := regexp.MustCompile(`"dynamodb:[A-Za-z]+"`).FindAllString(actions, -1); strings.Join(got, ",") != `"dynamodb:GetItem"` {
			t.Errorf("store table statement allows %v, want only dynamodb:GetItem", got)
		}
		if strings.Contains(resources, "/index/") {
			t.Errorf("store table statement covers an index: %s", resources)
		}
		var leading string
		for _, c := range nested(st, "condition") {
			if v, _ := literal(c.Body.Attributes["variable"]); v == "dynamodb:LeadingKeys" {
				if test, _ := literal(c.Body.Attributes["test"]); test != "ForAllValues:StringEquals" && test != "ForAllValues:StringLike" {
					t.Errorf("LeadingKeys condition test %q, want ForAllValues:StringEquals", test)
				}
				leading = sourceOf(t, module, c.Body.Attributes["values"].Expr)
			}
		}
		if strings.Join(strings.Fields(leading), "") != `["GlobalRules"]` {
			t.Errorf("store table statement leading keys %q, want exactly [\"GlobalRules\"]", leading)
		}
	}
	if storeStatements != 1 {
		t.Errorf("%d ranker statements on the store table, want 1", storeStatements)
	}

	var fn *hclsyntax.Block
	for _, r := range blocks(module, "resource", "aws_lambda_function") {
		if r.Labels[1] == "ranker" {
			fn = r
		}
	}
	if fn == nil {
		t.Fatal("no ranker Lambda")
	}
	env := nested(fn, "environment")
	if len(env) != 1 {
		t.Fatal("ranker Lambda has no environment block")
	}
	vars := sourceOf(t, module, env[0].Body.Attributes["variables"].Expr)
	if !regexp.MustCompile(`DYNAMODB_NAME\s*=\s*local\.dynamodb_table_name`).MatchString(vars) {
		t.Errorf("ranker environment does not set DYNAMODB_NAME = local.dynamodb_table_name:\n%s", vars)
	}
}
