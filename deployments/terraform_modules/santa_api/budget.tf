#
# Monthly cost budget for this deployment.
#
# The account may hold resources that are not Rudolph's, so the budget only counts costs tagged with the provider's
# default tag Name = Rudolph. Cost Explorer only sees that tag once Name is activated as a cost allocation tag in the
# Billing console (it can take up to 24 hours to appear); until then the budget sees nothing. Untagged costs, such as
# some data transfer, are not counted either.
#
# AWS Budgets only alert: they never stop or limit spending.
#
resource "aws_budgets_budget" "monthly" {
  count = var.monthly_budget_usd > 0 ? 1 : 0

  name         = "${var.prefix}-rudolph-monthly"
  budget_type  = "COST"
  time_unit    = "MONTHLY"
  limit_amount = tostring(var.monthly_budget_usd)
  limit_unit   = "USD"

  cost_filter {
    name   = "TagKeyValue"
    values = ["user:Name$Rudolph"]
  }

  notification {
    notification_type          = "ACTUAL"
    comparison_operator        = "GREATER_THAN"
    threshold                  = 80
    threshold_type             = "PERCENTAGE"
    subscriber_email_addresses = var.budget_alert_emails
  }

  notification {
    notification_type          = "ACTUAL"
    comparison_operator        = "GREATER_THAN"
    threshold                  = 100
    threshold_type             = "PERCENTAGE"
    subscriber_email_addresses = var.budget_alert_emails
  }

  notification {
    notification_type          = "FORECASTED"
    comparison_operator        = "GREATER_THAN"
    threshold                  = 100
    threshold_type             = "PERCENTAGE"
    subscriber_email_addresses = var.budget_alert_emails
  }

  lifecycle {
    # A cross-variable validation would need Terraform 1.9; this module supports 1.5.
    precondition {
      condition     = length(var.budget_alert_emails) > 0
      error_message = "budget_alert_emails must list at least one address when monthly_budget_usd is above 0."
    }
  }
}
