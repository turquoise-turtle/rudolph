package deployments

// S3 server access logs: AWS recommends granting the logging service through the destination bucket's policy, with
// ACLs disabled (BucketOwnerEnforced), rather than the log-delivery-write ACL. Production (owner, 2026-11-07) uses the
// recommended setup.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLogBuckets_PolicyNotACL(t *testing.T) {
	byDir := map[string][]tfFile{}
	for _, f := range loadTerraform(t, "terraform_modules") {
		byDir[filepath.Dir(f.path)] = append(byDir[filepath.Dir(f.path)], f)
	}
	found := 0
	for dir, module := range byDir {
		if len(blocks(module, "resource", "aws_s3_bucket_logging")) == 0 {
			continue
		}
		found++
		if acls := blocks(module, "resource", "aws_s3_bucket_acl"); len(acls) != 0 {
			t.Errorf("%s: aws_s3_bucket_acl.%s still grants access logging by ACL", dir, acls[0].Labels[1])
		}
		oc := blocks(module, "resource", "aws_s3_bucket_ownership_controls", "s3_logging")
		if len(oc) != 1 {
			t.Errorf("%s: no aws_s3_bucket_ownership_controls.s3_logging", dir)
		} else {
			for _, r := range nested(oc[0], "rule") {
				if v, _ := literal(r.Body.Attributes["object_ownership"]); v != "BucketOwnerEnforced" {
					t.Errorf("%s: s3_logging object_ownership %q, want BucketOwnerEnforced (ACLs disabled)", dir, v)
				}
			}
		}
		var policy string
		for _, p := range blocks(module, "resource", "aws_s3_bucket_policy") {
			if b, ok := p.Body.Attributes["bucket"]; ok && strings.Contains(sourceOf(t, module, b.Expr), "aws_s3_bucket.s3_logging") {
				policy = string(p.Body.SrcRange.SliceBytes(srcOf(t, module, p)))
			}
		}
		if policy == "" {
			t.Errorf("%s: the access-log bucket has no bucket policy", dir)
			continue
		}
		// The statement may live in a policy document or template elsewhere in the module directory.
		var all strings.Builder
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if !e.IsDir() {
				b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
				all.Write(b)
			}
		}
		for _, want := range []string{"logging.s3.amazonaws.com", "s3:PutObject", "aws:SourceAccount"} {
			if !strings.Contains(all.String(), want) {
				t.Errorf("%s: no %q in the module: the log bucket policy must let logging.s3.amazonaws.com PutObject for this account only", dir, want)
			}
		}
	}
	if found == 0 {
		t.Fatal("no module turns on access logging")
	}
}
