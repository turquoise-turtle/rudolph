#
# IAM
#

# IAM Role for the API Handler Lambda
resource "aws_iam_role" "api_handler_role" {
  name               = "${var.prefix}_rudolph_${var.endpoint}_role"
  assume_role_policy = data.aws_iam_policy_document.lambda_execution_policy.json
  path               = "/rudolph/"
}

data "aws_iam_policy_document" "lambda_execution_policy" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]

    principals {
      type        = "Service"
      identifiers = ["lambda.amazonaws.com"]
    }
  }
}

# Logs go to the function's own group only. Not AWSLambdaBasicExecutionRole, which allows writing to every log group
# of the account and creating any. Terraform creates the group before the function (lambda.tf), so Lambda never
# needs logs:CreateLogGroup.
data "aws_iam_policy_document" "logs" {
  statement {
    actions = [
      "logs:CreateLogStream",
      "logs:PutLogEvents",
    ]

    resources = [
      "${aws_cloudwatch_log_group.api_handler.arn}:*",
    ]
  }
}

resource "aws_iam_role_policy" "logs" {
  name   = "${var.prefix}_rudolph_${var.endpoint}_logs"
  role   = aws_iam_role.api_handler_role.id
  policy = data.aws_iam_policy_document.logs.json
}
