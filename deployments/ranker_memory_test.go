package deployments

// Ranker memory (owner, 2026-11-22): the compact fold holds about 350 bytes a binary, so 256 MB holds several
// hundred thousand distinct binaries, far more than a fleet of a few thousand Macs sees in 90 days. The owner kept
// the ranker at 256 MB rather than pay for 512 MB of headroom; each run logs binaries=<n> to compare with Max Memory
// Used, and memory_size is raised in ranker.tf when that nears the limit.

import (
	"testing"
)

func TestRankerMemory_LambdaHas256MB(t *testing.T) {
	files := loadTerraform(t, "terraform_modules/santa_api")
	fns := blocks(files, "resource", "aws_lambda_function", "ranker")
	if len(fns) != 1 {
		t.Fatalf("%d ranker functions", len(fns))
	}
	attr, ok := fns[0].Body.Attributes["memory_size"]
	if !ok {
		t.Fatal("the ranker sets no memory_size")
	}
	v, diags := attr.Expr.Value(nil)
	if diags.HasErrors() {
		t.Fatalf("memory_size must be a literal: %s", diags.Error())
	}
	if got, _ := v.AsBigFloat().Int64(); got != 256 {
		t.Errorf("ranker memory_size = %d, want 256", got)
	}
}
