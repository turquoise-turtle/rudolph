# The opt-in features of the Terraform audit each create their resources only when their own switch is on. The Go
# tests in deployments/ read the configuration; this plans it, so an inverted condition, or one widened with || to
# another switch, fails here. Run by TestTFA7_OptionalFeaturesFollowTheirSwitches with a mocked AWS provider: nothing
# is created and no credentials are needed.

mock_provider "aws" {
  # Some policy documents are format() templates with two %s verbs for a bucket name; a random string has none.
  mock_data "aws_iam_policy_document" {
    defaults = {
      json = "{\"Version\":\"2012-10-17\",\"Statement\":[{\"Resource\":[\"arn:aws:s3:::%s\",\"arn:aws:s3:::%s/*\"]}]}"
    }
  }
}

variables {
  prefix         = "tftest"
  org            = "tftest"
  region         = "us-east-1"
  aws_account_id = "123456789012"
  stage_name     = "test"

  # Any readable file: the module only hashes the zips, and the mocked provider uploads nothing.
  lambda_api_zip        = "tests/optional_features.tftest.hcl"
  lambda_authorizer_zip = "tests/optional_features.tftest.hcl"
  lambda_ranker_zip     = "tests/optional_features.tftest.hcl"
  lambda_fleet_zip      = "tests/optional_features.tftest.hcl"

  sync_token_sha256               = ["0000000000000000000000000000000000000000000000000000000000000000"]
  eventupload_handler             = "NONE"
  eventupload_kinesis_name        = ""
  eventupload_autocreate_policies = false
  eventupload_output_lambda_name  = ""
  route53_zone_name               = "example.com"
  use_existing_route53_zone       = false
}

run "defaults_create_none" {
  command = plan

  assert {
    condition     = length(aws_wafv2_web_acl.api) == 0 && length(aws_wafv2_web_acl_association.api) == 0
    error_message = "the WAF is created with enable_waf off"
  }
  assert {
    condition     = length(aws_cloudwatch_log_group.api_access) == 0
    error_message = "the access-log group is created with enable_api_access_logs off"
  }
  assert {
    condition     = length(aws_api_gateway_account.cloudwatch) == 0 && length(aws_iam_role.api_gateway_cloudwatch) == 0 && length(aws_iam_role_policy_attachment.api_gateway_cloudwatch) == 0
    error_message = "the account's API Gateway CloudWatch role is managed with manage_api_gateway_account_role off"
  }
  assert {
    condition     = length(aws_api_gateway_stage.stage.access_log_settings) == 0
    error_message = "the stage has access_log_settings with enable_api_access_logs off"
  }
}

run "waf_alone" {
  command = plan

  variables {
    enable_waf = true
  }

  assert {
    condition     = length(aws_wafv2_web_acl.api) == 1 && length(aws_wafv2_web_acl_association.api) == 1
    error_message = "enable_waf does not create the web ACL and its association with the stage"
  }
  assert {
    condition     = length(aws_cloudwatch_log_group.api_access) == 0 && length(aws_api_gateway_stage.stage.access_log_settings) == 0
    error_message = "enable_waf alone turns on the access log"
  }
  assert {
    condition     = length(aws_api_gateway_account.cloudwatch) == 0 && length(aws_iam_role.api_gateway_cloudwatch) == 0 && length(aws_iam_role_policy_attachment.api_gateway_cloudwatch) == 0
    error_message = "enable_waf alone manages the account's API Gateway CloudWatch role"
  }
}

run "access_logs_alone" {
  command = plan

  variables {
    enable_api_access_logs = true
  }

  assert {
    condition     = length(aws_cloudwatch_log_group.api_access) == 1 && length(aws_api_gateway_stage.stage.access_log_settings) == 1
    error_message = "enable_api_access_logs does not create the log group and the stage's access_log_settings"
  }
  assert {
    condition     = length(aws_api_gateway_account.cloudwatch) == 0 && length(aws_iam_role.api_gateway_cloudwatch) == 0 && length(aws_iam_role_policy_attachment.api_gateway_cloudwatch) == 0
    error_message = "enable_api_access_logs alone manages the account-wide API Gateway CloudWatch role"
  }
  assert {
    condition     = length(aws_wafv2_web_acl.api) == 0 && length(aws_wafv2_web_acl_association.api) == 0
    error_message = "enable_api_access_logs alone creates the WAF"
  }
}

run "account_role_alone" {
  command = plan

  variables {
    manage_api_gateway_account_role = true
  }

  assert {
    condition     = length(aws_api_gateway_account.cloudwatch) == 1 && length(aws_iam_role.api_gateway_cloudwatch) == 1 && length(aws_iam_role_policy_attachment.api_gateway_cloudwatch) == 1
    error_message = "manage_api_gateway_account_role does not create the role, its policy attachment and aws_api_gateway_account"
  }
  assert {
    condition     = length(aws_cloudwatch_log_group.api_access) == 0 && length(aws_api_gateway_stage.stage.access_log_settings) == 0
    error_message = "manage_api_gateway_account_role alone turns on the access log"
  }
  assert {
    condition     = length(aws_wafv2_web_acl.api) == 0 && length(aws_wafv2_web_acl_association.api) == 0
    error_message = "manage_api_gateway_account_role alone creates the WAF"
  }
}

run "all_on" {
  command = plan

  variables {
    enable_waf                      = true
    enable_api_access_logs          = true
    manage_api_gateway_account_role = true
  }

  assert {
    condition     = length(aws_wafv2_web_acl.api) == 1 && length(aws_wafv2_web_acl_association.api) == 1
    error_message = "the WAF is missing with every switch on"
  }
  assert {
    condition     = length(aws_cloudwatch_log_group.api_access) == 1 && length(aws_api_gateway_stage.stage.access_log_settings) == 1
    error_message = "the access log is missing with every switch on"
  }
  assert {
    condition     = length(aws_api_gateway_account.cloudwatch) == 1 && length(aws_iam_role.api_gateway_cloudwatch) == 1 && length(aws_iam_role_policy_attachment.api_gateway_cloudwatch) == 1
    error_message = "the account's API Gateway CloudWatch role is missing with every switch on"
  }
}
