#
# S3 Bucket for S3 logging, optional
#

resource "aws_s3_bucket" "s3_logging" {
  count = local.create_s3_logging_bucket ? 1 : 0

  bucket = local.s3_logging_bucket_name

  force_destroy = true
}

resource "aws_s3_bucket_policy" "s3_logging" {
  count = local.create_s3_logging_bucket ? 1 : 0

  bucket = aws_s3_bucket.s3_logging[0].id
  policy = format(
    data.aws_iam_policy_document.default_bucket_policy_template.json,
    local.s3_logging_bucket_name,
    local.s3_logging_bucket_name
  )
}

resource "aws_s3_bucket_versioning" "s3_logging" {
  count = local.create_s3_logging_bucket ? 1 : 0

  bucket = aws_s3_bucket.s3_logging[0].id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "s3_logging" {
  count = local.create_s3_logging_bucket ? 1 : 0

  bucket = aws_s3_bucket.s3_logging[0].id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = aws_kms_key.s3_logging[0].key_id
    }
  }
}

# New buckets have ACLs disabled (BucketOwnerEnforced). The log-delivery-write ACL used by S3 access logging
# can only be applied once ownership controls allow ACLs.
resource "aws_s3_bucket_ownership_controls" "s3_logging" {
  count = local.create_s3_logging_bucket ? 1 : 0

  bucket = aws_s3_bucket.s3_logging[0].id
  rule {
    object_ownership = "BucketOwnerPreferred"
  }
}

resource "aws_s3_bucket_acl" "s3_logging" {
  count = local.create_s3_logging_bucket ? 1 : 0

  depends_on = [aws_s3_bucket_ownership_controls.s3_logging]

  bucket = aws_s3_bucket.s3_logging[0].id
  acl    = "log-delivery-write"
}


// KMS Key for S3 server-side encryption
resource "aws_kms_key" "s3_logging" {
  count               = local.create_s3_logging_bucket ? 1 : 0
  enable_key_rotation = true
  description         = "S3 Logging S3 Server-Side Encryption"
}

// KMS Alias for S3 server-side encryption
resource "aws_kms_alias" "s3_logging" {
  count         = local.create_s3_logging_bucket ? 1 : 0
  name          = "alias/${local.s3_logging_bucket_name}-s3-sse"
  target_key_id = aws_kms_key.s3_logging[0].key_id
}
