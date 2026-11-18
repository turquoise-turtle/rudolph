// Default firehose bucket policy to use as template
// Enforce Secure Transport
data "aws_iam_policy_document" "firehose_bucket_policy_template" {
  statement {
    sid = ""

    effect = "Deny"

    principals {
      type        = "AWS"
      identifiers = ["*"]
    }

    actions = ["s3:*"]

    resources = [
      "arn:aws:s3:::%s",
      "arn:aws:s3:::%s/*",
    ]

    condition {
      test     = "Bool"
      variable = "aws:SecureTransport"
      values   = ["false"]
    }
  }
}


data "aws_iam_policy_document" "firehose_assume_role_policy" {
  statement {
    sid = ""

    effect = "Allow"

    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["firehose.amazonaws.com"]
    }

    # Only delivery streams of this account may use the role (confused-deputy guard)
    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [var.aws_account_id]
    }
  }
}

data "aws_iam_policy_document" "firehose_s3_policy" {
  statement {
    sid    = ""
    effect = "Allow"

    actions = [
      "s3:AbortMultipartUpload",
      "s3:GetBucketLocation",
      "s3:GetObject",
      "s3:ListBucket",
      "s3:ListBucketMultipartUploads",
      "s3:PutObject",
    ]

    resources = [
      aws_s3_bucket.rudolph_eventsupload_firehose.arn,
      "${aws_s3_bucket.rudolph_eventsupload_firehose.arn}/*",
    ]
  }

  # The bucket's default encryption is SSE-KMS with the module's key. S3 calls KMS on the role's behalf when Firehose
  # writes an object, so the role needs the key only through S3; the key policy delegates to IAM (its root statement).
  statement {
    sid = ""

    effect = "Allow"

    actions = [
      "kms:Encrypt",
      "kms:Decrypt",
      "kms:ReEncrypt*",
      "kms:GenerateDataKey*",
      "kms:DescribeKey"
    ]

    resources = [
      aws_kms_key.rudolph_eventsupload_kms_key.arn,
    ]

    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["s3.${var.region}.amazonaws.com"]
    }

  }
}

resource "aws_iam_policy" "eventsupload_firehose_s3_policy" {
  name   = "${var.prefix}_rudolph_eventupload_firehose_s3_policy"
  policy = data.aws_iam_policy_document.firehose_s3_policy.json
}

resource "aws_iam_role_policy_attachment" "eventsupload_firehose_s3_policy_attachment" {
  role       = aws_iam_role.eventsupload_firehose_role.name
  policy_arn = aws_iam_policy.eventsupload_firehose_s3_policy.arn
}

# Terraform creates the group and the stream Firehose logs to, so the role only writes log events, to its own group
resource "aws_cloudwatch_log_stream" "s3_delivery" {
  name           = "S3Delivery"
  log_group_name = aws_cloudwatch_log_group.eventsupload_firehose.name
}

data "aws_iam_policy_document" "firehose_cloudwatch_policy" {
  statement {
    effect = "Allow"

    actions = [
      "logs:PutLogEvents",
    ]

    resources = [
      "${aws_cloudwatch_log_group.eventsupload_firehose.arn}:*",
    ]
  }
}

resource "aws_iam_policy" "eventsupload_firehose_cloudwatch_policy" {
  name   = "${var.prefix}_rudolph_eventsupload_firehose_cloudwatch_policy"
  policy = data.aws_iam_policy_document.firehose_cloudwatch_policy.json
}

resource "aws_iam_role_policy_attachment" "eventsupload_firehose_cloudwatch_policy_attachment" {
  role       = aws_iam_role.eventsupload_firehose_role.name
  policy_arn = aws_iam_policy.eventsupload_firehose_cloudwatch_policy.arn
}

#
# Policy to be attached to specified Lambda function roles: eventupload only sends batches (pkg/firehose)
#
data "aws_iam_policy_document" "rudolph_firehose_eventupload" {
  statement {
    actions = [
      "firehose:PutRecordBatch",
    ]

    resources = [
      aws_kinesis_firehose_delivery_stream.eventsupload_firehose.arn,
    ]
  }

  # The stream is encrypted with the module's key, and Firehose checks that the caller of PutRecordBatch may itself
  # make and decrypt data keys with it. Only through Firehose, so the role cannot decrypt the bucket's objects.
  statement {
    actions = [
      "kms:Decrypt",
      "kms:GenerateDataKey",
    ]

    resources = [
      aws_kms_key.rudolph_eventsupload_kms_key.arn,
    ]

    condition {
      test     = "StringEquals"
      variable = "kms:ViaService"
      values   = ["firehose.${var.region}.amazonaws.com"]
    }
  }
}

resource "aws_iam_policy" "rudolph_firehose_eventupload" {
  name   = "${var.prefix}_rudolph_firehose_eventupload_policy"
  policy = data.aws_iam_policy_document.rudolph_firehose_eventupload.json
}

resource "aws_iam_role_policy_attachment" "rudolph_firehose_eventupload" {
  count      = length(var.firehose_upload_lambda_role_names)
  role       = element(var.firehose_upload_lambda_role_names, count.index)
  policy_arn = aws_iam_policy.rudolph_firehose_eventupload.arn
}


resource "aws_iam_role" "eventsupload_firehose_role" {
  name               = "${var.prefix}_rudolph_eventsupload_firehose_role"
  path               = "/rudolph/"
  assume_role_policy = data.aws_iam_policy_document.firehose_assume_role_policy.json
}
