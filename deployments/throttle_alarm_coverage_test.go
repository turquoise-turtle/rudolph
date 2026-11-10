package deployments

// DynamoDB throttle alarms cover every table and every global secondary index (review before fleet rollout): the
// alarm watched only the rules table, so throttling on the events table (every eventupload write, the ranker's reads,
// rudolph-web's event indexes) or on any index went unnoticed. Index throttles are reported under the
// GlobalSecondaryIndexName dimension, so each index is named in the module's alarm configuration; adding an index
// without alarming on it fails here.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestThrottleAlarms_CoverEveryTableAndIndex(t *testing.T) {
	moduleDir := filepath.Join("terraform_modules", "santa_api")
	files := loadTerraform(t, moduleDir)

	var indexes []string
	for _, table := range blocks(files, "resource", "aws_dynamodb_table") {
		for _, gsi := range nested(table, "global_secondary_index") {
			attr, ok := gsi.Body.Attributes["name"]
			if !ok {
				t.Fatalf("a global_secondary_index of %v has no name", table.Labels)
			}
			indexes = append(indexes, strings.Trim(strings.TrimSpace(sourceOf(t, files, attr.Expr)), `"`))
		}
	}
	if len(indexes) < 6 {
		t.Fatalf("found only %d indexes (%v); the test no longer reads the store module correctly", len(indexes), indexes)
	}

	// The module's own files (not its submodules) hold the alarms.
	entries, err := os.ReadDir(moduleDir)
	if err != nil {
		t.Fatal(err)
	}
	var top strings.Builder
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".tf") {
			b, err := os.ReadFile(filepath.Join(moduleDir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			top.Write(hclComment.ReplaceAll(b, nil))
			top.WriteByte('\n')
		}
	}
	src := top.String()

	for _, want := range []string{"ReadThrottleEvents", "WriteThrottleEvents", "GlobalSecondaryIndexName",
		"module.rule_store.dynamodb_table_name", "module.rule_store.events_dynamodb_table_name"} {
		if !strings.Contains(src, want) {
			t.Errorf("the module's throttle alarms do not use %s", want)
		}
	}
	for _, name := range indexes {
		if !strings.Contains(src, `"`+name+`"`) {
			t.Errorf("index %s has no throttle alarm (name it with the GlobalSecondaryIndexName dimension)", name)
		}
	}
}
