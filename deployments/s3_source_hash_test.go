package deployments

// Found in the first production deployment: the Lambda source zips set etag = filemd5(zip). S3's ETag is not the MD5
// of the object when it was uploaded in parts (the provider does that above 5 MB) or is encrypted with SSE-KMS (the
// source bucket is), so every plan showed the three zips "updated in-place" and every apply uploaded them again.
// source_hash is kept in the state and never compared with S3.

import "testing"

func TestS3Objects_UseSourceHashNotEtag(t *testing.T) {
	files := loadTerraform(t, "terraform_modules")
	objects := blocks(files, "resource", "aws_s3_object")
	if len(objects) == 0 {
		t.Fatal("no aws_s3_object resources found")
	}
	for _, b := range objects {
		name := b.Labels[1]
		if _, ok := b.Body.Attributes["source"]; !ok {
			continue
		}
		if _, ok := b.Body.Attributes["etag"]; ok {
			t.Errorf("aws_s3_object.%s sets etag: S3's ETag of a multipart or SSE-KMS object never equals the file's MD5, so every plan shows a change; use source_hash", name)
		}
		if _, ok := b.Body.Attributes["source_hash"]; !ok {
			t.Errorf("aws_s3_object.%s has no source_hash, so a changed file under the same key would not be uploaded again", name)
		}
	}
}
