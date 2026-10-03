#
# Top level REST API resource
#
resource "aws_api_gateway_rest_api" "api_gateway" {
  name        = "${var.prefix}_rudolph"
  description = "Rudolph's REST API Gateway"

  # If allowed_inbound_ips are set, then use the API IAM resource policy else leave this to a null resource
  policy = length(var.allowed_inbound_ips) != 0 ? data.aws_iam_policy_document.api_resource_policy.json : null

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
      sha1(jsonencode(aws_api_gateway_rest_api.api_gateway)),
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

# Stage-wide throttling, so that a misbehaving client or a flood of requests is answered with 429 by API Gateway
# instead of running up Lambda and DynamoDB costs. Santa treats a 429 like any failed sync and retries at its next
# sync interval.
#
# A sync is a preflight, zero or more eventuploads, one or more ruledownload pages and a postflight: roughly 4-6
# requests every 10 minutes per Mac, so ~clients/100 requests per second on average. The defaults (100/s, burst 200)
# cover a fleet of a few thousand Macs with headroom for clean syncs; raise them for larger fleets.
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

