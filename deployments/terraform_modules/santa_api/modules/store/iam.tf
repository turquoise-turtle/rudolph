#
# Policy of the sync Lambdas that read and write the store table (preflight, ruledownload, postflight)
#
data "aws_iam_policy_document" "lambda_permissions" {
  # Point reads, batch reads and partition queries only. No Scan, DescribeTable or index: only the CLI scans the table
  # (repair, group members --rebuild) or queries its indexes (lookup, machines), with the operator's own credentials.
  statement {
    actions = [
      "dynamodb:BatchGetItem",
      "dynamodb:GetItem",
      "dynamodb:Query",
    ]

    # Only this deployment's table. A pattern such as table/*_rudolph_store would let every Rudolph deployment in the
    # account read every other deployment's table.
    resources = [
      aws_dynamodb_table.store.arn,
    ]
  }

  # Allow PutItem, but only on certain key prefixes, for the purposes
  # of uploading new config data
  statement {
    actions = [
      "dynamodb:PutItem",
      "dynamodb:UpdateItem",
      "dynamodb:DeleteItem",
      # ruledownload writes machine rule delivery tracking in batches; LeadingKeys applies to every item of a batch
      "dynamodb:BatchWriteItem",
    ]

    # Writes only ever go to this deployment's table (never to another deployment's table, never to an index).
    resources = [
      aws_dynamodb_table.store.arn,
    ]

    # As an extra precaution, we prevent write operations to be conducted on GLOBAL rules and configurations
    # This prevents bugginess on the main API from propagating bad changes all around
    condition {
      test     = "ForAllValues:StringLike"
      variable = "dynamodb:LeadingKeys"

      values = [
        "Machine#*",             # This needs to be consistent with the machineInfoPKPrefix constant
        "MachineRules#*",        # This needs to be consistent with the MachineRulesPKPrefix constant
        "MachineRuleDelivery#*", # This needs to be consistent with the deliveryPKPrefix constant (machinerules)
        "GroupMembers#*",        # This needs to be consistent with the GroupMembersPKPrefix constant (groups); preflight writes reported members
      ]
    }

    # ForAllValues is also true when the request names no partition at all, so the key must be present
    condition {
      test     = "Null"
      variable = "dynamodb:LeadingKeys"
      values   = ["false"]
    }
  }
}

resource "aws_iam_policy" "lambda_policy" {
  name   = "${var.prefix}_rudolph_store_policy"
  policy = data.aws_iam_policy_document.lambda_permissions.json
}

# Keyed by the caller's fixed names, as the role names are only known after apply. Not count: removing a role from
# the middle of a list renumbered the rest, and replacing an attachment detaches the policy from a role that keeps it.
resource "aws_iam_role_policy_attachment" "lambda_policy" {
  for_each   = var.read_lambda_role_names
  role       = each.value
  policy_arn = aws_iam_policy.lambda_policy.arn
}
