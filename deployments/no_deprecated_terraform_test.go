package deployments

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// No deprecated Terraform before the first production deployment (owner, 2026-11-07): `terraform validate` with the
// AWS provider 6.x warned about aws_s3_bucket_object and the DynamoDB tables' hash_key/range_key.
func TestNoDeprecatedTerraform(t *testing.T) {
	files := loadTerraform(t, ".")
	if n := len(blocks(files, "resource", "aws_s3_bucket_object")); n != 0 {
		t.Errorf("%d aws_s3_bucket_object resources; use aws_s3_object (with moved blocks so nothing is recreated)", n)
	}
	for _, tb := range blocks(files, "resource", "aws_dynamodb_table") {
		for _, attr := range []string{"hash_key", "range_key"} {
			if _, ok := tb.Body.Attributes[attr]; ok {
				t.Errorf("aws_dynamodb_table.%s sets %s; use key_schema", tb.Labels[1], attr)
			}
		}
		for _, gsi := range nested(tb, "global_secondary_index") {
			for _, attr := range []string{"hash_key", "range_key"} {
				if _, ok := gsi.Body.Attributes[attr]; ok {
					t.Errorf("aws_dynamodb_table.%s: a global secondary index sets %s; use key_schema", tb.Labels[1], attr)
				}
			}
		}
	}
	// Terraform itself: at least 1.10 (older releases are no longer maintained).
	re := regexp.MustCompile(`required_version\s*=\s*">=\s*1\.(\d+)`)
	found := 0
	for _, f := range files {
		for _, m := range re.FindAllStringSubmatch(string(f.src), -1) {
			found++
			if minor, _ := strconv.Atoi(m[1]); minor < 10 {
				t.Errorf("%s: required_version >= 1.%s; want >= 1.10", f.path, m[1])
			}
		}
		if strings.Contains(string(f.src), "required_version") && !re.Match(f.src) {
			t.Errorf("%s: required_version is not of the form \">= 1.x\"", f.path)
		}
	}
	if found == 0 {
		t.Error("no required_version found")
	}
}
