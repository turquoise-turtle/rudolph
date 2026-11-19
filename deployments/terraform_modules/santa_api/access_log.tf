#
# API Gateway stage access log, optional (enable_api_access_logs)
#
# Requests that API Gateway refuses itself (a token the authorizer rejects, throttling, a WAF block, a missing route)
# never reach a Lambda, so without this log they leave no trace except a count in the 4XXError metric. One JSON line
# per request, billed as CloudWatch Logs ingestion (see docs/deploy.md), so it is off by default.
#
# API Gateway writes access logs with the account's API Gateway CloudWatch role (cloudwatchRoleArn), one setting per
# account and region shared by every REST API there. Without it the stage update fails with "CloudWatch Logs role ARN
# must be set in account settings to enable logging". Check with `aws apigateway get-account`.
#

locals {
  # JSON lines, so CloudWatch Logs Insights can query fields. requestId can be set by the client (a REST API takes it
  # from the x-amzn-requestid header when present), so extendedRequestId, which API Gateway always generates, is logged
  # too and is the one to trust; sourceIp is the connection's address, not X-Forwarded-For. The resource path template
  # is logged rather than the path, so no machine ID is. errorResponseType says why API Gateway answered an error
  # (UNAUTHORIZED, THROTTLED, WAF_FILTERED, ...), which the status alone does not. Only $context.integration.error is
  # used for integration failures: $context.integrationErrorMessage is filled in by HTTP APIs only.
  api_access_log_format = jsonencode({
    requestId         = "$context.requestId"
    extendedRequestId = "$context.extendedRequestId"
    sourceIp          = "$context.identity.sourceIp"
    requestTime       = "$context.requestTime"
    httpMethod        = "$context.httpMethod"
    resourcePath      = "$context.resourcePath"
    status            = "$context.status"
    responseLength    = "$context.responseLength"
    responseLatency   = "$context.responseLatency"
    errorResponseType = "$context.error.responseType"
    errorMessage      = "$context.error.message"
    authorizerError   = "$context.authorizer.error"
    integrationError  = "$context.integration.error"
    wafResponseCode   = "$context.wafResponseCode"
  })
}

resource "aws_cloudwatch_log_group" "api_access" {
  count = var.enable_api_access_logs ? 1 : 0

  name              = "/aws/apigateway/${var.prefix}_rudolph_access"
  retention_in_days = var.log_retention_in_days
}

#
# The account's API Gateway CloudWatch role, optional (manage_api_gateway_account_role)
#
# aws_api_gateway_account replaces the role that every REST API in this account and region logs with, and Terraform
# cannot tell whether another stack set it. It is only managed when asked for explicitly, never by
# enable_api_access_logs alone. Destroying it, by setting the variable back to false or by make destroy while it is
# true, makes hashicorp/aws 6.x reset the account's cloudwatchRoleArn to empty: access and execution logging then stops
# for every REST API in the account and region (and applies that turn logging on fail) until a role is set again.
#

resource "aws_iam_role" "api_gateway_cloudwatch" {
  count = var.manage_api_gateway_account_role ? 1 : 0

  name               = "${var.prefix}_rudolph_apigateway_cloudwatch_role"
  path               = "/rudolph/"
  assume_role_policy = data.aws_iam_policy_document.api_gateway_cloudwatch_assume_role[0].json
}

# No aws:SourceAccount condition: the role serves every API in the account and region, and AWS documents this trust
# policy without one.
data "aws_iam_policy_document" "api_gateway_cloudwatch_assume_role" {
  count = var.manage_api_gateway_account_role ? 1 : 0

  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["apigateway.amazonaws.com"]
    }
  }
}

# The AWS managed policy, rather than one limited to Rudolph's log group: the role is account-wide and other APIs
# log through it too.
resource "aws_iam_role_policy_attachment" "api_gateway_cloudwatch" {
  count = var.manage_api_gateway_account_role ? 1 : 0

  role       = aws_iam_role.api_gateway_cloudwatch[0].name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonAPIGatewayPushToCloudWatchLogs"
}

resource "aws_api_gateway_account" "cloudwatch" {
  count = var.manage_api_gateway_account_role ? 1 : 0

  cloudwatch_role_arn = aws_iam_role.api_gateway_cloudwatch[0].arn

  # API Gateway checks that it can assume the role and use the policy when the setting is saved
  depends_on = [aws_iam_role_policy_attachment.api_gateway_cloudwatch]
}
