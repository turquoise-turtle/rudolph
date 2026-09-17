#
# S3 Bucket for Lambda source
#

locals {
  source_bucket_name       = "${var.prefix}-${var.org}-rudolph-source"
  s3_logging_bucket_name   = var.existing_logging_bucket_name != "" ? var.existing_logging_bucket_name : "${local.source_bucket_name}-logging"
  create_s3_logging_bucket = var.enable_logging && var.existing_logging_bucket_name == ""
}

# The bucket is private: new buckets have ACLs disabled, so no ACL is set.
resource "aws_s3_bucket" "santa_api_source" {
  bucket = local.source_bucket_name

  force_destroy = true
}

resource "aws_s3_bucket_policy" "santa_api_source" {
  bucket = aws_s3_bucket.santa_api_source.id
  policy = format(
    data.aws_iam_policy_document.default_bucket_policy_template.json,
    local.source_bucket_name,
    local.source_bucket_name
  )
}

resource "aws_s3_bucket_versioning" "santa_api_source" {
  bucket = aws_s3_bucket.santa_api_source.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_logging" "santa_api_source" {
  count = var.enable_logging ? 1 : 0

  bucket = aws_s3_bucket.santa_api_source.id

  target_bucket = local.s3_logging_bucket_name
  target_prefix = "${local.source_bucket_name}/"

  # The ACL that lets S3 deliver logs must exist before logging is enabled
  depends_on = [aws_s3_bucket_acl.s3_logging]
}

resource "aws_s3_bucket_server_side_encryption_configuration" "santa_api_source" {
  bucket = aws_s3_bucket.santa_api_source.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = aws_kms_key.santa_api_source.key_id
    }
  }
}

// KMS Key for S3 server-side encryption
resource "aws_kms_key" "santa_api_source" {
  enable_key_rotation = true
  description         = "Rudolph Source S3 Server-Side Encryption"
}

// KMS Alias for S3 server-side encryption
resource "aws_kms_alias" "santa_api_source" {
  name          = "alias/${local.source_bucket_name}-s3-sse"
  target_key_id = aws_kms_key.santa_api_source.key_id
}
