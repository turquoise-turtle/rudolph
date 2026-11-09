#
# Top level REST API resource
#
resource "aws_api_gateway_rest_api" "api_gateway" {
  name        = "${var.prefix}_rudolph"
  description = "Rudolph's REST API Gateway"

  # If allowed_inbound_ips are set, then use the API IAM resource policy else leave this to a null resource
  policy = local.rest_api_policy

  endpoint_configuration {
    types = ["REGIONAL"] # FIXME (derek.wang) Switch to PRIVATE later and attach some VPCs
  }

  # Use the authorizer's UsageIdentifierKey to uniquely identify an endpoint.
  api_key_source = "AUTHORIZER"

  # Santa compresses request bodies (Content-Encoding: deflate by default) but still sends
  # Content-Type: application/json. A Lambda proxy integration only passes such bodies through intact when the media
  # type is binary: API Gateway then base64 encodes the raw body (isBase64Encoded = true) and the handlers decode and
  # decompress it.
  binary_media_types = ["application/json"]

  # Clients must use the custom domain; the default endpoint would bypass mTLS.
  disable_execute_api_endpoint = var.disable_execute_api_endpoint || local.mtls_enabled
}

locals {
  mtls_enabled = var.mtls_truststore_uri != ""

  rest_api_policy = length(var.allowed_inbound_ips) != 0 ? data.aws_iam_policy_document.api_resource_policy.json : null
}

##########################
# Stages and Deployments #
##########################

# Before hashicorp/aws 6.x the stage was created implicitly by the deployment's stage_name.
# Existing environments must import it once, see docs/deploy.md.
resource "aws_api_gateway_stage" "stage" {
  stage_name    = var.stage_name
  rest_api_id   = aws_api_gateway_rest_api.api_gateway.id
  deployment_id = aws_api_gateway_deployment.api_deployment.id
}

resource "aws_api_gateway_deployment" "api_deployment" {
  rest_api_id = aws_api_gateway_rest_api.api_gateway.id

  triggers = {
    redeployment = join(",", [
      # REST API settings that need a redeployment, from configuration: the whole resource includes attributes AWS
      # computes after apply, and hashing it replaced the deployment on every apply.
      sha1(jsonencode([
        aws_api_gateway_rest_api.api_gateway.binary_media_types,
        aws_api_gateway_rest_api.api_gateway.api_key_source,
        aws_api_gateway_rest_api.api_gateway.disable_execute_api_endpoint,
        local.rest_api_policy,
      ])),
      # Gateway responses also only reach clients through a new deployment.
      sha1(jsonencode([for response in [
        aws_api_gateway_gateway_response.unauthorized,
        aws_api_gateway_gateway_response.authorization_configuration_error,
        aws_api_gateway_gateway_response.access_denied,
        aws_api_gateway_gateway_response.default_5xx,
        aws_api_gateway_gateway_response.missing_authentication_token,
      ] : [response.response_type, response.status_code, response.response_templates]])),
      # Routes refer to models by name, which stays the same when a model's schema changes.
      sha1(jsonencode([for model in [
        aws_api_gateway_model.machine_config,
        aws_api_gateway_model.ruledownload,
        aws_api_gateway_model.preflight_request,
      ] : [model.name, model.content_type, model.schema]])),
      module.rudolph_api_authorizer.api_gateway_authorizer_sha,
      module.health_api.integration_shas,
      module.ruledownload_api.integration_shas,
      module.ruledownload_resource_api.integration_shas,
      module.preflight_api.integration_shas,
      module.preflight_resource_api.integration_shas,
      module.eventupload_api.integration_shas,
      module.eventupload_resource_api.integration_shas,
      module.xsrf_api.integration_shas,
      module.xsrf_resource_api.integration_shas,
      module.postflight_api.integration_shas,
      module.postflight_resource_api.integration_shas,
    ])
  }

  depends_on = [
    module.health_api.integration_ids,
    module.ruledownload_api.integration_ids,
    module.ruledownload_resource_api.integration_ids,
    module.preflight_api.integration_ids,
    module.preflight_resource_api.integration_ids,
    module.eventupload_api.integration_ids,
    module.eventupload_resource_api.integration_ids,
    module.xsrf_api.integration_ids,
    module.xsrf_resource_api.integration_ids,
    module.postflight_api.integration_ids,
    module.postflight_resource_api.integration_ids,
  ]

  lifecycle {
    create_before_destroy = true
  }
}

# Throttling, so that a runaway client or a flood of requests is answered with 429 by API Gateway instead of running up
# Lambda and DynamoDB costs. method_path "*/*" gives every method (6 routes) its own limit and token bucket; the limits
# are not shared between routes. It is a ceiling against floods, not a way to shed normal load: Santa retries a 429
# within the same sync stage (up to 5 attempts, backing off 4, 8, 16 and 32 seconds), so throttling a busy fleet adds
# requests, and a stage that is still throttled after the last attempt fails the whole sync.
#
# A sync is a preflight, zero or more eventuploads, one or more ruledownload pages and a postflight: roughly 4-6
# requests every 10 minutes per Mac, one or two per route, so ~clients/600 to clients/300 requests per second on each
# route. The default of 2,000/s per route (burst 2,000) leaves room for a fleet of 100,000 Macs (~170-330/s per route)
# with its clean syncs and morning peaks. All routes together can take 12,000/s, above the account's regional limit
# (10,000/s, burst 5,000 by default), so a flood across every route can still throttle other APIs in the account and
# region; the burst is kept at the rate so that a burst on one route cannot take most of the account's burst. The
# api_4xx alarm (monitoring.tf) fires when requests are throttled.
resource "aws_api_gateway_method_settings" "throttling" {
  rest_api_id = aws_api_gateway_rest_api.api_gateway.id
  stage_name  = aws_api_gateway_stage.stage.stage_name
  method_path = "*/*"

  settings {
    throttling_burst_limit = var.api_throttling_burst_limit
    throttling_rate_limit  = var.api_throttling_rate_limit

    # Per-method (detailed) metrics and execution logging are left off: they are billed per metric and per log
    # line, and the stage-level metrics the alarms use are free.
    metrics_enabled = false
    logging_level   = "OFF"
  }
}

# @doc https://www.terraform.io/docs/providers/aws/r/api_gateway_base_path_mapping.html
resource "aws_api_gateway_base_path_mapping" "gateway_custom_domain_mapping" {
  api_id      = aws_api_gateway_rest_api.api_gateway.id
  stage_name  = aws_api_gateway_stage.stage.stage_name
  domain_name = aws_api_gateway_domain_name.api_custom_domain.domain_name
}

