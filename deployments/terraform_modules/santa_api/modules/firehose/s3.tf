locals {
  source_bucket_name       = "${var.prefix}-${var.org}-rudolph-events"
  s3_logging_bucket_name   = var.existing_logging_bucket_name != "" ? var.existing_logging_bucket_name : "${local.source_bucket_name}-logging"
  create_s3_logging_bucket = var.enable_logging && var.existing_logging_bucket_name == ""
}

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
      data.aws_iam_policy_document.firehose_bucket_policy_template.json,
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
      values   = [aws_s3_bucket.rudolph_eventsupload_firehose.arn]
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

#
# S3 Bucket for firehose
#

resource "aws_s3_bucket" "rudolph_eventsupload_firehose" {
  bucket = local.source_bucket_name

  force_destroy = true


}

# ACLs disabled: Firehose writes with a role of this account, so the bucket owns every object without ACLs
resource "aws_s3_bucket_ownership_controls" "rudolph_eventsupload_firehose" {
  bucket = aws_s3_bucket.rudolph_eventsupload_firehose.id
  rule {
    object_ownership = "BucketOwnerEnforced"
  }
}


resource "aws_s3_bucket_policy" "rudolph_eventsupload_firehose" {
  bucket = aws_s3_bucket.rudolph_eventsupload_firehose.id
  policy = format(
    data.aws_iam_policy_document.firehose_bucket_policy_template.json,
    local.source_bucket_name,
    local.source_bucket_name
  )
}

resource "aws_s3_bucket_versioning" "rudolph_eventsupload_firehose" {
  bucket = aws_s3_bucket.rudolph_eventsupload_firehose.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_server_side_encryption_configuration" "rudolph_eventsupload_firehose" {
  bucket = aws_s3_bucket.rudolph_eventsupload_firehose.id

  rule {
    apply_server_side_encryption_by_default {
      sse_algorithm     = "aws:kms"
      kms_master_key_id = aws_kms_key.rudolph_eventsupload_kms_key.key_id
    }
  }
}

resource "aws_s3_bucket_logging" "rudolph_eventsupload_firehose" {
  count = var.enable_logging ? 1 : 0

  bucket = aws_s3_bucket.rudolph_eventsupload_firehose.id

  target_bucket = local.s3_logging_bucket_name
  target_prefix = "${local.source_bucket_name}/"

  # The bucket policy that lets S3 deliver logs must exist before logging is enabled
  depends_on = [aws_s3_bucket_policy.s3_logging]
}
