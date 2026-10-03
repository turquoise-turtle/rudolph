#
# CloudWatch alarms for what breaks syncing or loses events.
#
# Kept deliberately small for cost: six alarms on standard metrics (free) plus three metrics extracted from the
# Lambda logs by metric filters (filters are free, each extracted metric is billed as one custom metric). At
# us-east-1 prices that is a few US dollars a month, independent of fleet size. See docs/costs.md.
#
# Every alarm notifies var.alarm_sns_topic_arn when set, both when it fires and when it recovers.
#

locals {
  alarm_actions = var.alarm_sns_topic_arn == "" ? [] : [var.alarm_sns_topic_arn]

  # Custom metrics extracted from the Lambda logs
  alarm_metric_namespace = "Rudolph/${var.prefix}"

  # Handlers that serve API routes. The router logs one line per request with its status, e.g.
  #   POST /eventupload/{machine_id} machine=... status=413 duration=...
  handler_log_groups = {
    health       = module.health_function.log_group_name
    xsrf         = module.xsrf_function.log_group_name
    preflight    = module.preflight_function.log_group_name
    eventupload  = module.eventupload_function.log_group_name
    ruledownload = module.ruledownload_function.log_group_name
    postflight   = module.postflight_function.log_group_name
  }

  all_log_groups = merge(local.handler_log_groups, {
    authorizer = module.rudolph_api_authorizer.log_group_name
  })

  all_function_names = {
    health       = module.health_function.lambda_function_name
    xsrf         = module.xsrf_function.lambda_function_name
    preflight    = module.preflight_function.lambda_function_name
    eventupload  = module.eventupload_function.lambda_function_name
    ruledownload = module.ruledownload_function.lambda_function_name
    postflight   = module.postflight_function.lambda_function_name
    authorizer   = module.rudolph_api_authorizer.lambda_function_name
  }

  # Event sinks are only in use when eventupload forwards events somewhere
  event_sinks_enabled = var.eventupload_handler != "NONE" || var.eventupload_output_lambda_name != ""
}

#
# API Gateway: 5xx responses (Lambda errors, timeouts, authorizer failures, integration failures)
#
resource "aws_cloudwatch_metric_alarm" "api_5xx" {
  alarm_name          = "${var.prefix}_rudolph_api_5xx"
  alarm_description   = "Rudolph's API answered 5xx responses; Santa hosts are failing to sync."
  namespace           = "AWS/ApiGateway"
  metric_name         = "5XXError"
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 5
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"

  dimensions = {
    ApiName = aws_api_gateway_rest_api.api_gateway.name
    Stage   = aws_api_gateway_stage.stage.stage_name
  }

  alarm_actions = local.alarm_actions
  ok_actions    = local.alarm_actions
}

#
# 413 responses: a request body above 6 MiB after decompression. The host never gets that payload through, so it
# keeps failing (and for eventupload, never uploads those events) until someone looks.
#
resource "aws_cloudwatch_log_metric_filter" "request_too_large" {
  for_each = local.handler_log_groups

  name           = "${var.prefix}_rudolph_${each.key}_request_too_large"
  log_group_name = each.value
  pattern        = "\"status=413\""

  metric_transformation {
    name          = "RequestTooLarge"
    namespace     = local.alarm_metric_namespace
    value         = "1"
    default_value = "0"
  }
}

resource "aws_cloudwatch_metric_alarm" "request_too_large" {
  alarm_name          = "${var.prefix}_rudolph_request_too_large"
  alarm_description   = "A Santa host sent a request body above 6 MiB (HTTP 413). It will keep failing to sync or upload events until the cause is fixed."
  namespace           = local.alarm_metric_namespace
  metric_name         = "RequestTooLarge"
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"

  alarm_actions = local.alarm_actions
  ok_actions    = local.alarm_actions

  depends_on = [aws_cloudwatch_log_metric_filter.request_too_large]
}

#
# Lambda errors, across every Rudolph function
#
resource "aws_cloudwatch_metric_alarm" "lambda_errors" {
  alarm_name          = "${var.prefix}_rudolph_lambda_errors"
  alarm_description   = "A Rudolph Lambda function returned an error."
  evaluation_periods  = 1
  threshold           = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"

  metric_query {
    id          = "errors"
    expression  = "SUM(METRICS())"
    label       = "Lambda errors"
    return_data = true
  }

  dynamic "metric_query" {
    for_each = local.all_function_names

    content {
      id = "e_${metric_query.key}"

      metric {
        namespace   = "AWS/Lambda"
        metric_name = "Errors"
        stat        = "Sum"
        period      = 300

        dimensions = {
          FunctionName = metric_query.value
        }
      }
    }
  }

  alarm_actions = local.alarm_actions
  ok_actions    = local.alarm_actions
}

#
# Lambda timeouts. The runtime logs "Task timed out after N seconds" in the function's log group.
#
resource "aws_cloudwatch_log_metric_filter" "lambda_timeouts" {
  for_each = local.all_log_groups

  name           = "${var.prefix}_rudolph_${each.key}_timeouts"
  log_group_name = each.value
  pattern        = "\"Task timed out\""

  metric_transformation {
    name          = "LambdaTimeouts"
    namespace     = local.alarm_metric_namespace
    value         = "1"
    default_value = "0"
  }
}

resource "aws_cloudwatch_metric_alarm" "lambda_timeouts" {
  alarm_name          = "${var.prefix}_rudolph_lambda_timeouts"
  alarm_description   = "A Rudolph Lambda function timed out."
  namespace           = local.alarm_metric_namespace
  metric_name         = "LambdaTimeouts"
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"

  alarm_actions = local.alarm_actions
  ok_actions    = local.alarm_actions

  depends_on = [aws_cloudwatch_log_metric_filter.lambda_timeouts]
}

#
# DynamoDB throttling. The table is on-demand, so throttling means a hot partition or an account limit.
#
resource "aws_cloudwatch_metric_alarm" "dynamodb_throttles" {
  alarm_name          = "${var.prefix}_rudolph_dynamodb_throttles"
  alarm_description   = "Requests to Rudolph's DynamoDB table are being throttled."
  evaluation_periods  = 1
  threshold           = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"

  metric_query {
    id          = "throttles"
    expression  = "reads + writes"
    label       = "DynamoDB throttle events"
    return_data = true
  }

  metric_query {
    id = "reads"

    metric {
      namespace   = "AWS/DynamoDB"
      metric_name = "ReadThrottleEvents"
      stat        = "Sum"
      period      = 300

      dimensions = {
        TableName = module.rule_store.dynamodb_table_name
      }
    }
  }

  metric_query {
    id = "writes"

    metric {
      namespace   = "AWS/DynamoDB"
      metric_name = "WriteThrottleEvents"
      stat        = "Sum"
      period      = 300

      dimensions = {
        TableName = module.rule_store.dynamodb_table_name
      }
    }
  }

  alarm_actions = local.alarm_actions
  ok_actions    = local.alarm_actions
}

#
# Event sink failures: eventupload answers 5xx when Firehose, Kinesis or the output Lambda did not take the events.
# Santa retries the upload, but persistent failures mean events are piling up on hosts or being dropped.
#
resource "aws_cloudwatch_log_metric_filter" "event_sink_failures" {
  count = local.event_sinks_enabled ? 1 : 0

  name           = "${var.prefix}_rudolph_event_sink_failures"
  log_group_name = module.eventupload_function.log_group_name
  pattern        = "?\"status=500\" ?\"status=502\" ?\"status=503\""

  metric_transformation {
    name          = "EventSinkFailures"
    namespace     = local.alarm_metric_namespace
    value         = "1"
    default_value = "0"
  }
}

resource "aws_cloudwatch_metric_alarm" "event_sink_failures" {
  count = local.event_sinks_enabled ? 1 : 0

  alarm_name          = "${var.prefix}_rudolph_event_sink_failures"
  alarm_description   = "eventupload could not forward events to the event sink (Firehose, Kinesis or Lambda); events may be dropped."
  namespace           = local.alarm_metric_namespace
  metric_name         = "EventSinkFailures"
  statistic           = "Sum"
  period              = 300
  evaluation_periods  = 1
  threshold           = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"

  alarm_actions = local.alarm_actions
  ok_actions    = local.alarm_actions

  depends_on = [aws_cloudwatch_log_metric_filter.event_sink_failures]
}
