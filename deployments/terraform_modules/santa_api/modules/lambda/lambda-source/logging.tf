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
  policy = data.aws_iam_policy_document.s3_logging[0].json
}

data "aws_caller_identity" "current" {}

# S3 server access logging is granted by the bucket policy, as AWS recommends, not by the log-delivery-write ACL:
# the bucket has ACLs disabled. The SSL-only statement of the shared template is kept.
data "aws_iam_policy_document" "s3_logging" {
  count = local.create_s3_logging_bucket ? 1 : 0

  source_policy_documents = [
    format(
      data.aws_iam_policy_document.default_bucket_policy_template.json,
      local.s3_logging_bucket_name,
      local.s3_logging_bucket_name
    )
  ]

  statement {
    sid    = "S3ServerAccessLogsPolicy"
    effect = "Allow"

    principals {
      type        = "Service"
      identifiers = ["logging.s3.amazonaws.com"]
    }

    actions   = ["s3:PutObject"]
    resources = ["${aws_s3_bucket.s3_logging[0].arn}/${local.source_bucket_name}/*"]

    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }

    condition {
      test     = "ArnLike"
      variable = "aws:SourceArn"
      values   = [aws_s3_bucket.santa_api_source.arn]
    }
  }
}

resource "aws_s3_bucket_versioning" "s3_logging" {
  count = local.create_s3_logging_bucket ? 1 : 0

  bucket = aws_s3_bucket.s3_logging[0].id
  versioning_configuration {
    status = "Enabled"
  }
}

# S3 server access logs are only delivered to a destination bucket encrypted with SSE-S3, not SSE-KMS.
resource "aws_s3_bucket_server_side_encryption_configuration" "s3_logging" {
  count = local.create_s3_logging_bucket ? 1 : 0

  bucket = aws_s3_bucket.s3_logging[0].id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm = "AES256"
    }
  }
}

# ACLs disabled. Upgrading a deployment that granted log delivery with the log-delivery-write ACL: see "S3 access
# logs granted by bucket policy" in docs/deploy.md (AWS refuses BucketOwnerEnforced while that ACL is in place).
resource "aws_s3_bucket_ownership_controls" "s3_logging" {
  count = local.create_s3_logging_bucket ? 1 : 0

  # On an upgrade the policy grant must be in place before the ACL grant can go
  depends_on = [aws_s3_bucket_policy.s3_logging]

  bucket = aws_s3_bucket.s3_logging[0].id
  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}
