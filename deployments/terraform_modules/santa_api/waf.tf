#
# AWS WAF web ACL for the stage, optional (enable_waf)
#
# Two rules only, both of which look at the source address and never at the request: the AWS managed IP reputation
# list (addresses AWS has seen in botnets, scanning and other attacks) and a per-IP rate limit. The core rule set and
# any body size rule are deliberately left out: Santa uploads events as deflated binary bodies, often larger than the
# core rule set's 8 KB body limit, and its other rules would read the compressed bytes as text and block at random.
#
# The rate limit counts per source IP over 5 minutes. A whole fleet behind one office NAT or VPN egress shares one
# address, and each Mac sends about 2-3 requests every 5 minutes (4-6 per 10-minute sync, see the throttling comment in
# main.tf) and more during a clean sync or when the fleet comes online in the morning, so the default is sized for the
# largest fleet behind one address, not for one Mac. A blocked Mac is answered 403 and retries at its next sync.
#
# Billed per web ACL, per rule and per request (see docs/costs.md), so it is off by default.
#

resource "aws_wafv2_web_acl" "api" {
  count = var.enable_waf ? 1 : 0

  name        = "${var.prefix}_rudolph"
  description = "Rudolph's API Gateway stage: IP reputation list and per-IP rate limit"
  scope       = "REGIONAL"

  default_action {
    allow {}
  }

  rule {
    name     = "aws-ip-reputation-list"
    priority = 0

    # none applies the group's own actions (block); count would only measure them
    override_action {
      none {}
    }

    statement {
      managed_rule_group_statement {
        vendor_name = "AWS"
        name        = "AWSManagedRulesAmazonIpReputationList"
      }
    }

    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "${var.prefix}_rudolph_ip_reputation"
      sampled_requests_enabled   = true
    }
  }

  rule {
    name     = "per-ip-rate-limit"
    priority = 1

    action {
      block {}
    }

    statement {
      rate_based_statement {
        limit                 = var.waf_rate_limit_per_5_minutes
        evaluation_window_sec = 300
        aggregate_key_type    = "IP"
      }
    }

    visibility_config {
      cloudwatch_metrics_enabled = true
      metric_name                = "${var.prefix}_rudolph_rate_limit"
      sampled_requests_enabled   = true
    }
  }

  visibility_config {
    cloudwatch_metrics_enabled = true
    metric_name                = "${var.prefix}_rudolph"
    sampled_requests_enabled   = true
  }
}

resource "aws_wafv2_web_acl_association" "api" {
  count = var.enable_waf ? 1 : 0

  resource_arn = aws_api_gateway_stage.stage.arn
  web_acl_arn  = aws_wafv2_web_acl.api[0].arn
}
