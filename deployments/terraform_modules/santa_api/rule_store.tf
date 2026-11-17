module "rule_store" {
  source = "./modules/store"

  prefix                      = var.prefix
  aws_account_id              = var.aws_account_id
  region                      = var.region
  kms_key_administrators_arns = var.kms_key_administrators_arns
  table_deletion_protection   = var.table_deletion_protection

  # The sync Lambdas that read the store table and write its per-machine partitions. Health has its own, narrower
  # policy below; eventupload has events_write.
  read_lambda_role_names = {
    ruledownload = module.ruledownload_function.lambda_role_name
    preflight    = module.preflight_function.lambda_role_name
    postflight   = module.postflight_function.lambda_role_name
  }

  # Only eventupload writes execution events
  events_write_lambda_role_names = [
    module.eventupload_function.lambda_role_name,
  ]
}

# The attachments were a list with health first. Moving the others to their keys keeps them in place; only health's
# (index 0) is destroyed. Without these, every attachment after health was replaced, and replacing one detaches the
# policy from a role that the attachment before it had just attached it to.
moved {
  from = module.rule_store.aws_iam_role_policy_attachment.lambda_policy[1]
  to   = module.rule_store.aws_iam_role_policy_attachment.lambda_policy["ruledownload"]
}

moved {
  from = module.rule_store.aws_iam_role_policy_attachment.lambda_policy[2]
  to   = module.rule_store.aws_iam_role_policy_attachment.lambda_policy["preflight"]
}

moved {
  from = module.rule_store.aws_iam_role_policy_attachment.lambda_policy[3]
  to   = module.rule_store.aws_iam_role_policy_attachment.lambda_policy["postflight"]
}

# The health check only Queries one global rule (globalrules.PingDatabase): the GlobalRules partition of the store
# table, not an index, nothing else. No KMS statement: the store key's policy lets the account's principals use it
# through DynamoDB.
data "aws_iam_policy_document" "health" {
  statement {
    actions = ["dynamodb:Query"]

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

resource "aws_iam_role_policy" "health" {
  name   = "${var.prefix}_rudolph_health_store_policy"
  role   = module.health_function.lambda_role_name
  policy = data.aws_iam_policy_document.health.json
}
