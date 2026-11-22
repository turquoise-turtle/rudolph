package deployments

// Ranker memory (owner approved, 2026-11-22): the ranker folds every binary of 90 days of counters in memory, so it
// runs with 512 MB, twice its old 256, for headroom; a run lasts seconds to a couple of minutes an hour, a few cents a
// month more.

import (
	"testing"
)

func TestRankerMemory_LambdaHas512MB(t *testing.T) {
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
	if got, _ := v.AsBigFloat().Int64(); got != 512 {
		t.Errorf("ranker memory_size = %d, want 512", got)
	}
}
