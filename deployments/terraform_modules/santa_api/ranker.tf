#
# Ranker: a scheduled Lambda (hourly by default, see ranker_schedule_expression) that rebuilds the web view's
# top-blocked, top-allowed and top-unknown lists (snapshots and pointers in the events table) from the day counters and
# machine markers, marking unknown binaries that a global rule in the store table now covers. Code: cmd/ranker,
# internal/ranker. Layout and cost: docs/events-table.md.
#

locals {
  ranker_function_name = "${var.prefix}_rudolph_ranker"
  lambda_ranker_hash   = filebase64sha256(var.lambda_ranker_zip)
}

resource "aws_s3_object" "ranker_source" {
  bucket = local.lambda_source_bucket
  key    = "rudolph-source-ranker-${filemd5(var.lambda_ranker_zip)}.zip"
  source = var.lambda_ranker_zip
  # Not etag: S3's ETag is not the MD5 of a multipart or SSE-KMS upload, so it never matched and every plan re-uploaded.
  source_hash = filemd5(var.lambda_ranker_zip)
}

resource "aws_iam_role" "ranker" {
  name               = "${var.prefix}_rudolph_ranker_role"
  path               = "/rudolph/"
  assume_role_policy = data.aws_iam_policy_document.ranker_assume_role.json
}

data "aws_iam_policy_document" "ranker_assume_role" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
  }
}

# Logs go to the ranker's own group only (not AWSLambdaBasicExecutionRole, which allows every group of the account).
# Terraform creates the group before the function, so Lambda never needs logs:CreateLogGroup.
data "aws_iam_policy_document" "ranker_logs" {
  statement {
    actions = [
      "logs:CreateLogStream",
      "logs:PutLogEvents",
    ]

    resources = [
      "${aws_cloudwatch_log_group.ranker.arn}:*",
    ]
  }
}

resource "aws_iam_role_policy" "ranker_logs" {
  name   = "${var.prefix}_rudolph_ranker_logs"
  role   = aws_iam_role.ranker.id
  policy = data.aws_iam_policy_document.ranker_logs.json
}

# Reads only the day counters and machine markers, writes only snapshots and pointers, on the events table (not its
# indexes); on the store table, only point reads of global rules. No Scan: the job reads items it can name.
data "aws_iam_policy_document" "ranker" {
  statement {
    actions = [
      "dynamodb:Query",
    ]

    resources = [
      module.rule_store.events_dynamodb_table_arn,
    ]

    condition {
      test     = "ForAllValues:StringLike"
      variable = "dynamodb:LeadingKeys"
      values   = ["BinaryDay#*", "BinarySeen#*"]
    }

    condition {
      test     = "Null"
      variable = "dynamodb:LeadingKeys"
      values   = ["false"]
    }
  }

  statement {
    actions = [
      "dynamodb:PutItem",
      "dynamodb:BatchWriteItem",
    ]

    resources = [
      module.rule_store.events_dynamodb_table_arn,
    ]

    condition {
      test     = "ForAllValues:StringLike"
      variable = "dynamodb:LeadingKeys"
      values   = ["TopSnapshot#*", "Top#*"]
    }

    condition {
      test     = "Null"
      variable = "dynamodb:LeadingKeys"
      values   = ["false"]
    }
  }

  # Not the store module's lambda_permissions policy: that one allows Query of every partition. The ranker only
  # asks whether a global rule exists for a binary, signing ID or team ID, so it gets GetItem on that one partition.
  # No KMS statement: the store key's policy lets the account's principals use it through DynamoDB, and names the
  # ranker's role among the Lambdas when store_kms_key_user_arns narrows it.
  statement {
    actions = [
      "dynamodb:GetItem",
    ]

    resources = [
      module.rule_store.dynamodb_table_arn,
    ]

    condition {
      test     = "ForAllValues:StringEquals"
      variable = "dynamodb:LeadingKeys"
      values   = ["GlobalRules"]
    }

    condition {
      test     = "Null"
      variable = "dynamodb:LeadingKeys"
      values   = ["false"]
    }
  }
}

resource "aws_iam_role_policy" "ranker" {
  name   = "${var.prefix}_rudolph_ranker_policy"
  role   = aws_iam_role.ranker.id
  policy = data.aws_iam_policy_document.ranker.json
}

# Managed explicitly so logs expire. Lambda would otherwise create this group itself, without retention.
resource "aws_cloudwatch_log_group" "ranker" {
  name              = "/aws/lambda/${local.ranker_function_name}"
  retention_in_days = var.log_retention_in_days
}

resource "aws_lambda_function" "ranker" {
  function_name = local.ranker_function_name
  role          = aws_iam_role.ranker.arn
  handler       = "bootstrap"
  runtime       = "provided.al2023"
  architectures = ["arm64"]

  s3_bucket        = aws_s3_object.ranker_source.bucket
  s3_key           = aws_s3_object.ranker_source.key
  source_code_hash = local.lambda_ranker_hash
  # A run holds one small total per binary seen in 90 days (about 350 bytes each, at most about 800 with a file name
  # and signing ID of 255 bytes, so ~35 MB for 100,000 binaries), never the counter rows or the markers themselves.
  # 256 MB holds several hundred thousand binaries, far more than a fleet of a few thousand Macs sees in 90 days.
  # Nothing bounds the number of binaries, so each run logs binaries=<n> to compare with the REPORT line's Max Memory
  # Used (docs/events-table.md, "Top binaries"); raise this when that nears the limit.
  memory_size = 256

  # A run reads 90 day partitions and the markers of up to 2,000 binaries (the first 100 rows of 20 lists; in practice
  # far fewer, as the unknown lists mostly repeat binaries already in the blocked and allowed ones), plus up to three
  # rule lookups for each binary in the unknown lists, 16 calls at a time: seconds for a small fleet, a minute or two
  # when popular binaries have markers on 100,000 Macs (~6 MB each). The margin covers throttling retries; a run that
  # still times out leaves the previous lists in place.
  timeout = 300

  environment {
    variables = {
      REGION               = var.region
      EVENTS_DYNAMODB_NAME = module.rule_store.events_dynamodb_table_name
      DYNAMODB_NAME        = local.dynamodb_table_name
    }
  }

  depends_on = [aws_cloudwatch_log_group.ranker]
}

# EventBridge Scheduler rather than an EventBridge rule: rules only know UTC, while a schedule takes a time zone and
# follows its daylight saving, so a run limited to local working hours stays on them all year.
resource "aws_scheduler_schedule" "ranker" {
  # The scheduler role's trust policy names this schedule's ARN: keep the two in step
  name                         = "${var.prefix}_rudolph_ranker"
  description                  = "Rebuilds Rudolph's top-blocked, top-allowed and top-unknown binary lists for the web view"
  schedule_expression          = var.ranker_schedule_expression
  schedule_expression_timezone = var.ranker_schedule_timezone

  flexible_time_window {
    mode = "OFF"
  }

  target {
    arn      = aws_lambda_function.ranker.arn
    role_arn = aws_iam_role.ranker_scheduler.arn

    # The next scheduled run rebuilds every list anyway, so a failed delivery is not retried either.
    retry_policy {
      maximum_retry_attempts = 0
    }
  }
}

resource "aws_iam_role" "ranker_scheduler" {
  name               = "${var.prefix}_rudolph_ranker_scheduler_role"
  path               = "/rudolph/"
  assume_role_policy = data.aws_iam_policy_document.ranker_scheduler_assume_role.json
}

# Only EventBridge Scheduler acting for this account's default schedule group may use the role (confused-deputy
# guard). AWS checks aws:SourceArn against the schedule group's ARN, never the schedule's: a schedule ARN here makes
# Scheduler refuse to create or run the schedule ("must allow AWS EventBridge Scheduler to assume the role"). The
# schedule has no group_name, so it is in the default group.
data "aws_iam_policy_document" "ranker_scheduler_assume_role" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["scheduler.amazonaws.com"]
    }

    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [var.aws_account_id]
    }

    condition {
      test     = "ArnEquals"
      variable = "aws:SourceArn"
      values   = ["arn:aws:scheduler:${var.region}:${var.aws_account_id}:schedule-group/default"]
    }
  }
}

# The schedule invokes the unqualified function, so the unqualified ARN is enough.
data "aws_iam_policy_document" "ranker_scheduler" {
  statement {
    actions   = ["lambda:InvokeFunction"]
    resources = [aws_lambda_function.ranker.arn]
  }
}

resource "aws_iam_role_policy" "ranker_scheduler" {
  name   = "${var.prefix}_rudolph_ranker_scheduler_policy"
  role   = aws_iam_role.ranker_scheduler.id
  policy = data.aws_iam_policy_document.ranker_scheduler.json
}

# The next scheduled run rebuilds every list anyway, so a failed run is not retried; it still counts in lambda_errors.
resource "aws_lambda_function_event_invoke_config" "ranker" {
  function_name          = aws_lambda_function.ranker.function_name
  maximum_retry_attempts = 0
}
