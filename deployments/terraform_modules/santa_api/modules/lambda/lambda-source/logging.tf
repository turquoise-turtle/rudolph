#
# S3 Bucket for S3 logging, optional
#

resource "aws_s3_bucket" "s3_logging" {
  count = local.create_s3_logging_bucket ? 1 : 0

  bucket = local.s3_logging_bucket_name

  force_destroy = true
}

# No public access. The bucket policy grants only the S3 logging service, for this account and the source bucket
# (aws:SourceAccount, aws:SourceArn), which AWS does not count as public, and ACLs are disabled.
resource "aws_s3_bucket_public_access_block" "s3_logging" {
  count = local.create_s3_logging_bucket ? 1 : 0

  bucket = aws_s3_bucket.s3_logging[0].id

  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

resource "aws_s3_bucket_policy" "s3_logging" {
  count = local.create_s3_logging_bucket ? 1 : 0

  bucket = aws_s3_bucket.s3_logging[0].id
  policy = data.aws_iam_policy_document.s3_logging[0].json

  # S3 refuses concurrent changes to a bucket's policy and its public access block
  depends_on = [aws_s3_bucket_public_access_block.s3_logging]
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

# Without expiry the bucket keeps every access log for ever. The bucket is versioned, so expiring a log at
# access_log_retention_days only puts a delete marker on top of it: the data stays as a noncurrent version until that
# expires 3 days later, and the delete marker, left with no versions under it, is removed by the second rule after that.
# A log is therefore really deleted about access_log_retention_days + 3 days after it was written, give or take S3's
# daily lifecycle run. Logs are written once and never replaced, so noncurrent versions only exist because of expiry.
resource "aws_s3_bucket_lifecycle_configuration" "s3_logging" {
  count = local.create_s3_logging_bucket ? 1 : 0

  bucket = aws_s3_bucket.s3_logging[0].id

  rule {
    id     = "expire-access-logs"
    status = "Enabled"

    # Empty: the whole bucket, whatever prefix a source bucket logs under
    filter {}

    expiration {
      days = var.access_log_retention_days
    }

    noncurrent_version_expiration {
      noncurrent_days = 3
    }

    # The parts of an unfinished multipart upload are billed but never listed, so they would otherwise stay unnoticed
    abort_incomplete_multipart_upload {
      days_after_initiation = 7
    }
  }

  # S3 refuses days and expired_object_delete_marker in one expiration, so the delete markers get a rule of their own
  rule {
    id     = "remove-expired-delete-markers"
    status = "Enabled"

    filter {}

    expiration {
      expired_object_delete_marker = true
    }
  }

  # S3 applies noncurrent version rules only once versioning is on
  depends_on = [aws_s3_bucket_versioning.s3_logging]
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
