#
# Fleet job: a scheduled Lambda (once a day by default, see fleet_schedule_expression) that walks every Mac and
# rebuilds the web view's fleet health lists and Lockdown readiness figures (a snapshot and one pointer in the events
# table). Code: cmd/fleet, internal/fleet. What the lists mean, limits and cost: docs/fleet-health.md.
#
# It publishes no metrics and sends no notifications. Its one alarm, fleet_job below, is built from Lambda's own
# metrics, which cost nothing.
#

locals {
  fleet_function_name = "${var.prefix}_rudolph_fleet"
  lambda_fleet_hash   = filebase64sha256(var.lambda_fleet_zip)
}

resource "aws_s3_object" "fleet_source" {
  bucket = local.lambda_source_bucket
  key    = "rudolph-source-fleet-${filemd5(var.lambda_fleet_zip)}.zip"
  source = var.lambda_fleet_zip
  # Not etag: S3's ETag is not the MD5 of a multipart or SSE-KMS upload, so it never matched and every plan re-uploaded.
  source_hash = filemd5(var.lambda_fleet_zip)
}

resource "aws_iam_role" "fleet" {
  name               = "${var.prefix}_rudolph_fleet_role"
  path               = "/rudolph/"
  assume_role_policy = data.aws_iam_policy_document.fleet_assume_role.json
}

data "aws_iam_policy_document" "fleet_assume_role" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
  }
}

# Logs go to the job's own group only (not AWSLambdaBasicExecutionRole, which allows every group of the account).
# Terraform creates the group before the function, so Lambda never needs logs:CreateLogGroup.
data "aws_iam_policy_document" "fleet_logs" {
  statement {
    actions = [
      "logs:CreateLogStream",
      "logs:PutLogEvents",
    ]

    resources = [
      "${aws_cloudwatch_log_group.fleet.arn}:*",
    ]
  }
}

resource "aws_iam_role_policy" "fleet_logs" {
  name   = "${var.prefix}_rudolph_fleet_logs"
  role   = aws_iam_role.fleet.id
  policy = data.aws_iam_policy_document.fleet_logs.json
}

# Only the partitions the job names, and no Scan: it lists the Macs through the sensor data index. Every
# dynamodb:LeadingKeys condition has its Null check beside it: ForAllValues is also true when a request names no
# partition at all. No KMS statement: the store key's policy lets the account's principals use it through DynamoDB, and
# names this role among the Lambdas when store_kms_key_user_arns narrows it.
data "aws_iam_policy_document" "fleet" {
  # Every Mac: the sensor data rows of the index that lists them.
  statement {
    actions = [
      "dynamodb:Query",
    ]

    resources = [
      "${module.rule_store.dynamodb_table_arn}/index/DataType_MachineID",
    ]

    condition {
      test     = "ForAllValues:StringEquals"
      variable = "dynamodb:LeadingKeys"
      values   = ["SensorData"]
    }

    condition {
      test     = "Null"
      variable = "dynamodb:LeadingKeys"
      values   = ["false"]
    }
  }

  # Each Mac's own rows (config, sensor data, groups, sync state); the sort keys of the global rules, to tell which
  # unknown binaries a rule now covers; and the newest row of the rules feed, the time of the last global rule change,
  # so Macs that have not synced since are not judged as rules hash outliers.
  statement {
    actions = [
      "dynamodb:Query",
    ]

    resources = [
      module.rule_store.dynamodb_table_arn,
    ]

    condition {
      test     = "ForAllValues:StringLike"
      variable = "dynamodb:LeadingKeys"
      values   = ["Machine#*", "GlobalRules", "RulesFeed"]
    }

    condition {
      test     = "Null"
      variable = "dynamodb:LeadingKeys"
      values   = ["false"]
    }
  }

  # The three items every Mac is judged against: group registry, global config, and the fleet health setting: the
  # stale threshold with its holiday and the minimum Santa version (rudolph fleet holiday, fleet min-santa-version).
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
      values   = ["GroupRegistry", "GlobalConfig", "FleetHealth"]
    }

    condition {
      test     = "Null"
      variable = "dynamodb:LeadingKeys"
      values   = ["false"]
    }
  }

  # Each Mac's unknown markers, written by eventupload.
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
      values   = ["UnknownSeen#*"]
    }

    condition {
      test     = "Null"
      variable = "dynamodb:LeadingKeys"
      values   = ["false"]
    }
  }

  # The snapshot's rows and its pointer: nothing else in the events table is the job's to write.
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
      values   = ["FleetSnapshot#*", "FleetPointer"]
    }

    condition {
      test     = "Null"
      variable = "dynamodb:LeadingKeys"
      values   = ["false"]
    }
  }
}

resource "aws_iam_role_policy" "fleet" {
  name   = "${var.prefix}_rudolph_fleet_policy"
  role   = aws_iam_role.fleet.id
  policy = data.aws_iam_policy_document.fleet.json
}

# Managed explicitly so logs expire. Lambda would otherwise create this group itself, without retention.
resource "aws_cloudwatch_log_group" "fleet" {
  name              = "/aws/lambda/${local.fleet_function_name}"
  retention_in_days = var.log_retention_in_days
}

resource "aws_lambda_function" "fleet" {
  function_name = local.fleet_function_name
  role          = aws_iam_role.fleet.arn
  handler       = "bootstrap"
  runtime       = "provided.al2023"
  architectures = ["arm64"]

  s3_bucket        = aws_s3_object.fleet_source.bucket
  s3_key           = aws_s3_object.fleet_source.key
  source_code_hash = local.lambda_fleet_hash
  # A run holds every machine ID (about 60 bytes each), the sort keys of the global rules (about 10 MB for 80,000) and,
  # for the rules hash check, a small sample of each Mac that completed a sync in the last day (about 200 bytes). Rows
  # are written as the walk goes, never held. Each run logs macs=<n> rules=<n> to compare with the REPORT line's Max
  # Memory Used (docs/fleet-health.md, "Limits").
  memory_size = 256

  # The Lambda maximum. A run makes about two reads and one write call a Mac, 32 Macs at a time, and paces its rows at
  # 1,000 a second: seconds for 1,000 Macs, a few minutes for 100,000. A run that times out counts in Errors and
  # leaves the previous snapshot in place.
  timeout = 900

  environment {
    variables = {
      REGION               = var.region
      EVENTS_DYNAMODB_NAME = module.rule_store.events_dynamodb_table_name
      DYNAMODB_NAME        = local.dynamodb_table_name
    }
  }

  depends_on = [aws_cloudwatch_log_group.fleet]
}

# EventBridge Scheduler, as for the ranker: a schedule takes a time zone, so a cron() run stays at the same local hour
# all year.
resource "aws_scheduler_schedule" "fleet" {
  # The scheduler role's trust policy names this schedule's ARN: keep the two in step
  name                         = "${var.prefix}_rudolph_fleet"
  description                  = "Rebuilds Rudolph's fleet health lists and Lockdown readiness figures for the web view"
  schedule_expression          = var.fleet_schedule_expression
  schedule_expression_timezone = var.fleet_schedule_timezone

  flexible_time_window {
    mode = "OFF"
  }

  target {
    arn      = aws_lambda_function.fleet.arn
    role_arn = aws_iam_role.fleet_scheduler.arn

    # The next day's run rebuilds everything anyway, so a failed delivery is not retried either.
    retry_policy {
      maximum_retry_attempts = 0
    }
  }
}

resource "aws_iam_role" "fleet_scheduler" {
  name               = "${var.prefix}_rudolph_fleet_scheduler_role"
  path               = "/rudolph/"
  assume_role_policy = data.aws_iam_policy_document.fleet_scheduler_assume_role.json
}

# Only EventBridge Scheduler acting for this account's default schedule group may use the role (confused-deputy
# guard). AWS checks aws:SourceArn against the schedule group's ARN, never the schedule's: a schedule ARN here makes
# Scheduler refuse to create or run the schedule ("must allow AWS EventBridge Scheduler to assume the role"). The
# schedule has no group_name, so it is in the default group.
data "aws_iam_policy_document" "fleet_scheduler_assume_role" {
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
data "aws_iam_policy_document" "fleet_scheduler" {
  statement {
    actions   = ["lambda:InvokeFunction"]
    resources = [aws_lambda_function.fleet.arn]
  }
}

resource "aws_iam_role_policy" "fleet_scheduler" {
  name   = "${var.prefix}_rudolph_fleet_scheduler_policy"
  role   = aws_iam_role.fleet_scheduler.id
  policy = data.aws_iam_policy_document.fleet_scheduler.json
}

# The next day's run rebuilds everything anyway, so a failed run is not retried.
resource "aws_lambda_function_event_invoke_config" "fleet" {
  function_name          = aws_lambda_function.fleet.function_name
  maximum_retry_attempts = 0
}

#
# The job's only alarm: no successful run for fleet_alarm_days days. Successful runs are Invocations minus Errors
# (failures, timeouts and out-of-memory all count in Errors), summed per UTC day; a day on which the job was not
# invoked at all has no data, which FILL and treat_missing_data both read as no successful run. That includes the days
# before the job existed, so a new alarm fires once until the first run is counted. That is deliberate: reading missing
# data as fine would also hide a schedule that never invokes the job, as the scheduler trust bug of upgrade step 31
# did. One failed day does not fire it: the lists are a day older, and the web view shows their age. The job is left
# out of lambda_errors and of the lambda_timeouts filters, so its failure has this one name.
#
# Unlike the other alarms it exists only with an SNS topic to notify.
#
resource "aws_cloudwatch_metric_alarm" "fleet_job" {
  count = var.alarm_sns_topic_arn == "" ? 0 : 1

  alarm_name          = "${var.prefix}_rudolph_fleet_job"
  alarm_description   = "Rudolph's fleet job has not completed a run for ${var.fleet_alarm_days} day(s); the web view's fleet health and Lockdown readiness figures are out of date."
  evaluation_periods  = var.fleet_alarm_days
  datapoints_to_alarm = var.fleet_alarm_days
  threshold           = 1
  comparison_operator = "LessThanThreshold"
  treat_missing_data  = "breaching"

  metric_query {
    id          = "successes"
    expression  = "FILL(invocations, 0) - FILL(errors, 0)"
    label       = "Successful fleet job runs"
    return_data = true
  }

  metric_query {
    id = "invocations"

    metric {
      namespace   = "AWS/Lambda"
      metric_name = "Invocations"
      stat        = "Sum"
      period      = 86400

      dimensions = {
        FunctionName = aws_lambda_function.fleet.function_name
      }
    }
  }

  metric_query {
    id = "errors"

    metric {
      namespace   = "AWS/Lambda"
      metric_name = "Errors"
      stat        = "Sum"
      period      = 86400

      dimensions = {
        FunctionName = aws_lambda_function.fleet.function_name
      }
    }
  }

  alarm_actions = local.alarm_actions
  ok_actions    = local.alarm_actions
}
