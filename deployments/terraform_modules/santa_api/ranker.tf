#
# Ranker: an hourly Lambda that rebuilds the web view's top-blocked and top-allowed lists (snapshots and pointers in
# the events table) from the day counters and machine markers. Code: cmd/ranker, internal/ranker. Layout and cost:
# docs/events-table.md.
#

locals {
  ranker_function_name = "${var.prefix}_rudolph_ranker"
  lambda_ranker_hash   = filebase64sha256(var.lambda_ranker_zip)
}

resource "aws_s3_bucket_object" "ranker_source" {
  bucket = local.lambda_source_bucket
  key    = "rudolph-source-ranker-${filemd5(var.lambda_ranker_zip)}.zip"
  source = var.lambda_ranker_zip
  etag   = filemd5(var.lambda_ranker_zip)
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

resource "aws_iam_role_policy_attachment" "ranker_basic_execution" {
  role       = aws_iam_role.ranker.id
  policy_arn = "arn:aws:iam::aws:policy/service-role/AWSLambdaBasicExecutionRole"
}

# Reads only the day counters and machine markers, writes only snapshots and pointers, on the events table alone (not
# its indexes, not the store table). No Scan: the job reads partitions it can name.
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
  runtime       = "provided.al2"
  architectures = ["arm64"]

  s3_bucket        = aws_s3_bucket_object.ranker_source.bucket
  s3_key           = aws_s3_bucket_object.ranker_source.key
  source_code_hash = local.lambda_ranker_hash
  memory_size      = 256

  # A run reads about 90 day partitions and up to ~4,000 marker partitions, 16 at a time: seconds normally. The
  # margin covers throttling retries; a run that still times out leaves the previous lists in place.
  timeout = 300

  environment {
    variables = {
      REGION               = var.region
      EVENTS_DYNAMODB_NAME = module.rule_store.events_dynamodb_table_name
    }
  }

  depends_on = [aws_cloudwatch_log_group.ranker]
}

resource "aws_cloudwatch_event_rule" "ranker" {
  name                = "${var.prefix}_rudolph_ranker_hourly"
  description         = "Rebuilds Rudolph's top-blocked and top-allowed binary lists for the web view"
  schedule_expression = "rate(1 hour)"
}

resource "aws_cloudwatch_event_target" "ranker" {
  rule = aws_cloudwatch_event_rule.ranker.name
  arn  = aws_lambda_function.ranker.arn
}

resource "aws_lambda_permission" "ranker_schedule" {
  statement_id  = "AllowExecutionFromEventBridge"
  action        = "lambda:InvokeFunction"
  function_name = aws_lambda_function.ranker.function_name
  principal     = "events.amazonaws.com"
  source_arn    = aws_cloudwatch_event_rule.ranker.arn
}

# The next hourly run rebuilds every list anyway, so a failed run is not retried; it still counts in lambda_errors.
resource "aws_lambda_function_event_invoke_config" "ranker" {
  function_name          = aws_lambda_function.ranker.function_name
  maximum_retry_attempts = 0
}
