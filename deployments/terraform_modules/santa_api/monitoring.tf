#
# CloudWatch alarms for what breaks syncing or loses events.
#
# Kept deliberately small for cost: ten alarms (nine when no event sink is configured). Seven alarm on AWS metrics,
# which are published for free: api_5xx, api_4xx, lambda_errors and the four dynamodb_throttles alarms (reads and
# writes of each table). Three alarm on metrics that metric filters extract from the Lambda logs: request_too_large,
# lambda_timeouts and event_sink_failures (filters are free, each extracted metric is billed as one custom metric).
# Alarms are billed per metric they evaluate: 33 in all, as lambda_errors sums one metric per function (7: the ranker
# included, the authoriser left out), dynamodb_throttles sums each table with its indexes (20: the rule store and its
# five indexes, the events table and its three, for reads and for writes) and api_4xx divides 4xx responses by
# requests (2). At us-east-1 prices that is about 4.20 US dollars a month (3.80 without an event sink), independent of
# fleet size. Keep docs/costs.md and docs/deploy.md in step with this list.
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
    ranker     = aws_cloudwatch_log_group.ranker.name
  })

  # The functions lambda_errors watches. The authoriser is left out: it rejects a wrong token by returning the
  # "Unauthorized" error, which Lambda counts in Errors, so any scanner with a made-up token would fire the alarm.
  # Rejected tokens show in api_4xx instead, and an authoriser that really fails (crashes or times out) makes API Gateway
  # answer 500, which api_5xx covers.
  #
  # Gap: the authoriser also returns "Unauthorized" (HTTP 401, not 500) for configuration failures: no valid token
  # digests configured, malformed digests in sync_token_sha256, or a MethodArn it cannot parse. Every sync is then
  # refused, and only api_4xx sees it, which evaluates only periods with at least api_4xx_alarm_min_requests requests
  # (default 100). Small fleets should lower api_4xx_alarm_min_requests so that this is noticed.
  error_alarm_function_names = {
    health       = module.health_function.lambda_function_name
    xsrf         = module.xsrf_function.lambda_function_name
    preflight    = module.preflight_function.lambda_function_name
    eventupload  = module.eventupload_function.lambda_function_name
    ruledownload = module.ruledownload_function.lambda_function_name
    postflight   = module.postflight_function.lambda_function_name
    ranker       = aws_lambda_function.ranker.function_name
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
# API Gateway: 4xx responses, above all 429 when the stage throttles (see aws_api_gateway_method_settings.throttling).
# Throttled requests never reach Lambda, so this metric is the only place they show up. Santa retries a 429 within the
# sync stage, so sustained throttling means syncs are failing, not just slowing down. The same metric also counts
# 401/403 from the authorizer, which fire this alarm when hosts lose their sync token (a botched token rotation).
#
# It alarms on the fraction of requests answered 4xx, so that the threshold does not depend on the fleet's size, but
# only in 5-minute periods with at least var.api_4xx_alarm_min_requests requests: in a quiet period (a small fleet
# overnight) a handful of scanner requests answered 401 would otherwise be a large fraction. Two periods in a row
# keep a short burst from firing it.
#
resource "aws_cloudwatch_metric_alarm" "api_4xx" {
  alarm_name          = "${var.prefix}_rudolph_api_4xx"
  alarm_description   = "More than 5% of the requests to Rudolph's API were answered 4xx for 10 minutes (counting only periods with at least ${var.api_4xx_alarm_min_requests} requests): throttling (429, raise api_throttling_rate_limit) or rejected sync tokens (401/403)."
  evaluation_periods  = 2
  datapoints_to_alarm = 2
  threshold           = 0.05
  comparison_operator = "GreaterThanThreshold"
  treat_missing_data  = "notBreaching"

  metric_query {
    id          = "fraction"
    expression  = "IF(requests >= ${var.api_4xx_alarm_min_requests}, FILL(errors, 0) / requests, 0)"
    label       = "Fraction of requests answered 4xx"
    return_data = true
  }

  metric_query {
    id = "errors"

    metric {
      namespace   = "AWS/ApiGateway"
      metric_name = "4XXError"
      stat        = "Sum"
      period      = 300

      dimensions = {
        ApiName = aws_api_gateway_rest_api.api_gateway.name
        Stage   = aws_api_gateway_stage.stage.stage_name
      }
    }
  }

  metric_query {
    id = "requests"

    metric {
      namespace   = "AWS/ApiGateway"
      metric_name = "Count"
      stat        = "Sum"
      period      = 300

      dimensions = {
        ApiName = aws_api_gateway_rest_api.api_gateway.name
        Stage   = aws_api_gateway_stage.stage.stage_name
      }
    }
  }

  alarm_actions = local.alarm_actions
  ok_actions    = local.alarm_actions
}

#
# 413 responses: a request body above 6 MiB after decompression. The host never gets that payload through, so it
# keeps failing (and for eventupload, never uploads those events) until someone looks.
#
# Through API Gateway this cannot fire: API Gateway decompresses the body itself and refuses one too large for Lambda
# before any handler runs, so no status=413 line is logged (those requests show in api_4xx or api_5xx instead). It is
# kept for a front end that passes compressed bodies through to the handlers.
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
  alarm_description   = "A handler refused a request body that decompresses to more than 6 MiB (HTTP 413); the Santa host will keep failing to sync or upload events until the cause is fixed. Only fires behind a front end that passes compressed bodies through to the handler: through API Gateway, oversized bodies are refused before the handler runs and show in api_4xx or api_5xx."
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
# Lambda errors, across every Rudolph function except the authoriser
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
    for_each = local.error_alarm_function_names

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
# DynamoDB throttling, on both tables and every global secondary index. The tables are on-demand, so throttling means a
# hot partition or an account limit. Throttling on an index is reported under the GlobalSecondaryIndexName dimension
# (with TableName), so each index needs a metric of its own. One alarm per table and direction keeps each within
# CloudWatch's limit of 10 metrics per metric-math alarm.
#
locals {
  # The index names are written out rather than read from the store module so that an index added there without being
  # added here fails deployments/throttle_alarm_coverage_test.go.
  dynamodb_throttle_tables = {
    store = {
      label   = "rule store"
      table   = module.rule_store.dynamodb_table_name
      indexes = ["SerialNum_DataType_MachineID", "DataType_MachineID", "DataType_SerialKey", "DataType_HostnameKey", "GlobalRulePolicy_SK"]
    }
    events = {
      label   = "events"
      table   = module.rule_store.events_dynamodb_table_name
      indexes = ["Fleet_TS", "Decision_TS", "TeamID_TS"]
    }
  }

  dynamodb_throttle_alarms = merge([
    for key, t in local.dynamodb_throttle_tables : {
      for direction in ["read", "write"] : "${key}_${direction}" => merge(t, { direction = direction })
    }
  ]...)
}

resource "aws_cloudwatch_metric_alarm" "dynamodb_throttles" {
  for_each = local.dynamodb_throttle_alarms

  alarm_name          = "${var.prefix}_rudolph_dynamodb_${each.key}_throttles"
  alarm_description   = "${title(each.value.direction)}s to Rudolph's ${each.value.label} DynamoDB table or its indexes are being throttled."
  evaluation_periods  = 1
  threshold           = 1
  comparison_operator = "GreaterThanOrEqualToThreshold"
  treat_missing_data  = "notBreaching"

  metric_query {
    id          = "throttles"
    expression  = "SUM(METRICS())"
    label       = "DynamoDB ${each.value.direction} throttle events"
    return_data = true
  }

  metric_query {
    id = "table"

    metric {
      namespace   = "AWS/DynamoDB"
      metric_name = each.value.direction == "read" ? "ReadThrottleEvents" : "WriteThrottleEvents"
      stat        = "Sum"
      period      = 300

      dimensions = {
        TableName = each.value.table
      }
    }
  }

  dynamic "metric_query" {
    for_each = each.value.indexes

    content {
      id = "index_${metric_query.key}"

      metric {
        namespace   = "AWS/DynamoDB"
        metric_name = each.value.direction == "read" ? "ReadThrottleEvents" : "WriteThrottleEvents"
        stat        = "Sum"
        period      = 300

        dimensions = {
          TableName                = each.value.table
          GlobalSecondaryIndexName = metric_query.value
        }
      }
    }
  }

  alarm_actions = local.alarm_actions
  ok_actions    = local.alarm_actions

  lifecycle {
    precondition {
      condition     = length(each.value.indexes) + 1 <= 10
      error_message = "CloudWatch evaluates at most 10 metrics in one alarm; split the ${each.value.label} table's ${each.value.direction} throttle alarm."
    }
  }
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
