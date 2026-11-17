// KMS Key for rudolph_eventsupload S3 server-side encryption
resource "aws_kms_key" "rudolph_eventsupload_kms_key" {
  enable_key_rotation = true
  description         = "Rudolph EventsUpload S3 Server-Side Encryption"
  policy              = data.aws_iam_policy_document.rudolph_eventsupload_kms_key_policy.json
}

data "aws_iam_policy_document" "rudolph_eventsupload_kms_key_policy" {
  # No statement for every principal of the account: the Firehose role's IAM policy grants it the key through S3 only,
  # which the root statement below lets IAM policies do.
  statement {
    sid    = "Allow access for root user"
    effect = "Allow"
    actions = [
      "kms:Create*",
      "kms:Describe*",
      "kms:Enable*",
      "kms:List*",
      "kms:Put*",
      "kms:Update*",
      "kms:Revoke*",
      "kms:Disable*",
      "kms:Get*",
      "kms:Delete*",
      "kms:TagResource",
      "kms:UntagResource",
      "kms:ScheduleKeyDeletion",
      "kms:CancelKeyDeletion",
      "kms:Encrypt",
      "kms:Decrypt",
      "kms:ReEncrypt*",
      "kms:GenerateDataKey*",
      "kms:DescribeKey"
    ]
    resources = ["*"]

    principals {
      type = "AWS"
      identifiers = [
        "arn:aws:iam::${var.aws_account_id}:root",
      ]
    }
  }

  dynamic "statement" {
    for_each = length(var.kms_key_administrators_arns) == 0 ? [] : [1]

    content {
      sid    = "Allow access for Key Administrators"
      effect = "Allow"
      actions = [
        "kms:Create*",
        "kms:Describe*",
        "kms:Enable*",
        "kms:List*",
        "kms:Put*",
        "kms:Update*",
        "kms:Revoke*",
        "kms:Disable*",
        "kms:Get*",
        "kms:Delete*",
        "kms:TagResource",
        "kms:UntagResource",
        "kms:ScheduleKeyDeletion",
        "kms:CancelKeyDeletion",
        "kms:DescribeKey"
      ]
      resources = ["*"]

      # Administrators manage the key; they cannot encrypt, decrypt or make data keys with it
      principals {
        type        = "AWS"
        identifiers = var.kms_key_administrators_arns
      }
    }
  }

}

// KMS Alias for S3 server-side encryption
resource "aws_kms_alias" "rudolph_eventsupload_kms_alias" {
  name          = "alias/${local.source_bucket_name}-s3-sse"
  target_key_id = aws_kms_key.rudolph_eventsupload_kms_key.key_id
}
