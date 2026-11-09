# First production deployment

This runbook takes Rudolph and rudolph-web to their first deployment in a real AWS account, verifies it with one or more test Macs, cleans up the test data and then moves the rest of the fleet onto it. Each step says what to run, what success looks like and what to capture. Run it in order; later steps rely on the rules, groups and events earlier steps create.

The deployment is production from the first apply: there is no separate dev or staging deployment. The checks below run against it with test binaries, a test group and test rules that only ever reach the test Macs, and step 13 removes them again. If the checks turn up a major issue, tear the deployment down (step 15); otherwise migrate the rest of the fleet onto it (step 14).

Nothing in it has been run yet: where it predicts AWS behaviour that the tests cannot show (DynamoDB Local and the static Terraform checks stand in for AWS), the step says so and asks you to record what actually happens. Disagreements between the other docs and the code are listed at the end under [Doc issues found](#doc-issues-found).

**The account is not empty.** It already holds a Route 53 public hosted zone and resources that have nothing to do with Rudolph (for example an EC2 instance), and may hold leftovers from an earlier Rudolph attempt. Nothing in this runbook touches a resource it did not create: Terraform manages only what is in its own state, the read-only checks in 1.3 and 2.9 make sure the names it will use are free, and teardown destroys only the two Rudolph stacks. Every command that writes to AWS is yours to run, from your own Mac.

## 0. Decisions and shell setup

### 0.1 Decide before you start

- **Region.** Any region with Lambda arm64 and API Gateway REST APIs. rudolph-web also needs a `us-east-1` provider alias even without CloudFront.
- **Route 53 hosted zone.** Rudolph always creates an ACM certificate and a custom domain, `<prefix>-rudolph.<zone>`. Use the account's existing public hosted zone (`use_existing_route53_zone = true`): Terraform only adds the certificate validation record and the `<prefix>-rudolph` records to it, and never manages the zone itself. Step 2.9 checks that no record of that name exists yet.
- **Names.** `ENV` (the environment directory) is `prod`: rudolph-web's pages print commands that start with `ENV=prod rudolph`, so the Rudolph environment must have that name. `prefix` (every resource name) can be `prod` too, or another prefix of your choice if `prod` is taken in this account (step 2.9). rudolph-web has its own `prefix` (`WEB_PREFIX` below), which names its Lambda and SSM parameters; it can equal Rudolph's. `prefix` and `org` end up in S3 bucket names (`<prefix>-<org>-rudolph-source`) and a DNS name, so use lower-case letters, digits and dashes only; the bucket name must be globally unique.
- **Cost budgets.** The monthly amounts, in US dollars, at which you want to hear about Rudolph's and rudolph-web's spend, and who receives the alerts (steps 2.6 and 4.2).
- **Alarm notifications.** Whether to create an SNS topic with your email subscribed, so the alarms reach you (step 2.4). Recommended for production.
- **Editing in rudolph-web.** rudolph-web is deployed read-only (step 4.2). Step 10.2 turns its editing on for a test of the forms; decide then whether to leave it on, and who holds `Rudolph.Admin` if you do.
- **Afterwards.** If the checks pass, migrate the rest of the fleet (step 14). If there are major issues, tear down (step 15). Step 12 needs at least 2 to 3 days of Cost Explorer data, which can overlap with the first migration stage.

### 0.2 Shell variables

Run every Rudolph command from the root of your Rudolph checkout: the `rudolph` CLI finds the environment's settings at `deployments/environments/$ENV/config.auto.tfvars.json`, relative to the working directory.

```sh
export AWS_PROFILE=rudolph-prod           # the profile for the production account
export REGION=us-east-1                   # your region
export ENV=prod
export PREFIX=prod                        # = "prefix" in config.auto.tfvars.json
export WEB_PREFIX=prod                    # = "prefix" in rudolph-web's config.auto.tfvars.json
export ORG=yourorg                        # = "org"
export ZONE=example.com                   # = "route53_zone_name", the existing hosted zone
export HOST="${PREFIX}-rudolph.${ZONE}"
export STORE="${PREFIX}_rudolph_store"
export EVENTS="${PREFIX}_rudolph_events"
export CAPTURE_DIR="$HOME/rudolph-first-deploy/$(date +%F)"
mkdir -p "$CAPTURE_DIR"
```

Keep captures in `$CAPTURE_DIR`, outside both repositories. They hold the account ID, machine IDs, serial numbers, user names and file paths: summarise the results elsewhere, do not commit the raw captures. Never write the sync token, the Entra client secret or the cookie key into `$CAPTURE_DIR`.

## 1. Prerequisites

### 1.1 Tools on your admin Mac

| Tool | Version | Check |
|---|---|---|
| Go | 1.27.3 or newer, for both repositories: each `go.mod` has `go 1.27` and `toolchain go1.27.3`, which an older `go` command downloads | `go version` |
| Terraform | 1.10+ (tested with 1.17), provider `hashicorp/aws ~> 6.72` (both repositories' `versions.tf`) | `terraform version` |
| AWS CLI | v2 | `aws --version` |
| Xcode command line tools | for `lipo`, which `scripts/build.sh` uses for the universal CLI | `xcode-select -p` |
| `zip`, `openssl`, `perl`, `curl` | as shipped with macOS | |
| `jq` | optional, for reading JSON output | `jq --version` |

Capture: `{ go version; terraform version; aws --version; sw_vers; } > "$CAPTURE_DIR/01-tools.txt"`.

### 1.2 AWS profile

```sh
aws sts get-caller-identity --region "$REGION" | tee "$CAPTURE_DIR/01-identity.json"
```

Success: the account ID is the production account. The role needs to create IAM roles and policies, Lambda, API Gateway, DynamoDB, KMS keys, S3 buckets, ACM certificates, Route 53 records, CloudWatch alarms, log groups and metric filters, EventBridge Scheduler schedules (`scheduler:CreateSchedule`, `scheduler:GetSchedule`, `scheduler:UpdateSchedule`, `scheduler:DeleteSchedule`, and `iam:PassRole` on the role `prod_rudolph_ranker_scheduler_role` under the path `/rudolph/`), SSM parameters, SNS topics (step 2.4) and AWS Budgets (`budgets:ViewBudget`, `budgets:ModifyBudget`, `budgets:TagResource`, `budgets:UntagResource` and `budgets:ListTagsForResource`, since the provider's default tags apply to the budgets; steps 2.6 and 4.2). Only these deployer credentials need the Budgets permissions; no Rudolph or rudolph-web Lambda does.

### 1.3 Terraform state bucket

The S3 backend bucket must exist before `terraform init`; both repositories share it with different keys.

**If the account already has a state bucket** (for example from an earlier, paused Rudolph attempt), reuse it after checking it:

```sh
export STATE_BUCKET=your-terraform-state-bucket
aws s3api get-bucket-versioning --bucket "$STATE_BUCKET"          # "Status": "Enabled"
aws s3api get-bucket-encryption --bucket "$STATE_BUCKET"          # a default encryption rule (AES256 or aws:kms)
aws s3api get-public-access-block --bucket "$STATE_BUCKET"        # all four settings true
aws s3api list-objects-v2 --bucket "$STATE_BUCKET" --query 'Contents[].[Key,LastModified,Size]' --output table | tee "$CAPTURE_DIR/01-state-objects.txt"
```

- Versioning off: turn it on (`aws s3api put-bucket-versioning --bucket "$STATE_BUCKET" --versioning-configuration Status=Enabled`) before the first apply, so a damaged state can be restored.
- No default encryption or public access not fully blocked: fix it with your usual tooling before storing state in it.
- Choose new state keys that no listed object uses, for example `rudolph-prod.tfstate` for Rudolph and `rudolph-web-prod.tfstate` for rudolph-web. An existing object under the key you pick would make Terraform start from that old state: pick another key rather than reuse or delete it. A state object from the earlier attempt also tells you which resources it may have left behind (step 2.9); leave the object where it is.

**If there is none**, create one with your usual tooling: private, versioned, encrypted, public access blocked.

Capture: the bucket name, the outputs above, the two keys you chose and the KMS key used for the bucket, if any.

### 1.4 Test Macs

- **Main test Mac:** Santa 2026.9 from https://github.com/northpolesec/santa/releases, with its system extension and Full Disk Access approved (MDM: `configs/santa-sysext.mobileconfig` and `configs/santa-tcc.mobileconfig`; on an unmanaged test Mac, approve them in System Settings). Use a Mac you can put into lockdown for half an hour, not your daily machine. If it is already a fleet Mac syncing with another server, note which profile it has now so you can put it back (step 14.4).
- **Optional:** a second Mac on Santa 2026.8, and a macOS 13 Mac on Santa 2025.12 (the only release supported there, with the limits in [configuring Santa](configuring-santa.md)).
- On each, `santactl version` and `santactl status` work.

Capture: `santactl version` and `sw_vers` from each Mac.

## 2. Create the Rudolph environment and build

### 2.1 Create the environment directory

```sh
make init-env
```

Success: `deployments/environments/$ENV/` exists with `main.tf`, `_variables.tf`, `_backend.tf`, `_outputs.tf`, `config.auto.tfvars.json`, `versions.tf` and a `.gitignore` of `*`. The repository's own `.gitignore` also ignores every environment directory except `example`, so the account ID, domain and token digests never reach Git.

### 2.2 Create the sync token

```sh
TOKEN="$(openssl rand -base64 32)"
printf '%s' "$TOKEN" | shasum -a 256 | cut -d ' ' -f 1     # the digest for sync_token_sha256
```

Store `$TOKEN` in your password manager now; it goes into the Santa profile in step 5 and the `curl` checks in step 3. Only the digest goes into Terraform.

### 2.3 Edit `_backend.tf`

Set `bucket` (`$STATE_BUCKET`), `key` (the Rudolph key chosen in 1.3, e.g. `rudolph-prod.tfstate`), `region`, `kms_key_id` (or remove it and `encrypt` if you do not use one) and the `region` of the `provider "aws"` block, which must equal `$REGION`. Keep the `default_tags` (`Name = Rudolph`): the cost budget (step 2.6) and Cost Explorer (step 12.4) use it to tell Rudolph's costs from the rest of the account's.

### 2.4 Alarm notifications

Rudolph's alarms only notify an SNS topic you give it. To be emailed:

```sh
aws sns list-topics --region "$REGION" --query "Topics[?ends_with(TopicArn, ':${PREFIX}-rudolph-alarms')].TopicArn"   # must be empty: see below
TOPIC_ARN="$(aws sns create-topic --region "$REGION" --name "${PREFIX}-rudolph-alarms" --query TopicArn --output text)"
echo "$TOPIC_ARN"
aws sns subscribe --region "$REGION" --topic-arn "$TOPIC_ARN" --protocol email --notification-endpoint admin@example.com
```

`create-topic` does not fail when a topic of that name exists: it silently returns the existing one, with whatever subscriptions it already has. So check first (the first command, also part of 2.9): if a `${PREFIX}-rudolph-alarms` topic is listed, find out what it is (`aws sns list-subscriptions-by-topic`) and either reuse it deliberately or pick another name; either way, record that this runbook did not create it, so teardown leaves it alone.

Confirm the subscription from the email AWS sends (until then nothing is delivered), and put `$TOPIC_ARN` into `alarm_sns_topic_arn` in 2.5. Terraform does not manage this topic, so teardown handles it separately (step 15). To skip notifications, leave `alarm_sns_topic_arn` empty: the alarms still change state in CloudWatch, but nobody hears about it.

Capture: the topic ARN, whether this runbook created it, and that the subscription shows as confirmed (`aws sns list-subscriptions-by-topic --region "$REGION" --topic-arn "$TOPIC_ARN"`).

### 2.5 Edit `config.auto.tfvars.json`

```json
{
  "aws_account_id": "123456789012",
  "org": "yourorg",
  "prefix": "prod",
  "stage_name": "prod",
  "region": "us-east-1",
  "eventupload_handler": "NONE",
  "eventupload_kinesis_name": "",
  "eventupload_autocreate_policies": false,
  "route53_zone_name": "example.com",
  "use_existing_route53_zone": true,
  "eventupload_output_lambda_name": "",
  "allowed_inbound_ips": [],
  "kms_key_administrators_arns": [],
  "sync_token_sha256": ["<digest from 2.2>"],
  "authorizer_result_ttl_in_seconds": 300,
  "mtls_truststore_uri": "",
  "mtls_truststore_version": "",
  "disable_execute_api_endpoint": false,
  "log_retention_in_days": 30,
  "api_throttling_rate_limit": 2000,
  "api_throttling_burst_limit": 2000,
  "alarm_sns_topic_arn": "arn:aws:sns:us-east-1:123456789012:prod-rudolph-alarms",
  "api_4xx_alarm_min_requests": 10,
  "monthly_budget_usd": 50,
  "budget_alert_emails": ["admin@example.com"],
  "table_deletion_protection": true,
  "ranker_schedule_expression": "rate(1 hour)",
  "ranker_schedule_timezone": "UTC"
}
```

- `eventupload_handler` `NONE`: no Firehose or Kinesis sink; the execution events table is filled anyway. The `event_sink_failures` alarm and filter are then not created.
- `use_existing_route53_zone` `true`: the zone already exists in the account. With `false` Terraform would create a second zone of the same name that nothing delegates to.
- `alarm_sns_topic_arn`: the topic from 2.4, or empty.
- `api_4xx_alarm_min_requests` `10` instead of 100, and keep it there. The `api_4xx` alarm only counts 5-minute periods with at least this many requests, and a Mac sends about 2 to 3 requests every 5 minutes, so the default of 100 leaves the alarm off for fleets below about 40 Macs. That alarm is the only one that sees a broken sync token (every sync refused with 401) or throttling. With 10, a fleet of about 4 Macs or more is covered; the price is that a handful of scanner requests in a quiet period (a few 401s among a few dozen requests) can fire it now and then. For a small fleet, an occasional false alarm is the better trade than not hearing about a fleet that cannot sync. Raise it once the fleet is large enough that 100 requests per 5 minutes is normal.
- `monthly_budget_usd` and `budget_alert_emails`: see 2.6. `0` means no budget.
- `table_deletion_protection` `true` (the default): nothing, `terraform destroy` included, can delete the two DynamoDB tables while it is on. Teardown (step 15) turns it off first.
- `ranker_schedule_expression` `rate(1 hour)` and `ranker_schedule_timezone` `UTC` (the defaults): an EventBridge Scheduler schedule runs the ranker every hour, around the clock, in UTC, which steps 9.1 and 10.1 rely on. Keep the defaults for the smoke test. A working-hours schedule such as `cron(0 8-18 ? * MON-FRI *)` in `America/New_York` can follow once the checks have passed (see [Ranker schedule](deploy.md#ranker-schedule)); when you switch, also raise rudolph-web's `top_list_stale_after` (step 4.2) to fit it, or its top list pages warn of a stale list outside those hours.
- `enable_s3_logging` is not in the file and defaults to `true`, which creates the access-log bucket that audit item D3 is about (step 11.2). Leave it on.

### 2.6 Cost budget, before the first apply

The account already has other costs, so Rudolph gets a budget of its own that counts only resources tagged `Name = Rudolph` (rudolph-web gets its own, filtered on `Name = rudolph-web`, in step 4.2):

1. Set `monthly_budget_usd` to the monthly amount you want to hear about (for a small fleet, a few times the figures in [costs.md](costs.md)) and `budget_alert_emails` to who should hear. The apply creates the budget `<prefix>-rudolph-monthly`, which emails them when actual spend passes 80% and 100% of the amount, and when AWS forecasts that the month will end above 100%. Terraform refuses a budget without email addresses.
2. Activate the `Name` cost allocation tag: Billing and Cost Management → Cost allocation tags → User-defined cost allocation tags → `Name` → Activate. A tag only appears there once some resource carries it, so if `Name` is not listed yet, activate it after the first apply. It takes up to 24 hours to take effect, and costs are only tagged from activation onwards: until then the Rudolph budget sees nothing. Cost allocation tags can only be activated in the organisation's management (payer) account: if this account is a member of an AWS Organization, ask whoever runs the management account to activate `Name` there. Activating it once covers both `Name = Rudolph` and `Name = rudolph-web`.
3. Recommended: in the Billing console, also create an account-wide monthly cost budget with email alerts as a backstop for everything else in the account: the other resources (such as the EC2 instance) and any cost that carries no tag. Rudolph and rudolph-web each have their own budget.

**AWS Budgets alert; they never cap or stop spending.** Exceeding a budget changes nothing in the account: the alerts are a prompt to look (Cost Explorer, step 12.4) and act.

Capture: the amount and recipients, a screenshot of the activated tag, and whether you created the account-wide budget.

### 2.7 Check the module outputs

The example `_outputs.tf`, copied by `make init-env`, exports `sync_base_url`, `events_dynamodb_table_arn`, `events_dynamodb_table_name`, `store_kms_key_arn`, `api_gateway_id` and `raw_url`. Check they are all there:

```sh
grep '^output' deployments/environments/$ENV/_outputs.tf
```

If the environment was created before they were added to the example, copy them from `deployments/environments/example/_outputs.tf`.

### 2.8 Check dependencies, test and build

Before the first plan, check both repositories' Go dependencies for known vulnerabilities (rudolph-web is built in step 4.4, but its check belongs here, before anything is deployed):

```sh
make deps-check 2>&1 | tee "$CAPTURE_DIR/02-deps-check.txt"
make -C /path/to/rudolph-web deps-check 2>&1 | tee "$CAPTURE_DIR/02-web-deps-check.txt"
```

Each runs `govulncheck` (the version pinned in its `Makefile`), then lists modules that have a newer version or are deprecated. Success: both print `No vulnerabilities found.` and exit 0. The list of newer or deprecated modules after it is for information only (it includes modules that are never built, such as `github.com/golang/protobuf`) and does not fail the target. If `govulncheck` reports a vulnerability, `make` stops with an error: do not deploy that commit; update the affected module (or the Go toolchain, for a standard library finding), run the tests and the check again, and record what you changed.

Then test and build Rudolph:

```sh
make test 2>&1 | tee "$CAPTURE_DIR/02-test.txt" | tail -5
make build 2>&1 | tee "$CAPTURE_DIR/02-build.txt" | tail -12
ls -l build/package/ ./rudolph
```

Success: tests pass; `build/package/` holds `api_deployment.zip`, `api_authorizer_deployment.zip` and `ranker_deployment.zip`; `./rudolph` is a symlink to the universal macOS CLI. `make deploy` would run the same test and build steps (not `deps-check`); doing them separately lets you look at the plan before applying.

Put the CLI on your `PATH` for this shell, so the commands rudolph-web offers to copy (`ENV=prod rudolph ...`) paste as they are:

```sh
export PATH="$PWD:$PATH"
rudolph --help | head -5
```

### 2.9 Leftovers from an earlier attempt, and the DNS name

Before the first plan, check with read-only commands that nothing in the account already uses the names Terraform will create for this prefix. An earlier attempt may have left resources behind (a destroy that failed half-way, or resources created without state), and Terraform fails with "already exists" when it meets one.

```sh
{
  echo "== DynamoDB tables";      aws dynamodb list-tables --region "$REGION" --query "TableNames[?starts_with(@, '${PREFIX}_rudolph')]"
  echo "== Lambda functions";     aws lambda list-functions --region "$REGION" --query "Functions[?starts_with(FunctionName, '${PREFIX}_rudolph')].FunctionName"
  echo "== Log groups";           aws logs describe-log-groups --region "$REGION" --log-group-name-prefix "/aws/lambda/${PREFIX}_rudolph" --query 'logGroups[].logGroupName'
  echo "== S3 buckets";           aws s3api list-buckets --query "Buckets[?starts_with(Name, '${PREFIX}-${ORG}-rudolph')].Name"
  echo "== KMS aliases";          aws kms list-aliases --region "$REGION" --query "Aliases[?starts_with(AliasName, 'alias/${PREFIX}-')].AliasName"
  echo "== REST APIs";            aws apigateway get-rest-apis --region "$REGION" --query "items[?starts_with(name, '${PREFIX}_rudolph')].[id,name]"
  echo "== API custom domains";   aws apigateway get-domain-names --region "$REGION" --query "items[?domainName=='${HOST}'].domainName"
  echo "== IAM roles";            aws iam list-roles --query "Roles[?starts_with(RoleName, '${PREFIX}_rudolph')].RoleName"
  echo "== IAM policies";         aws iam list-policies --scope Local --query "Policies[?starts_with(PolicyName, '${PREFIX}_rudolph')].PolicyName"
  echo "== CloudWatch alarms";    aws cloudwatch describe-alarms --region "$REGION" --alarm-name-prefix "${PREFIX}_rudolph_" --query 'MetricAlarms[].AlarmName'
  echo "== Scheduler schedules";  aws scheduler list-schedules --region "$REGION" --name-prefix "${PREFIX}_rudolph" --query 'Schedules[].Name'
  echo "== Budgets";              aws budgets describe-budgets --account-id "$(aws sts get-caller-identity --query Account --output text)" --query "Budgets[?BudgetName=='${PREFIX}-rudolph-monthly'].BudgetName"
  echo "== SNS alarm topic";      aws sns list-topics --region "$REGION" --query "Topics[?ends_with(TopicArn, ':${PREFIX}-rudolph-alarms')].TopicArn"
  echo "== rudolph-web Lambda";   aws lambda list-functions --region "$REGION" --query "Functions[?starts_with(FunctionName, '${WEB_PREFIX}_rudolph_web')].FunctionName"
  echo "== rudolph-web logs";     aws logs describe-log-groups --region "$REGION" --log-group-name-prefix "/aws/lambda/${WEB_PREFIX}_rudolph_web" --query 'logGroups[].logGroupName'
  echo "== rudolph-web IAM";      aws iam list-roles --path-prefix /rudolph-web/ --query "Roles[?starts_with(RoleName, '${WEB_PREFIX}_rudolph_web')].RoleName"
  echo "== rudolph-web SSM";      aws ssm get-parameters-by-path --region "$REGION" --path "/${WEB_PREFIX}/rudolph-web" --query 'Parameters[].Name'
  echo "== rudolph-web budget";   aws budgets describe-budgets --account-id "$(aws sts get-caller-identity --query Account --output text)" --query "Budgets[?BudgetName=='${WEB_PREFIX}_rudolph_web_monthly'].BudgetName"
} 2>&1 | tee "$CAPTURE_DIR/02-leftovers.txt"

ZONE_ID="$(aws route53 list-hosted-zones-by-name --dns-name "$ZONE" --query 'HostedZones[0].Id' --output text)"
aws route53 list-resource-record-sets --hosted-zone-id "$ZONE_ID" \
  --query "ResourceRecordSets[?contains(Name, '${PREFIX}-rudolph.')].[Name,Type]" | tee "$CAPTURE_DIR/02-dns.txt"
```

Check that `$ZONE_ID` is the public zone you expect (`aws route53 get-hosted-zone --id "$ZONE_ID"`: `PrivateZone` false, the right name), since the account may hold more than one zone.

Success: every list is empty (`[]`; `describe-budgets` may instead answer `NotFoundException` when the account has no budgets at all), and the zone has no record named `<prefix>-rudolph.<zone>` and no certificate validation record (`_<hash>.<prefix>-rudolph.<zone>`) for it. The KMS alias list may show aliases of other software that start with the prefix; only `alias/<prefix>-santa-rules-tables-sse` and `alias/<prefix>-<org>-rudolph-source-s3-sse` matter.

If anything is listed, do not delete it on the strength of its name alone:

- **Simplest:** pick another prefix (for example `rudolph-prod` or `prod2`), set it in `config.auto.tfvars.json` and `PREFIX` (or, for a rudolph-web leftover or an existing alarm topic, in rudolph-web's config and `WEB_PREFIX`, or another topic name), and run the checks again. `ENV` stays `prod`.
- **Or deal with each one deliberately:** find out what it is and whether anything still uses it (its tags, creation date, the earlier attempt's state object from 1.3, Lambda invocation and DynamoDB request metrics, API Gateway traffic). A leftover from the earlier attempt that nothing uses can be removed by hand, or brought under this deployment with an `import` block (as in [deploy.md](deploy.md#4-import-resources-that-aws-created-implicitly)). A DynamoDB table may hold rules you want to keep: back it up first (`aws dynamodb create-backup`). A KMS key scheduled for deletion keeps its alias until it is deleted; cancel the deletion or pick another prefix.
- **Never delete a resource you cannot account for.** If you are unsure what it is, use another prefix.

Capture: both outputs and what you decided for each item listed.

## 3. Deploy Rudolph

### 3.1 Init

```sh
make tf-init 2>&1 | tee "$CAPTURE_DIR/03-init.txt" | tail -5
```

Success: `Terraform has been successfully initialized!`, provider `hashicorp/aws` 6.x installed, and no existing state found under the new key. If it fails with `AccessControlListNotSupported` on the state bucket, remove the `acl = "private"` line from `_backend.tf` and run it again (record this).

### 3.2 Plan

```sh
make plan 2>&1 | tee "$CAPTURE_DIR/03-plan.txt" | tail -3
grep -E '# module.santa_api.*will be created' "$CAPTURE_DIR/03-plan.txt" | sed 's/ will be created//' | sort > "$CAPTURE_DIR/03-plan-resources.txt"
wc -l < "$CAPTURE_DIR/03-plan-resources.txt"
```

Success: `Plan: N to add, 0 to change, 0 to destroy.` Record N. **A first plan must not change or destroy anything:** any `to change` or `to destroy` means the state key already held state (step 1.3); stop and check.

With the settings in 2.5 (`eventupload_handler` `NONE`, S3 access logging on, a budget above 0, an existing zone, no mTLS), reading `deployments/terraform_modules/santa_api/` gives **N = 148**, all under `module.santa_api`. That figure comes from the code, not from a real plan (no plan has been run against AWS): if N differs, compare `03-plan-resources.txt` with this breakdown and record the difference.

| Part of the module | Resources | Count |
|---|---|---|
| Six API handlers (`health`, `xsrf`, `preflight`, `eventupload`, `ruledownload`, `postflight`) | per function: `aws_lambda_function`, `aws_lambda_alias`, `aws_lambda_permission` (API Gateway), `aws_cloudwatch_log_group`, `aws_iam_role`, `aws_iam_role_policy_attachment` (basic execution) | 36 |
| Authoriser | the same six for `prod_rudolph_authorizer`, plus `aws_api_gateway_authorizer`, its invocation role and that role's inline policy | 9 |
| Ranker (`ranker.tf`) | `aws_s3_object.ranker_source`, function, log group, role, basic execution attachment, role policy, `aws_lambda_function_event_invoke_config`, `aws_scheduler_schedule`, scheduler role and its policy | 10 |
| API resources and methods | 11 `aws_api_gateway_resource` (5 route roots, 5 `{machine_id}` children, `health`); for each of the 6 methods an `aws_api_gateway_method`, `aws_api_gateway_integration` and `aws_api_gateway_method_response` | 29 |
| REST API (`main.tf`, `gateway_response.tf`, `models.tf`) | REST API, stage, deployment, method settings, base path mapping; 5 `aws_api_gateway_gateway_response`; 3 `aws_api_gateway_model` | 13 |
| Domain (`route53.tf`) | ACM certificate, its validation, the validation record, `aws_api_gateway_domain_name`, the `A` alias record | 5 |
| Store (`module.rule_store`) | the two tables, KMS key and alias, `prod_rudolph_store_policy` with 4 attachments (health, ruledownload, preflight, postflight), `prod_rudolph_events_write_policy` with 1 attachment (eventupload) | 11 |
| Lambda source buckets (`module.lambda_source[0]`) | source bucket, its policy, versioning, logging, encryption, KMS key and alias; logging bucket, its policy, versioning, encryption and ownership controls | 12 |
| Lambda zips (`lambda.tf`) | `aws_s3_object.santa_api_source`, `aws_s3_object.santa_api_authorizer_source` | 2 |
| Monitoring | 6 alarms; 6 `*_request_too_large` and 8 `*_timeouts` metric filters | 20 |
| Budget | `aws_budgets_budget.monthly[0]` | 1 |

Check that the plan holds, by name (with `prefix` `prod`):

- **DynamoDB** (`module.rule_store`): `aws_dynamodb_table.store` named `prod_rudolph_store`, `PAY_PER_REQUEST`, TTL on `ExpiresAfter`, point-in-time recovery on, `deletion_protection_enabled = true`, server-side encryption with the store KMS key, and five global secondary indexes, each naming its keys in `key_schema` blocks (no `hash_key` or `range_key` inside an index): `SerialNum_DataType_MachineID`, `DataType_MachineID`, `DataType_SerialKey`, `DataType_HostnameKey`, `GlobalRulePolicy_SK`. `aws_dynamodb_table.events` named `prod_rudolph_events`, same settings (deletion protection included), with three indexes, also in `key_schema` blocks: `Fleet_TS`, `Decision_TS`, `TeamID_TS`. The IAM policy `prod_rudolph_events_write_policy` and its attachment to the eventupload role.
- **KMS:** two keys, both with rotation on: the tables' key (alias `alias/prod-santa-rules-tables-sse`) and the Lambda source bucket's key (alias `alias/prod-<org>-rudolph-source-s3-sse`). The access-log bucket has no key of its own.
- **Lambda:** `prod_rudolph_health`, `_xsrf`, `_preflight`, `_eventupload`, `_ruledownload`, `_postflight` and `_authorizer`, each `provided.al2023` on `arm64` with an alias named after `stage_name`, an `aws_lambda_permission` for API Gateway on that alias, a role under the path `/rudolph/` and a log group with 30 days' retention; `prod_rudolph_ranker` (256 MB, 300 s timeout) with its own role and policy, log group and `aws_lambda_function_event_invoke_config` with 0 retries. The eventupload function's environment has `EVENTS_DYNAMODB_NAME` and `DYNAMODB_NAME`.
- **EventBridge Scheduler:** `aws_scheduler_schedule.ranker` named `prod_rudolph_ranker` with `schedule_expression = "rate(1 hour)"`, `schedule_expression_timezone = "UTC"`, `flexible_time_window` mode `OFF`, and a target of the `prod_rudolph_ranker` function ARN with `maximum_retry_attempts = 0` and the role `prod_rudolph_ranker_scheduler_role` (path `/rudolph/`, trusted by `scheduler.amazonaws.com` for this account only), whose policy `prod_rudolph_ranker_scheduler_policy` allows only `lambda:InvokeFunction` on that function. No `aws_cloudwatch_event_rule` or `aws_cloudwatch_event_target`, and no `aws_lambda_permission` for the ranker: the schedule invokes it through its role (each API handler and the authoriser still have their own permission for API Gateway).
- **API Gateway:** REST API `prod_rudolph` with `binary_media_types = ["application/json"]` and `api_key_source = "AUTHORIZER"`; the stage `prod`; a deployment; `aws_api_gateway_method_settings.throttling` (`*/*`, 2000/2000, metrics and logging off); a `REQUEST` authorizer with identity source `method.request.header.Authorization`; `GET /health` and `POST /{preflight,eventupload,ruledownload,postflight,xsrf}/{machine_id}`; the custom domain `prod-rudolph.<zone>` with `TLS_1_2`, its base path mapping, the ACM certificate and its DNS validation, and two Route 53 records (the validation record and the `A` alias).
- **S3:** `prod-<org>-rudolph-source` (versioning, SSE-KMS, SSL-only policy, access logging) with three `aws_s3_object` resources (API, authoriser and ranker zips, keys `rudolph-source-<MD5>.zip`); `prod-<org>-rudolph-source-logging` with versioning, SSE-S3 default encryption (`sse_algorithm = "AES256"`, no KMS key), `aws_s3_bucket_ownership_controls` (`BucketOwnerEnforced`, ACLs disabled) and a bucket policy that lets `logging.s3.amazonaws.com` write the access logs. No `aws_s3_bucket_acl` and no `aws_s3_bucket_object` anywhere.
- **Alarms:** six, `prod_rudolph_api_5xx`, `_api_4xx`, `_request_too_large`, `_lambda_errors`, `_lambda_timeouts`, `_dynamodb_throttles`, each with `alarm_actions` and `ok_actions` set to the topic from 2.4; metric filters `*_request_too_large` on the six handler log groups and `*_timeouts` on all eight log groups.
- **Budget:** `aws_budgets_budget.monthly[0]` named `prod-rudolph-monthly`, `COST`, `MONTHLY`, the amount from 2.5, a `TagKeyValue` cost filter of `user:Name$Rudolph` and three notifications to `budget_alert_emails`.

And that it does **not** hold: anything Firehose or Kinesis, `event_sink_failures`, an `aws_route53_zone` (with `use_existing_route53_zone = true`), or any resource that is not in the `module.santa_api` tree. The module's `removed` blocks for the old `aws_s3_bucket_object` zips only matter to deployments that had them; on a new state they do nothing.

Capture: the plan file, N, and any warnings at the end of the plan. None are expected: the module uses no deprecated resources or arguments, and `terraform validate` on the example environment prints none. Record the text of any warning.

### 3.3 Apply

```sh
make justdeploy 2>&1 | tee "$CAPTURE_DIR/03-apply.txt"
```

Review the plan it prints, type `yes`. The ACM validation waits for DNS; allow 5 to 15 minutes.

Success: `Apply complete! Resources: N added, 0 changed, 0 destroyed.` and outputs including `sync_base_url = "https://prod-rudolph.<zone>/"`.

Capture: the full apply output, its duration, and the outputs:

```sh
terraform -chdir=deployments/environments/$ENV output | tee "$CAPTURE_DIR/03-outputs.txt"
```

Watch for, and record if they happen: an error creating Lambda functions with runtime `provided.al2023`, an error on the log bucket's ownership controls or bucket policy (audit D3), or the custom domain timing out on certificate validation. If the `Name` cost allocation tag was not listed in 2.6, activate it now.

### 3.4 First requests

```sh
curl -s -o /dev/null -w '%{http_code}\n' "https://$HOST/health"                                   # 401: no token
curl -s -w '\n%{http_code}\n' -H "Authorization: Bearer $TOKEN" "https://$HOST/health"            # {"status":"healthy"} 200
curl -s -w '\n%{http_code}\n' -H "Authorization: Bearer wrong" "https://$HOST/health"             # 401
```

Success: `401`, then `{"status":"healthy"}` with `200`, then `401`. The first is answered by API Gateway without invoking the authoriser; the third invokes it, and the authoriser answers by returning the `Unauthorized` error. Note the time of the third request: step 11.3 checks that it did not fire `lambda_errors`.

Capture: the three outputs, and the log lines:

```sh
aws logs tail "/aws/lambda/${PREFIX}_rudolph_health" --since 10m --region "$REGION" | tee "$CAPTURE_DIR/03-health-log.txt"
```

Expect `GET /health machine=- status=200 duration=...` and the Lambda `REPORT` line; no request headers, no token.

### 3.5 Compressed request bodies and the direction of `binary_media_types`

`binary_media_types = ["application/json"]` makes API Gateway hand every `application/json` request body to Lambda base64 encoded, which is what lets Santa's deflate-compressed bodies through. Responses are not base64 encoded by the handlers, so they must still arrive as plain JSON. This step checks both directions before a Mac depends on them. It writes the sensor data row of a made-up machine, which shows up in rudolph-web as `smoke-curl` (test data: step 13.6 removes it).

```sh
CURL_MID="$(uuidgen)"; echo "$CURL_MID" > "$CAPTURE_DIR/03-curl-machine-id.txt"
BODY='{"serial_num":"SMOKECURL01","hostname":"smoke-curl","santa_version":"2026.9.0","os_version":"15.0","os_build":"24A335","client_mode":"MONITOR"}'

# 1. zlib ("deflate"), Santa's default
printf '%s' "$BODY" | perl -MCompress::Zlib -0777 -ne 'print compress($_)' |
  curl -s -w '\n%{http_code} %{content_type}\n' -X POST "https://$HOST/preflight/$CURL_MID" \
    -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -H 'Content-Encoding: deflate' \
    -H 'Accept: application/json' --data-binary @- | tee "$CAPTURE_DIR/03-preflight-deflate.txt"

# 2. gzip
printf '%s' "$BODY" | gzip -c |
  curl -s -w '\n%{http_code}\n' -X POST "https://$HOST/preflight/$CURL_MID" \
    -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -H 'Content-Encoding: gzip' \
    --data-binary @- | tee "$CAPTURE_DIR/03-preflight-gzip.txt"

# 3. uncompressed, with Accept: */*
curl -s -w '\n%{http_code}\n' -X POST "https://$HOST/preflight/$CURL_MID" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -H 'Accept: */*' \
  --data-binary "$BODY" | tee "$CAPTURE_DIR/03-preflight-plain.txt"

# 4. wrong media type
curl -s -o /dev/null -w '%{http_code}\n' -X POST "https://$HOST/preflight/$CURL_MID" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: text/plain' --data-binary "$BODY"
```

Success:

1. to 3. `200`, and a body that starts with `{` and is readable JSON (not base64 such as `eyJ...`), containing `"client_mode":"MONITOR"`, `"sync_type":"clean"` (this machine reports no rules, so every preflight is a clean sync), and `"allowed_path_regex":""` and `"blocked_path_regex":""` (Santa 2026.9 gets empty strings).
4. `415`.

If 1. answers 400, request bodies are not reaching the handler intact; if 1. to 3. answer 200 with a base64 body, the response direction is wrong and Santa will fail to parse every response. Either one stops the deployment: record it and go to [If something fails](#if-something-fails); if it cannot be fixed, tear down (step 15).

Capture: the four results and the preflight log lines (`aws logs tail "/aws/lambda/${PREFIX}_rudolph_preflight" --since 10m --region "$REGION"`).

## 4. rudolph-web

Deploying the web view now lets every later step check its pages as well. Its repository is separate; run these commands in your rudolph-web checkout.

### 4.1 Entra ID app registration

In Microsoft Entra admin centre → App registrations → New registration (details in rudolph-web's `docs/deploy.md`):

1. Single tenant. Platform Web. Leave the redirect URI for now (the Function URL exists only after the first apply), or add `http://localhost:8080/auth/callback`.
2. Certificates & secrets → New client secret. Copy the value into your password manager; it is shown once.
3. App roles → Create app role, twice, allowed member types Users/Groups, values exactly `Rudolph.Viewer` and `Rudolph.Admin`.
4. Enterprise applications → the app → Properties → Assignment required: Yes. Users and groups: assign `Rudolph.Viewer` to one test account and `Rudolph.Admin` to another (or to yourself, changing it between sign-ins; role changes apply at the next sign-in).

Capture: tenant ID, client ID, the two role values as created (screenshot), which account holds which role.

### 4.2 Environment

```sh
cd /path/to/rudolph-web
export ENV=prod                     # the rudolph-web environment
make init-env
```

Edit `deployments/environments/prod/_backend.tf` (state bucket, the rudolph-web key chosen in 1.3 such as `rudolph-web-prod.tfstate`, region; keep the `aws.us_east_1` provider alias) and `config.auto.tfvars.json`:

```json
{
  "aws_account_id": "123456789012",
  "region": "us-east-1",
  "prefix": "prod",
  "tenant_id": "<tenant GUID>",
  "client_id": "<application (client) ID>",
  "base_url": "",
  "post_logout_redirect_url": "",
  "session_ttl": "1h",
  "cli_env": "prod",
  "top_list_stale_after": "3h",
  "create_ssm_parameters": false,
  "ssm_kms_key_arn": "",
  "dynamodb_table_name": "prod_rudolph_store",
  "dynamodb_kms_key_arn": "",
  "events_dynamodb_table_name": "prod_rudolph_events",
  "enable_editing": false,
  "lambda_memory_size": 256,
  "lambda_timeout_seconds": 10,
  "log_retention_in_days": 30,
  "enable_cloudfront": false,
  "domain_name": "",
  "acm_certificate_arn": "",
  "route53_zone_name": "",
  "use_existing_route53_zone": true,
  "cloudfront_price_class": "PriceClass_100",
  "monthly_budget_usd": 20,
  "budget_alert_emails": ["admin@example.com"],
  "tags": {}
}
```

- `prefix` is `$WEB_PREFIX`.
The keys are those of rudolph-web's example `config.auto.tfvars.json`, in the same order; its environment README documents each.

- `cli_env` is the **Rudolph** environment name, `prod`, so the pages' commands start with `ENV=prod rudolph`.
- `top_list_stale_after` `3h` (the default, whole hours from `1h` to `168h`): the top list pages warn when the list is older than this. It suits the hourly ranker from 2.5; if you move the ranker to a working-hours schedule later, raise it to fit (rudolph-web's `docs/deploy.md` gives figures for a weekday schedule).
- `enable_editing` `false`: deploy read-only first. The pages only show data and copy-and-paste CLI commands, the write routes do not exist (404), and the role has no write action. Step 10.2 turns editing on to test it.
- `monthly_budget_usd` and `budget_alert_emails`: rudolph-web's own budget, `<web prefix>_rudolph_web_monthly`, which counts only costs tagged `Name = rudolph-web` (the default tag in rudolph-web's `_backend.tf`; keep it, and do not override `Name` through `tags`). Like Rudolph's (2.6) it needs the `Name` cost allocation tag active, alerts at its thresholds, and never stops spending. `0` creates no budget.
- `dynamodb_table_name` and `events_dynamodb_table_name` follow Rudolph's prefix: change them if you chose another prefix in 2.9.
- `dynamodb_kms_key_arn` is left empty on purpose: Rudolph's key policy lets any principal in the account use the key through DynamoDB, so the web role should need no KMS grant. Step 4.5 checks this; if the pages fail with a KMS `AccessDenied`, set it to the `store_kms_key_arn` output from 3.3 and apply again.

### 4.3 SSM parameters

The names default to `/<web prefix>/rudolph-web/cookie-key` and `/<web prefix>/rudolph-web/client-secret`. Step 2.9 checked that neither exists yet.

```sh
aws ssm put-parameter --region "$REGION" --name "/${WEB_PREFIX}/rudolph-web/cookie-key" \
  --type SecureString --value "$(openssl rand -base64 32)"
aws ssm put-parameter --region "$REGION" --name "/${WEB_PREFIX}/rudolph-web/client-secret" \
  --type SecureString --value "$(cat)"     # paste the Entra client secret, then ctrl-D
```

Success: both commands print a `Version` of 1. (No `--overwrite`: if a parameter of that name exists, the command fails rather than replace it.)

### 4.4 Deploy (two applies without CloudFront)

```sh
make test 2>&1 | tail -3
make build 2>&1 | tail -3                        # writes build/package/rudolph_web_deployment.zip, which plan and apply pass as lambda_zip
make tf-init 2>&1 | tail -3
make plan 2>&1 | tee "$CAPTURE_DIR/04-web-plan.txt" | tail -3
make justdeploy 2>&1 | tee "$CAPTURE_DIR/04-web-apply-1.txt" | tail -12
```

`make plan` and `make justdeploy` pass `lambda_zip` from `build/package/`, so build and init must come first. (`make deploy` runs test, build, tf-init and justdeploy in one go, but rebuilds the zip, so the plan it shows can differ from the one you reviewed.)

The plan should create `<web prefix>_rudolph_web` (arm64, `provided.al2023`, 256 MB, 10 s), its Function URL (`authorization_type = NONE`), its log group, and a role whose inline policy has `WriteOwnLogs`, `ReadSecrets`, `DecryptSecrets`, `ReadRudolphStore` and `ReadRudolphEvents`, with `dynamodb:GetItem` and `dynamodb:Query` only, on the two tables and their `index/*`. No `dynamodb:Scan`, no write action, no `WriteRudolphRules` or `AppendRudolphAudit` statement (those come with `enable_editing`, step 10.2). The function's environment has `RUDOLPH_WEB_EDITING` `false`, `RUDOLPH_WEB_TOP_LIST_STALE_AFTER` `3h` and `RUDOLPH_WEB_CLI_ENV` `prod`. With `monthly_budget_usd` above 0 it also creates the budget `<web prefix>_rudolph_web_monthly`. That is 6 resources under `module.rudolph_web` (function, Function URL, log group, role, role policy, budget; reading the module, as in 3.2): nothing CloudFront, ACM, Route 53 or SSM with the settings above. Like Rudolph's, it must add only and change or destroy nothing, and print no warnings.

After the first apply: put the `function_url` output, **without the trailing slash**, into `base_url`, register `<function_url>auth/callback` (the `redirect_url` output after the second apply) as a Web redirect URI on the app registration, and run `make justdeploy` again against the same build (`04-web-apply-2.txt`). Until the second apply every request answers 500 and the log says `config: RUDOLPH_WEB_REDIRECT_URL is required`; that is expected.

Capture: plan, both apply outputs, `url` and `redirect_url` outputs.

### 4.5 First sign-in

Open the `url` output in a private window, sign in as the **viewer** account.

Success: redirected to Entra, back to the machines page, which lists `smoke-curl` (serial `SMOKECURL01`). No 500 or 503.

Capture: screenshot; and the function's log:

```sh
aws logs tail "/aws/lambda/${WEB_PREFIX}_rudolph_web" --since 15m --region "$REGION" | tee "$CAPTURE_DIR/04-web-log.txt"
```

The whole page walk-through is step 10, once there is data to show. Back in the Rudolph checkout, restore `ENV`:

```sh
cd /path/to/rudolph && export ENV=prod
```

## 5. Santa configuration profile and first sync

### 5.1 Machine ID

On the main test Mac:

```sh
MID="$(uuidgen)"; echo "$MID"
sudo defaults write /Library/Preferences/com.northpolesec.santa.machine-mapping MachineUUID -string "$MID"
```

This is the file the example profile's `MachineIDPlist` names; `rudolph info` reads it (step 5.3). If the Mac already has a machine ID there (from your fleet's existing setup), keep it and use that as `$MID` instead. Record `$MID` in `$CAPTURE_DIR/05-machine-ids.txt` on your admin Mac and `export MID=...` there too.

### 5.2 Test configuration profile

Write this outside both repositories (it holds the token), fill in the values, and install it: through your MDM scoped to the test Mac only, or on an unmanaged test Mac by opening it and approving it in System Settings → General → Device Management. It is a test profile: step 14 replaces it with the fleet profile.

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>PayloadContent</key>
  <array>
    <dict>
      <key>PayloadType</key>
      <string>com.northpolesec.santa</string>
      <key>PayloadIdentifier</key>
      <string>com.northpolesec.santa.rudolph-test</string>
      <key>PayloadUUID</key>
      <string>REPLACE-WITH-uuidgen-1</string>
      <key>PayloadVersion</key>
      <integer>1</integer>
      <key>ClientMode</key>
      <integer>1</integer>
      <key>SyncBaseURL</key>
      <string>https://prod-rudolph.example.com/</string>
      <key>SyncExtraHeaders</key>
      <dict>
        <key>Authorization</key>
        <string>Bearer REPLACE-WITH-TOKEN</string>
      </dict>
      <key>MachineIDPlist</key>
      <string>/Library/Preferences/com.northpolesec.santa.machine-mapping.plist</string>
      <key>MachineIDKey</key>
      <string>MachineUUID</string>
      <key>MachineOwnerGroups</key>
      <array>
        <string>smoke</string>
      </array>
    </dict>
  </array>
  <key>PayloadDisplayName</key>
  <string>Santa (Rudolph test)</string>
  <key>PayloadIdentifier</key>
  <string>com.example.santa.rudolph-test</string>
  <key>PayloadType</key>
  <string>Configuration</string>
  <key>PayloadUUID</key>
  <string>REPLACE-WITH-uuidgen-2</string>
  <key>PayloadVersion</key>
  <integer>1</integer>
</dict>
</plist>
```

- `SyncBaseURL` is the `sync_base_url` output, with its trailing slash.
- `SyncExtraHeaders` carries the token: exactly `Bearer ` followed by the token whose digest is in `sync_token_sha256`.
- `MachineOwnerGroups` reports the test group `smoke`, used in step 6.4. Rudolph ignores it until the group is registered, and again once step 13 deletes the group.
- `ClientMode` `1` starts the Mac in monitor mode.
- Leave out `SyncClientContentEncoding` (deflate, checked in 3.5) and `SyncEnableProtoTransfer`.
- **`SyncEnableCleanSyncEventUpload`** (the exact key Santa reads) is left out. Santa uploads no events during a clean sync unless it is true, and a Mac that holds no rules is clean synced on every sync. Set it to `<true/>` only for a fleet that will run without any rules; this runbook adds rules in step 6, after which syncs become incremental and events are uploaded.
- If the Mac already has a Santa profile from your current sync server, remove it from the Mac's MDM scope while this one is installed: two profiles setting `SyncBaseURL` conflict. Step 14.4 puts it back if needed.

The payload above uses the `com.northpolesec.santa` domain that Santa 2026.8 and newer read. `configs/santa-configuration.mobileconfig` in this repository uses the same domain, token header and machine-mapping path, so it is the starting point for the fleet profile in step 14; the payload above is trimmed to what the checks need and adds `MachineOwnerGroups`. For a managed Mac, `configs/santa-sysext.mobileconfig` and `configs/santa-tcc.mobileconfig` approve North Pole Security's system extension and full-disk access (team ID `ZMCG7MLDV9`).

Check on the Mac:

```sh
santactl status
```

Success: the sync server shown is `https://prod-rudolph.<zone>/`, mode Monitor. Capture: `santactl status --json > ~/05-status-before.json` (copy to `$CAPTURE_DIR`).

### 5.3 First sync: an empty deployment

```sh
sudo santactl sync --debug 2>&1 | tee ~/05-sync-1.txt
sudo santactl sync --debug 2>&1 | tee ~/05-sync-2.txt
```

Success: both end in a successful sync with no HTTP errors (no 401, 400, 415 or 5xx). Rudolph has no rules yet, so each sync is a **clean** sync (the Mac holds no rules) and its rule download carries one rule: a `REMOVE` of the placeholder identifier `0000…0000` (64 zeros), which is how Rudolph makes Santa 2026.8+ clear its rules on a clean sync with nothing to send. Santa may therefore report one rule received rather than none. The Mac still holds 0 rules afterwards, so the second sync is clean again. (A Mac that held rules from another sync server loses them here: that is what a clean sync does.)

On the admin Mac:

```sh
rudolph machines | tee "$CAPTURE_DIR/05-machines.txt"
rudolph lookup machine-ids | tee "$CAPTURE_DIR/05-lookup.txt"
rudolph lookup serial-number "<test Mac serial>"
rudolph machine show -m "$MID" | tee "$CAPTURE_DIR/05-machine-show.txt"
rudolph config get --machine "$MID" | tee "$CAPTURE_DIR/05-config-get.txt"
```

Success: `machines` lists the test Mac and `smoke-curl` by serial number, with hostname, Santa version and today's UTC date as the last sync day; `lookup machine-ids` lists `$MID` and the curl machine; the serial lookup finds `$MID`; `machine show` shows what the test Mac reported (its hostname, serial number, Santa version, client mode `MONITOR`, 0 rules) and a last sync a few minutes ago; `config get` shows built-in or inherited settings (there is no global configuration yet).

`rudolph info` is the same check from the test Mac's side: it reads the Mac's own machine ID from the machine-mapping plist of 5.1, so it only makes sense on the test Mac. If the CLI and AWS credentials for the account are on that Mac, run `ENV=prod rudolph info` there (from a Rudolph checkout, or with `~/.rudolph-cli/prod.json`): it prints `$MID` and the same summary as `machine show`. Without a deployment configured it prints only the machine ID, which is enough to confirm `$MID`.

In the router logs, one sync is a preflight, possibly an eventupload, one or more ruledownload pages and a postflight:

```sh
for f in preflight eventupload ruledownload postflight; do
  aws logs tail "/aws/lambda/${PREFIX}_rudolph_$f" --since 15m --region "$REGION" --format short | grep "machine=$MID"
done | tee "$CAPTURE_DIR/05-router-lines.txt"
```

Capture: both sync outputs (redact nothing but the token, which Santa does not print), the router lines, and in rudolph-web the machine page of `$MID` (last sync, client mode `MONITOR`, Santa version, "Sync health", last sync type `CLEAN`).

### 5.4 Global configuration

```sh
rudolph config get --global
rudolph config set --global -c monitor            # answer yes
rudolph config get --global | tee "$CAPTURE_DIR/05-global-config.txt"
```

Success: the first shows no stored global configuration; after `set`, client mode monitor with the default batch size and regexes. It applies at the Mac's next sync.

This is not test data: it is the configuration every Mac migrated in step 14 will get. Set it to what the fleet should run with (see [sync.md](sync.md) and [lockdown.md](lockdown.md)); monitor is the safe start.

## 6. Rules reaching the Mac

Make two harmless test binaries on the test Mac (Go is enough; `go build` signs them ad hoc on Apple silicon):

```sh
mkdir -p ~/smoke-bins && cd ~/smoke-bins
for n in blocked unknown grouped machine; do
  mkdir -p "$n" && printf 'package main\n\nimport "fmt"\n\nfunc main() { fmt.Println("rudolph smoke: %s") }\n' "$n" > "$n/main.go"
  (cd "$n" && go mod init "smoke/$n" >/dev/null 2>&1; go build -o "../smoke-$n" .)
done
shasum -a 256 smoke-* | tee ~/06-hashes.txt
```

Without Go on the test Mac, build them on the admin Mac (`GOOS=darwin GOARCH=arm64`) and copy them over. Export the hashes on the admin Mac as `SHA_BLOCKED`, `SHA_UNKNOWN`, `SHA_GROUPED`, `SHA_MACHINE`. Each hash is unique to these freshly built binaries, so even a global rule on one of them affects nothing but the test Macs; step 13 removes every rule this step creates.

### 6.1 Global rule, clean sync, then incremental

```sh
rudolph rule deny -t binary -i "$SHA_BLOCKED" --global --custom-msg "Rudolph test: global block"
rudolph rules --global | tee "$CAPTURE_DIR/06-global-rules.txt"
```

On the test Mac:

```sh
sudo santactl sync --debug 2>&1 | tee ~/06-sync-3.txt     # clean: the Mac still holds 0 rules
sudo santactl sync --debug 2>&1 | tee ~/06-sync-4.txt     # incremental (normal)
santactl rule --check --path ~/smoke-bins/smoke-blocked
santactl fileinfo ~/smoke-bins/smoke-blocked
./smoke-bins/smoke-blocked; echo "exit $?"
```

Success: sync 3 is clean and delivers the blocklist rule; sync 4 is normal and delivers no rule. `santactl rule --check` reports a blocklist binary rule; `fileinfo` shows the SHA-256 and its rule as blocked. Running the binary is blocked, with the custom message in Santa's dialog. In rudolph-web, the machine page's last sync type goes `CLEAN` then `NORMAL`. Cleanup: step 13.1.

### 6.2 Machine rule

```sh
rudolph rule allow -t binary -i "$SHA_MACHINE" --machine "$MID" --expires 2h
rudolph rules --machine "$MID"
```

Sync on the Mac, then `santactl rule --check --path ~/smoke-bins/smoke-machine`. Success: an allowlist binary rule; `rudolph rules --machine` shows its expiry about 2 hours ahead. Then remove it, sync, and check the rule is gone from the Mac (this is also its cleanup):

```sh
rudolph rule remove -t binary -i "$SHA_MACHINE" --machine "$MID"
```

### 6.3 Temporary client mode

```sh
rudolph config update --machine "$MID" -c lockdown --for 30m      # asks for confirmation: yes
rudolph config get --machine "$MID" | tee "$CAPTURE_DIR/06-temp-mode.txt"
```

On the Mac: `sudo santactl sync`, then `santactl status` shows mode Lockdown. Run `~/smoke-bins/smoke-unknown`: blocked as unknown (this creates the `BLOCK_UNKNOWN` event used in step 7). Then end it (the cleanup; it would also end by itself after 30 minutes):

```sh
rudolph config update --machine "$MID" --end-temporary-mode
```

Sync again; `santactl status` shows Monitor. Success: `config get` shows the temporary mode and its end in local time and UTC while it runs; rudolph-web's machine page shows it under "Sync health". Capture both `config get` outputs and the two `santactl status` outputs.

### 6.4 Group rule

```sh
rudolph group create smoke --description "Rudolph test group (delete after the first deployment checks)"
rudolph rule deny -t binary -i "$SHA_GROUPED" --group smoke --custom-msg "Rudolph test: group block"
```

Sync the Mac (it reports `smoke` through `MachineOwnerGroups`; the server caches the group list for up to 60 seconds, so wait a minute after `group create`), then:

```sh
rudolph group show --machine "$MID" | tee "$CAPTURE_DIR/06-group-show.txt"
rudolph group get smoke
```

On the Mac: `santactl rule --check --path ~/smoke-bins/smoke-grouped`, and run it. Success: `group show` lists `smoke` as a reported group that counts and as effective; the Mac gets the group rule without a clean sync; the binary is blocked with the group message. Optional: `rudolph group update smoke --no-reported`, sync, and the rule leaves the Mac (the Mac is now ungrouped); `--accept-reported` brings it back. Only Macs whose profile reports `smoke` (the test Macs) are in the group. Cleanup: step 13.2.

Optional, on the 2026.8 and macOS 13 (2025.12) Macs: repeat 5.1 to 5.3 and 6.1. On 2025.12, also add `rudolph rule silent-gui -t binary -i "$SHA_UNKNOWN" --global` and check `santactl rule --check` there shows a silent blocklist rule (Rudolph sends `SILENT_GUI_BLOCKLIST` as `SILENT_BLOCKLIST` to releases before 2026.6), and that rudolph-web marks the Mac "legacy client". Then remove the rule:

```sh
rudolph rule remove -t binary -i "$SHA_UNKNOWN" --global
```

## 7. Events: blocked execution to events table to rudolph-web

After 6.1 and 6.3 the Mac holds `BLOCK_BINARY` and `BLOCK_UNKNOWN` events. Run `~/smoke-bins/smoke-unknown` once more in monitor mode (an `ALLOW_UNKNOWN` event), then sync so they are uploaded:

```sh
sudo santactl sync --debug 2>&1 | tee ~/07-sync-events.txt
```

On the admin Mac:

```sh
aws logs tail "/aws/lambda/${PREFIX}_rudolph_eventupload" --since 10m --region "$REGION" | tee "$CAPTURE_DIR/07-eventupload-log.txt"
aws dynamodb query --region "$REGION" --table-name "$EVENTS" \
  --key-condition-expression 'PK = :pk' --expression-attribute-values "{\":pk\":{\"S\":\"Exec#$MID\"}}" \
  --projection-expression 'SK, Decision, FileName, FileSHA256, SigningID, TeamID' | tee "$CAPTURE_DIR/07-events.json"
aws dynamodb query --region "$REGION" --table-name "$EVENTS" \
  --key-condition-expression 'PK = :pk' --expression-attribute-values "{\":pk\":{\"S\":\"BinaryDay#$(date -u +%F)\"}}" | tee "$CAPTURE_DIR/07-day-counters.json"
```

Success: `POST /eventupload/{machine_id} machine=$MID status=200`; items with `Decision` `BLOCK_BINARY` (smoke-blocked), `BLOCK_UNKNOWN` and `ALLOW_UNKNOWN` (smoke-unknown), and the group block; a day counter per binary with `Blocked` or `Allowed` counts; no `dropped` line in the log. In rudolph-web: `/events` lists them newest first; `/events?decision=BLOCK_BINARY` and `/events?machine=<MID>` filter them.

If no events arrive: check that the sync was normal, not clean (events are not uploaded during a clean sync), and look for a 500 in the eventupload log (see [If something fails](#if-something-fails)).

These test events stay in production until they expire; step 13.5 explains.

## 8. Collecting allowed executions per rule (Google team ID)

This checks the per-rule collection path end to end: one Mac uploading every execution, one flagged rule, and only that rule's executions stored. The team ID rule is a machine rule on the test Mac, so the rest of the fleet never gets it.

```sh
rudolph config update --machine "$MID" --enable-all-event-upload
rudolph rule allow -t teamid -i EQHXZ8M8AV --machine "$MID" --expires 24h
rudolph rule update -t teamid -i EQHXZ8M8AV --machine "$MID" --collect-allowed-events=true
rudolph rule get -t teamid -i EQHXZ8M8AV --machine "$MID" | tee "$CAPTURE_DIR/08-rule-get.txt"
```

Expect `rule update` to warn that `EnableAllEventUpload` is off in the global configuration (it is on for this Mac only, which is the point).

On the Mac: sync (now with `enable_all_event_upload: true` in the preflight response), confirm `santactl fileinfo /Applications/Gemini.app` (or Google Chrome if Gemini is not installed) shows team ID `EQHXZ8M8AV` and record its signing ID; quit and launch Gemini and Chrome a few times (Santa only uploads decisions it did not answer from its cache; `sudo santactl flushcache` clears that cache if your Santa has it), wait at least a minute (eventupload caches the collection index for up to 60 seconds in a warm Lambda), then sync again.

Success:

- rudolph-web `/events?team_id=EQHXZ8M8AV` lists `ALLOW_TEAMID` events with signing IDs such as `EQHXZ8M8AV:com.google.Chrome` and Gemini's own; capture the list of distinct signing IDs.
- Other allowed executions this Mac uploaded (Apple-signed apps, other vendors) are **not** in the table: only `BLOCK_*`, `ALLOW_UNKNOWN` and the flagged rule's executions are stored.
- Optionally, the groups doc example: `rudolph rule deny -t signingid -i "<Gemini signing ID>" --group smoke --custom-msg "Gemini is not allowed for the test group"`, sync, launch Gemini: blocked (a signing ID rule beats a team ID rule), while Chrome still runs. Step 13.2 deletes it with the group.

Capture: the eventupload log lines (status and duration of these larger uploads), the count of `ALLOW_TEAMID` items, and the count of eventupload requests from this Mac while the setting is on (step 12.1 uses it).

Turn it off again (the stored events cost money; uploads with every execution cost Lambda time). Removing the rule also switches its collection off:

```sh
rudolph config update --machine "$MID" --inherit enable-all-event-upload
rudolph rule remove -t teamid -i EQHXZ8M8AV --machine "$MID"
```

## 9. Ranker and the policy index backfill

### 9.1 The ranker's first run

Either wait for the EventBridge Scheduler schedule `${PREFIX}_rudolph_ranker` (`rate(1 hour)` in UTC with the defaults from 2.5), or invoke it once:

```sh
aws lambda invoke --region "$REGION" --function-name "${PREFIX}_rudolph_ranker" "$CAPTURE_DIR/09-ranker-out.json"
aws logs tail "/aws/lambda/${PREFIX}_rudolph_ranker" --since 15m --region "$REGION" | tee "$CAPTURE_DIR/09-ranker-log.txt"
aws dynamodb get-item --region "$REGION" --table-name "$EVENTS" \
  --key '{"PK":{"S":"Top#Blocked#7"},"SK":{"S":"Current"}}' | tee "$CAPTURE_DIR/09-pointer.json"
```

Success: the invoke answers `"StatusCode": 200` without `FunctionError`; the log has only `START`, `END` and `REPORT` lines (the ranker logs nothing on success); the pointer has `Snapshot` and `GeneratedAt`. rudolph-web `/events/top-blocked?days=1` lists smoke-blocked, smoke-grouped and smoke-unknown with counts and `Machines` 1; `/events/top-allowed` lists smoke-unknown and, after step 8, Google binaries. Then check the next scheduled run happened: `aws scheduler get-schedule --region "$REGION" --name "${PREFIX}_rudolph_ranker"` shows `State` `ENABLED` and the expression from 2.5, and within about an hour of the apply (with `rate(1 hour)`), the ranker log has a second `REPORT` and the page shows a newer generation time.

Capture: `REPORT` lines (duration, max memory used), screenshots of both lists for 1 and 7 days.

Keep the hourly schedule until the checks are done (step 10.1 expects no stale-list warning, and 12.4 compares the cost with the hourly estimate). To change it afterwards, set `ranker_schedule_expression` and `ranker_schedule_timezone` in Rudolph's `config.auto.tfvars.json` and rudolph-web's `top_list_stale_after` to fit, and apply each (see 2.5); the plan updates only `aws_scheduler_schedule.ranker` and, in rudolph-web, the function's environment.

### 9.2 `rules backfill-policy-index`

```sh
rudolph rules backfill-policy-index --dry-run
rudolph rules backfill-policy-index | tee "$CAPTURE_DIR/09-backfill.txt"
rudolph rules backfill-policy-index
```

Success: `Would update 0 global rules`, then `Updated 0 global rules` twice. On a new deployment every global rule is written with `GlobalRulePolicy` already; anything other than 0 means a rule was written without it, which would be a bug to record. In rudolph-web, `/rules?policy=BLOCKLIST` lists the smoke-blocked rule.

## 10. rudolph-web page walk-through

### 10.1 Read-only pages

rudolph-web is still deployed with `enable_editing` `false` (4.2). Signed in as the **viewer**:

| Page | Check |
|---|---|
| `/` | redirects to or shows the machines page |
| `/machines` | the test Macs and `smoke-curl`, last seen as a UTC date |
| `/machines?serial=<first 3 characters>` and `?hostname=<prefix>` | prefix search, case-insensitive |
| `/machines?serial=a&hostname=b` | 400, "Filter by serial number or by hostname, not both." |
| `/machines/<MID>` | last sync, client mode, rule counts, machine rules (none left after 6.2 and 8), groups (assigned, reported, effective), Sync health, a "Commands" disclosure |
| `/rules`, `?type=BINARY`, `?policy=BLOCKLIST`, both | global rules filtered |
| `/groups`, `/groups/smoke`, and "Macs in no group" | registered group with description and "reported counts", its rule |
| `/events`, `?machine=<MID>`, `?decision=BLOCK_UNKNOWN`, `?team_id=EQHXZ8M8AV` | newest first, one filter at a time; two filters together answer 400 |
| `/events/top-blocked?days=1`, `7`, `30`, `90`; `/events/top-allowed` likewise | generation time shown, no "more than 3 hours ago" warning (`top_list_stale_after`) |
| any list with `?cursor=garbage` | 400 |
| `/admin` and `/admin/audit` | 403 for the viewer |
| `/rules`, `/machines/<MID>`, `/groups/smoke` | no add or remove forms, only the copy-and-paste commands |
| Sign out (button) | back to Entra's sign-out, then signed out |

Copy one command from an event row (e.g. block a binary) and one from the machine page, paste each with `--dry-run` appended in the Rudolph checkout: each prints what it would write and `Dry run: the changes shown are not written.` This is production: always keep `--dry-run` on these test pastes.

Sign in as the **admin**: `/admin` answers 200 and says the deployment is read-only and that editing is turned on with `enable_editing`; `/admin/audit` answers 404 (the route only exists with editing), and no page shows a form. Sign in with an account that has neither role (if you have one): every page 403.

Capture: a screenshot per page, the dry-run outputs, and `aws logs tail "/aws/lambda/${WEB_PREFIX}_rudolph_web" --since 30m` (no secrets, cookies or tokens in it).

### 10.2 Editing from the web view

rudolph-web's editing lets people with `Rudolph.Admin` add and remove global, machine and group rules and set or end a Mac's temporary client mode from the pages, writing through Rudolph's own packages and recording each change in Rudolph's audit trail (rudolph-web's `docs/deploy.md`, "Editing"). This step turns it on, makes one change of each kind on the test Mac, checks that the Mac receives them, that the audit trail names the person, and that a viewer cannot write. Every change is undone within the step.

**What turning it on adds.** `enable_editing = true` sets `RUDOLPH_WEB_EDITING=true` on the function and adds two statements to its role's inline policy, both on the store table only (no index, not the events table):

- `WriteRudolphRules`: `dynamodb:PutItem`, `dynamodb:UpdateItem` and `dynamodb:DeleteItem`, with a `ForAllValues:StringLike` condition on `dynamodb:LeadingKeys` limiting it to `GlobalRules`, `RulesFeed`, `MachineRules#*`, `Machine#*`, `GroupRules#*`, `GroupRegistry` and `EventCollection`.
- `AppendRudolphAudit`: `dynamodb:PutItem` only, on `Audit#*`, so the role cannot update or delete an audit entry.

`LeadingKeys` matches the partition key only, so `Machine#*` (which the temporary mode needs for a Mac's `Config` row) also lets the role write that Mac's sensor data, sync state and group rows, and `GroupRegistry` lets it create and delete groups; the code writes only the rows it needs. That is the broadest effect the role can have: record that you accept it before leaving editing on.

1. In rudolph-web's `config.auto.tfvars.json`, set `"enable_editing": true`. From the rudolph-web checkout, against the build from 4.4 (do not rebuild: a new zip changes the function's code hash):

   ```sh
   cd /path/to/rudolph-web && export ENV=prod
   make plan 2>&1 | tee "$CAPTURE_DIR/10-web-editing-plan.txt" | tail -3
   make justdeploy 2>&1 | tee "$CAPTURE_DIR/10-web-editing-apply.txt" | tail -5
   cd /path/to/rudolph && export ENV=prod
   ```

   Success: the plan shows `0 to add, 2 to change, 0 to destroy`: `aws_iam_role_policy.web` gains `WriteRudolphRules` and `AppendRudolphAudit` as above, and `aws_lambda_function.web` changes only `RUDOLPH_WEB_EDITING` from `false` to `true`. If `build/package/` is gone, run `make build` first; the plan then also updates the function's code, which is expected in that case only. Anything else in the plan: stop and check.
2. Sign in as the **admin**. `/admin` now says editing is on and links to `/admin/audit`. On `/rules`, with the form "Add a global rule": type `BINARY`, identifier the value of `$SHA_UNKNOWN` (smoke-unknown, from step 6), policy `BLOCKLIST`, custom message `Rudolph test: web global block`, description `Rudolph web editing test`. The page returns to `/rules` and lists the rule.
3. On `/machines/<MID>`, with "Add a rule for this Mac": type `BINARY`, identifier the value of `$SHA_MACHINE`, policy `ALLOWLIST`, expires `2h`. Then, in the temporary mode form, set `LOCKDOWN` for `30m`. The page shows the machine rule with its expiry and, under "Sync health", the temporary mode and its end.
4. On the test Mac:

   ```sh
   sudo santactl sync --debug 2>&1 | tee ~/10-sync-web-1.txt
   santactl status
   santactl rule --check --path ~/smoke-bins/smoke-unknown
   santactl rule --check --path ~/smoke-bins/smoke-machine
   ~/smoke-bins/smoke-unknown; echo "exit $?"
   ~/smoke-bins/smoke-machine; echo "exit $?"
   ```

   Success: the sync is normal (incremental) and succeeds; `santactl status` shows mode Lockdown; smoke-unknown has a blocklist binary rule and is blocked with `Rudolph test: web global block`; smoke-machine has an allowlist binary rule and runs. On the admin Mac, `rudolph rules --global`, `rudolph rules --machine "$MID"` and `rudolph config get --machine "$MID"` show the same rule, rule and temporary mode as the CLI would have written them.
5. Back on `/machines/<MID>`, end the temporary mode now, and remove the machine rule with its **Remove rule** button; on `/rules`, remove the global rule. Sync the Mac again (`~/10-sync-web-2.txt`): `santactl status` shows Monitor, and `santactl rule --check` shows no rule for either binary. `rudolph rules --machine "$MID"` may still list the machine rule as marked for removal until the Mac has synced (as in 13.3).
6. As the admin, open `/admin/audit`. Success: the six changes, newest first: `rule.remove` (target `global`), then `rule.remove`, `config.end-temporary-mode`, `config.temporary-mode` and `rule.add` (each target `machine:<MID>`), then `rule.add` (target `global`), each with the actor `web:<Entra user name> (<Entra object ID>)` and a summary, and each target linking to the page it changed (`/rules`, `/machines/<MID>`). The CLI changes of steps 5 to 9 are listed too, with `cli:` actors.
7. **A viewer cannot write.** In one browser, as the admin, open `/rules` and `/machines/<MID>` in two tabs (both show the forms). In a third tab, sign out and sign in as the **viewer**; then go back to the first two tabs and submit an add-rule form (any value) and the temporary mode form. Both answer 403, because the session cookie is now the viewer's, and nothing is written (the audit page, as the admin later, shows no new entry). Reload the pages as the viewer: no forms are shown, and `/admin/audit` answers 403.
8. On the admin Mac:

   ```sh
   rudolph audit --since 1d | tee "$CAPTURE_DIR/10-audit.txt"
   ```

   Success: the web entries of this step, with `web:` actors, and the CLI entries of steps 5 to 9 (`config.set`, `rule.add`, `rule.remove`, `config.update`, `config.temporary-mode`, `config.end-temporary-mode`, `group.create`, `rule.update` and so on) with the actor `cli:<your user>@<your host>`, newest first, with UTC and local times. If the list ends with `Showing the newest 100 entries`, add `--limit 500`.

Then decide whether to leave editing on:

- **Leave it on** if admins should change rules and temporary modes from the pages. Assign `Rudolph.Admin` only to the people who may do that; removing someone's assignment takes effect only when their session ends (up to `session_ttl`, one hour), or at once if you roll the cookie key (rudolph-web's `docs/deploy.md`, "Rolling the secrets").
- **Turn it off** to keep the view read-only: set `"enable_editing": false` in rudolph-web's `config.auto.tfvars.json` and run `make plan` and `make justdeploy` again from the rudolph-web checkout, against the same build. The plan changes the same two resources back; afterwards the forms are gone, the write routes and `/admin/audit` answer 404, and the role has no write action. The audit entries stay in Rudolph's table, and `rudolph audit` still lists them.

Capture: the plan and apply outputs, screenshots of the forms, of the machine page with the temporary mode and of `/admin/audit`, the two sync outputs, the 403 pages, `10-audit.txt`, and what you decided about editing.

## 11. Checks the audit left for the first deploy

### 11.1 Compressed request bodies and `binary_media_types`

Done in 3.5 with `curl`, and by every Santa sync since (Santa compresses with deflate by default). Record the result here as well. Optionally set `SyncClientContentEncoding` to `gzip` in the test profile for one sync, then back.

### 11.2 Audit D3: the S3 ACL

Audit item D3 expected `aws_s3_bucket_acl "log-delivery-write"` to fail on new buckets, whose ACLs are disabled by default. The code now sets no ACL at all: the access-log bucket keeps ACLs disabled (`BucketOwnerEnforced` ownership) and lets S3 deliver access logs through its bucket policy, as AWS recommends. Check:

```sh
SRC="${PREFIX}-${ORG}-rudolph-source"
aws s3api get-bucket-ownership-controls --bucket "$SRC-logging"
aws s3api get-bucket-policy --bucket "$SRC-logging" --query Policy --output text
aws s3api get-bucket-acl --bucket "$SRC-logging"
aws s3api get-bucket-logging --bucket "$SRC"
aws s3api get-bucket-encryption --bucket "$SRC-logging"
aws s3api get-public-access-block --bucket "$SRC"
```

Success: the apply in 3.3 had no ACL error; ownership `BucketOwnerEnforced`; the bucket policy has the SSL-only deny and an `Allow` for the service principal `logging.s3.amazonaws.com` to `s3:PutObject` on `arn:aws:s3:::$SRC-logging/$SRC/*`, with `aws:SourceAccount` the account ID and `aws:SourceArn` `arn:aws:s3:::$SRC`; the ACL grants only the bucket owner (no grant to the log delivery group); the source bucket logs to `$SRC-logging` with prefix `$SRC/`; the logging bucket's default encryption is `AES256` (SSE-S3).

Then, a few hours after the apply, look for access logs:

```sh
aws s3 ls "s3://$SRC-logging/$SRC/" | head
```

Success: log objects have arrived (S3 delivers access logs on a best-effort basis, usually within a few hours). The logging bucket uses SSE-S3 because S3 server access logs are only delivered to destination buckets with SSE-S3 default encryption. If none arrive after a day, record it with the output of the commands above.

### 11.3 Alarm firing paths

Firing alarms on purpose is safe in production: nothing acts on them except the notifications. Each check below **emails every subscriber of the topic from 2.4**, once when the alarm fires and again when it returns to `OK`; tell them beforehand.

List them first: `aws cloudwatch describe-alarms --alarm-name-prefix "${PREFIX}_rudolph_" --region "$REGION" --query 'MetricAlarms[].[AlarmName,StateValue]' --output table`. Expect six, all `OK` or `INSUFFICIENT_DATA`.

- **No `lambda_errors` from a wrong token.** The request in 3.4 with `Bearer wrong` made the authoriser return its `Unauthorized` error, which Lambda counts as a function error. `lambda_errors` leaves the authoriser out, so it must not fire. Check that the authoriser's `Errors` metric shows the error for that minute, and that `lambda_errors` stayed `OK` or `INSUFFICIENT_DATA` (`aws cloudwatch describe-alarm-history --alarm-name "${PREFIX}_rudolph_lambda_errors" --history-item-type StateUpdate --region "$REGION"` shows no change to `ALARM`). If it did fire, record it.
- **`request_too_large`**, for real: a body that decompresses to more than 6 MiB. It is refused before anything is written.

  ```sh
  head -c 7000000 /dev/zero | tr '\0' ' ' | perl -MCompress::Zlib -0777 -ne 'print compress($_)' |
    curl -s -w '\n%{http_code}\n' -X POST "https://$HOST/preflight/$CURL_MID" \
      -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -H 'Content-Encoding: deflate' --data-binary @-
  ```

  Success: `413`; the preflight log has `status=413`; within 5 to 10 minutes `prod_rudolph_request_too_large` is `ALARM` (and emails the topic's subscribers), and returns to `OK` later.
- **`api_4xx`**, for real: more than 5% of at least `api_4xx_alarm_min_requests` (10) requests in each of two consecutive 5-minute periods answered 4xx. Requests without a token are answered 401 by API Gateway without invoking Lambda:

  ```sh
  for i in $(seq 1 30); do curl -s -o /dev/null "https://$HOST/health"; sleep 20; done
  ```

  Success: after about 10 minutes `prod_rudolph_api_4xx` is `ALARM` and the subscribers are emailed.
- **`api_5xx`, `lambda_timeouts`, `dynamodb_throttles`**: there is no safe way to cause them. Check that each notifies by setting its state by hand; CloudWatch puts it back at the next evaluation (another email):

  ```sh
  aws cloudwatch set-alarm-state --region "$REGION" --alarm-name "${PREFIX}_rudolph_api_5xx" \
    --state-value ALARM --state-reason "first deployment check: set by hand"
  ```

Capture: the alarm history of each (`aws cloudwatch describe-alarm-history --alarm-name ... --history-item-type StateUpdate`), and whether the SNS emails arrived.

### 11.4 Logs carry no secrets or payloads

```sh
for g in authorizer health preflight eventupload ruledownload postflight xsrf ranker; do
  aws logs filter-log-events --region "$REGION" --log-group-name "/aws/lambda/${PREFIX}_rudolph_$g" \
    --filter-pattern '"Bearer"' --query 'events[].message' --output text
done
```

Success: no output. Also search for the test Mac's user name and `smoke-blocked`: they must not appear (the router logs route, machine, status and duration only).

## 12. Measurements

Leave the test Mac syncing normally (every 600 seconds by default) for at least a few hours, ideally a few days, while you use it. Record each figure next to the number the docs assume. The first migration stage (step 14.2) can start while this runs, once you have the quiet window of 12.2.

### 12.1 Events per Mac per day

eventupload logs one line per upload with counts only (no hashes, paths, decisions or names): `eventupload: machine=<id> received=<n> stored=<m>`. `received` is the number of execution events in the request; `stored` is the number this upload wrote to the events table. An event already stored by an earlier attempt of the same upload (Santa re-sends a batch after a 5xx) is not counted again, so summing `stored` never counts an event twice (it can miss one whose write succeeded just as the upload timed out; the retry then finds it already stored). `received` is counted again on every retry: compare it with the router line's `status=500` count before reading it as Santa's event volume.

Events per Mac per day, in CloudWatch Logs Insights on `/aws/lambda/prod_rudolph_eventupload`:

```
parse @message /eventupload: machine=(?<machine>\S+) received=(?<received>\d+) stored=(?<stored>\d+)/
| filter ispresent(machine)
| stats count() as uploads, sum(received) as received_events, sum(stored) as stored_events by machine, bin(1d)
```

Uploads per Mac per day by status, from the router line:

```
parse @message /POST \S+ machine=(?<machine>\S+) status=(?<status>\d+) duration=(?<dur>\S+)/
| filter ispresent(machine)
| stats count() as uploads by machine, status, bin(1d)
```

To cross-check `stored` against the table, count the stored events per Mac for one UTC day (the sort key starts with the execution time in microseconds, 20 digits):

```sh
DAY=2026-11-10
A=$(printf '%020d' $(( $(date -j -u -f '%Y-%m-%d %H:%M:%S' "$DAY 00:00:00" +%s) * 1000000 )))
B=$(printf '%020d' $(( $(date -j -u -f '%Y-%m-%d %H:%M:%S' "$DAY 23:59:59" +%s) * 1000000 + 999999 )))
aws dynamodb query --region "$REGION" --table-name "$EVENTS" --select COUNT \
  --key-condition-expression 'PK = :pk AND SK BETWEEN :a AND :b' \
  --expression-attribute-values "{\":pk\":{\"S\":\"Exec#$MID\"},\":a\":{\"S\":\"$A\"},\":b\":{\"S\":\"$B~\"}}"
```

Compare with: rudolph-web `docs/data-layer-design.md` ("Event store options": about 50 events per Mac per day by default in monitor mode, about 5 in lockdown, about 500 with `EnableAllEventUpload`) and [events-table.md](events-table.md#costs). With the default upload setting nearly every uploaded event is stored, so `received` and `stored` stay close; with `EnableAllEventUpload` (step 8) `received` shows every uploaded execution and `stored` only the blocked, unknown and collected ones.

### 12.2 DynamoDB consumed capacity per sync

Pick a quiet window of a few hours in which only the test Mac syncs and nobody runs `rudolph` or rudolph-web (before step 14 moves more Macs over). Count its syncs (preflights) in Logs Insights on the preflight log group (`filter @message like /status=200/ | stats count()`), then:

```sh
S=2026-11-10T00:00:00Z; E=2026-11-10T06:00:00Z
for m in ConsumedReadCapacityUnits ConsumedWriteCapacityUnits; do
  aws cloudwatch get-metric-statistics --region "$REGION" --namespace AWS/DynamoDB --metric-name "$m" \
    --dimensions Name=TableName,Value="$STORE" --start-time "$S" --end-time "$E" --period 21600 --statistics Sum
done
```

Index writes are reported separately: repeat the write metric with `Name=GlobalSecondaryIndexName,Value=DataType_SerialKey` (and the other indexes) added to the dimensions. Divide by the number of syncs.

Compare with: audit C3 in `docs/audit/shortcomings.md` (budget for a steady-state sync with no changes: at most 3 writes and 5 reads, no scans) and [sync.md](sync.md#sensor-data-row) (a steady-state preflight writes nothing to the machine list indexes; about one index write per Mac per day when `LastSeenDay` changes).

### 12.3 Lambda durations and memory

Logs Insights over all `/aws/lambda/prod_rudolph_*` groups:

```
filter @type = "REPORT"
| stats count() as invocations, avg(@duration) as avgMs, pct(@duration, 95) as p95Ms, max(@duration) as maxMs,
        max(@maxMemoryUsed / 1048576) as maxMemMB, count(@initDuration) as coldStarts by @log
```

Compare with the configured memory (preflight and eventupload 256 MB, ruledownload and postflight 512 MB, ranker 256 MB) and the eventupload timeout (10 s, of which sinks and the events table get the deadline minus 1.5 s: [sync.md](sync.md#delivery-to-the-event-sink)). Also record API Gateway `Latency` and `IntegrationLatency` for the stage.

### 12.4 Costs

1. Check the `Name` cost allocation tag activated in 2.6 shows as active.
2. After 2 to 3 days, Cost Explorer, daily, grouped by service, filtered to tag `Name` = `Rudolph`. The filter is required here: the account's other resources would otherwise be counted too.
3. Look at the budget (Billing → Budgets → `prod-rudolph-monthly`): its actual spend should match the Cost Explorer view, and its forecast should be well below the amount. If it shows nothing after 2 days, the tag is not active.

Compare with [costs.md](costs.md) (a floor of about US$0.10 a day for one Mac, plus fixed items: two KMS customer managed keys at US$1 a month each and point-in-time recovery on both tables; the alarms about US$2.40 a month; the hosted zone is not Rudolph's here, as it already existed) and the ranker estimate in [events-table.md](events-table.md#top-binaries) (about US$2 a month at 1,000 Macs, so cents here).

Capture: a CSV export of the Cost Explorer view.

## 13. Clean up the test data

Once steps 5 to 12 have passed, remove what the checks created, so that production holds only the fleet's real rules and Macs. Every command asks for confirmation; add `--dry-run` first to see what it would change. Each cleanup command is itself recorded in the audit trail (13.7).

### 13.1 Global rules (6.1, and 6.4's optional `silent-gui` rule)

```sh
rudolph rule remove -t binary -i "$SHA_BLOCKED" --global
rudolph rule remove -t binary -i "$SHA_UNKNOWN" --global     # only if you added the silent-gui rule and did not remove it in 6.4
rudolph rules --global | tee "$CAPTURE_DIR/13-global-rules.txt"
```

Success: neither hash is listed. Macs drop the rules at their next sync.

### 13.2 The test group and its rules (6.4, 8)

```sh
rudolph group get smoke                   # lists the group's rules
rudolph group delete smoke --force        # deletes the group and all of its rules
rudolph group list
```

`group delete` refuses a group that still has rules unless `--force` is given, which deletes them with it. Success: `smoke` is no longer listed. The test Macs keep reporting `smoke` through `MachineOwnerGroups` until step 14 replaces their profile; Rudolph ignores a reported group that is not registered.

### 13.3 Machine rules and the test Mac's configuration (6.2, 6.3, 8)

6.2 and 8 removed their machine rules, and 6.3 ended its temporary mode. Check nothing is left:

```sh
rudolph rules --machine "$MID"
rudolph config get --machine "$MID"
```

Success: no machine rules other than ones marked for removal, which the Mac drops at its next sync; no temporary mode; every setting `(inherited)`. For a setting still marked `(machine)`, run `rudolph config update --machine "$MID" --inherit <setting>`.

### 13.4 Test binaries

On the test Mac: `rm -rf ~/smoke-bins`.

### 13.5 Events from the test binaries (7, 8)

There is no CLI command to delete events. The test executions stay in the events table until DynamoDB's TTL deletes them: each event 90 days after its upload, day counters and machine markers about 100 days after their last event, and the top list snapshots within 8 days of the last ranker run that counted them. Until then they show in rudolph-web's `/events` pages, and in the top lists for whichever windows still include them; they are labelled by binary name (`smoke-blocked`, `smoke-unknown`, `smoke-grouped`), so they are easy to recognise.

### 13.6 The `smoke-curl` machine (3.5)

The made-up machine's rows would expire on their own 90 days after its last preflight; until then it is listed in rudolph-web's machine list and in `rudolph lookup machine-ids`. Remove it now with `rudolph machine forget` (see [cli.md](cli.md#forgetting-a-retired-mac)), which lists what it will delete and asks first:

```sh
CURL_MID="$(cat "$CAPTURE_DIR/03-curl-machine-id.txt")"
rudolph machine forget -m "$CURL_MID"
```

Expect the list to show the sensor data and the sync state (and no machine rules, configuration or groups), with the hostname `smoke-curl`. Check the ID and the hostname before answering `yes`: this deletes from the production table. Afterwards `rudolph machine show -m "$CURL_MID"` says no machine with that ID has synced, and `rudolph machines` no longer lists `SMOKECURL01`.

Do not forget the test Mac itself: it stays on Rudolph and moves to the fleet profile in step 14.2.

### 13.7 The audit trail stays

Every change the checks made through the CLI (steps 5 to 9 and this step's cleanup, `machine.forget` included) and through rudolph-web (10.2) is recorded in Rudolph's audit trail, in the store table under `Audit#<UTC day>`. Audit entries never expire and there is no command to delete them: they are the record of this test and stay. Check they are complete and keep a copy with the captures:

```sh
rudolph audit --since 30d --limit 1000 | tee "$CAPTURE_DIR/13-audit.txt"
```

`--since` takes a duration (`7d`, `36h`) or an RFC 3339 time, at most 400 days back (`rudolph audit --since 400d` is the furthest); older entries are kept but not listed. In rudolph-web, `/admin/audit` (with editing on) shows the last 30 days.

Capture: the outputs of 13.1 to 13.3, 13.6 and 13.7.

## 14. Migrating the rest of the fleet

With the checks passed and the test data cleaned up, move the rest of the fleet onto Rudolph in stages, so a problem shows on a few Macs before it can reach all of them.

### 14.1 The fleet profile

Start from `configs/santa-configuration.mobileconfig` (plus `configs/santa-sysext.mobileconfig` and `configs/santa-tcc.mobileconfig` if the Macs do not have them yet): `SyncBaseURL` is the `sync_base_url` output, `SyncExtraHeaders` carries `Bearer <token>`, and the machine ID settings match how your Macs get theirs. Leave out the test profile's `MachineOwnerGroups` `smoke`; use `MachineOwnerGroups` only for real groups (see [groups.md](groups.md)). Keep `ClientMode` monitor until the fleet's rules are in place (see [lockdown.md](lockdown.md)).

Before the first stage, load the fleet's real rules (`rudolph rules import`, see [rules.md](rules.md)) and set the global configuration (5.4). Remember that a Mac's first sync with Rudolph is a clean sync: it replaces every rule the Mac held with Rudolph's, so the rules must be in Rudolph before the Macs arrive.

### 14.2 Stages

1. **The test Macs.** Replace the test profile from 5.2 with the fleet profile and sync. Check they sync cleanly and get the fleet's rules.
2. **A few Macs** (a handful, ideally different models, macOS versions and users, including someone who will tell you when something is blocked). Scope the fleet profile to them in your MDM and remove their old Santa profile in the same change. Watch them for a few days.
3. **The rest**, in one go or in larger batches as your fleet size suggests, once stage 2 has run for a few days without surprises.

### 14.3 What to watch at each stage

- **Alarms** (step 2.4): `api_4xx` (a wrong token in the profile shows as 401s), `api_5xx`, `lambda_errors`, `request_too_large` (a Mac with a large backlog of events after the move).
- **rudolph-web:** the machine list fills in as Macs sync; each machine page's "Sync health" shows a recent sync and the expected client mode and rule count. A Mac that is in the MDM scope but not in the list has not synced: check `santactl status` and `sudo santactl sync --debug` on it. From the CLI, `rudolph machines --limit 500` lists the Macs that have synced by serial number with their last sync day, and `rudolph machine show -m <machine ID>` shows one Mac's last sync, configuration and groups.
- **Audit trail:** `rudolph audit --since 7d` (or `/admin/audit` with editing on) shows every rule and configuration change made during the migration, and by whom.
- **Events:** `/events?decision=BLOCK_BINARY` and `BLOCK_UNKNOWN`, and the top-blocked list, for anything blocked that should not be (in monitor mode, for what would be blocked in lockdown).
- **Costs:** the budgets (2.6, 4.2) and Cost Explorer (12.4); costs grow with the number of Macs (see [costs.md](costs.md)).

### 14.4 Rolling a Mac back

To move a Mac back to its previous sync server, remove the fleet profile from it in your MDM and reinstall its old Santa profile (or scope it back into the old profile's group). At its next sync Santa uses the old server again, which sends that server's rules (a clean sync if the old server asks for one). Santa keeps enforcing the rules and mode it last received until then. Nothing needs to change in Rudolph: the Mac's rows expire 90 days after its last preflight. If the Mac is not coming back, `rudolph machine forget -m <machine ID>` removes them now (as in 13.6), once it has stopped syncing with Rudolph.

## 15. Teardown (only if there are major issues)

Tear down only if the checks turned up problems that rule out using the deployment. **It deletes every rule, configuration and event in Rudolph, and the audit trail with them** (it is kept in the store table), so do it before any fleet Mac depends on it, and move any Mac already on Rudolph back first (14.4). Keep the record first: `rudolph audit --since 400d --limit 10000 > "$CAPTURE_DIR/15-audit-before-teardown.txt"`.

1. Turn off table deletion protection, which otherwise makes the destroy fail on the tables. In `deployments/environments/$ENV/config.auto.tfvars.json`, set `"table_deletion_protection": false`, then apply it from the Rudolph checkout, at the commit that is deployed, against the existing build:

   ```sh
   export ENV=prod && make justdeploy
   ```

   Do not use `make deploy` here: it rebuilds, and `make build` re-zips the binaries with new timestamps, which changes their hashes, so the plan also replaces the S3 source objects, updates every Lambda's code and creates a new API deployment. Never ship new code as part of a teardown. With the build from the last deployment still in `build/package/`, the plan shows only `deletion_protection_enabled` changing from `true` to `false` on the two tables. If `build/package/` is gone (a fresh checkout, another computer), run `make build` at the deployed commit first; the plan then also lists those S3 object, Lambda code and API deployment changes, which is expected in that case only. Anything else in the plan: stop and check.
2. Destroy both stacks, each from its own checkout. Review each plan before typing `yes`: it must list only resources of that stack.

   ```sh
   # rudolph-web checkout
   export ENV=prod && make destroy
   aws ssm delete-parameter --region "$REGION" --name "/${WEB_PREFIX}/rudolph-web/cookie-key"
   aws ssm delete-parameter --region "$REGION" --name "/${WEB_PREFIX}/rudolph-web/client-secret"

   # Rudolph checkout
   export ENV=prod && make destroy
   ```

- `make destroy` deletes only what is in that stack's Terraform state: both DynamoDB tables (all rules, configurations and events), the S3 buckets (emptied first, `force_destroy`), the Lambdas, API, alarms, the cost budgets, the certificate and the `<prefix>-rudolph` records in the hosted zone. The hosted zone itself and every other resource in the account are untouched. KMS keys are scheduled for deletion, not deleted at once.
- Left behind: the state bucket and its objects, the hosted zone, the Entra app registration and its secret, the SNS topic from 2.4 (only if this runbook created it, as recorded in 2.4, delete it with `aws sns delete-topic --region "$REGION" --topic-arn "$TOPIC_ARN"`, which also removes its subscriptions; a topic that existed before stays), the account-wide budget if you created one, and the Santa profile on the test Macs. Remove the profile; Santa keeps its last rules and mode until reconfigured.
- Afterwards: `aws resourcegroupstaggingapi get-resources --tag-filters Key=Name,Values=Rudolph --region "$REGION"` lists what still carries the tag (deleted resources can linger in it for a while).
- Revoke the sync token: it is only valid while a deployment holds its digest, but delete it from the password manager so it is never reused.

Capture: the deletion protection apply and both destroy outputs.

## If something fails

| Symptom | Look at |
|---|---|
| `make plan` or `apply` errors | the Terraform output in `$CAPTURE_DIR`; "already exists": a leftover (step 2.9); for a Lambda runtime error, see [Doc issues found](#doc-issues-found); for an ACL error, step 11.2 |
| `make destroy` fails on a DynamoDB table | deletion protection is still on: step 15.1 |
| 401 on every sync | `SyncExtraHeaders` value (exactly `Bearer <token>`), the digest in `sync_token_sha256` (`printf '%s'`, not `echo`); `/aws/lambda/prod_rudolph_authorizer` |
| 400 on preflight | `/aws/lambda/prod_rudolph_preflight`: `preflight: refusing request: ...` names the field over its limit; a machine ID that is not a UUID is logged as `machine=invalid` |
| 415 | the request's `Content-Type` is not `application/json` |
| 413 | the body decompresses to more than 6 MiB; the `request_too_large` alarm |
| 500 from any handler | the handler's log group: the line before `status=500` is `internal server error: "..."` with the cause |
| 502 or 504 | the handler timed out or crashed: `Task timed out` or a panic in its log group; `lambda_timeouts` |
| Sync succeeds but no events | the sync was clean (no events then), or eventupload answered 500 (events table write failed: IAM `prod_rudolph_events_write_policy`, throttling) |
| Allowed executions not stored | `EnableAllEventUpload` not on for the Mac, rule not flagged (`rudolph rule get`), or the 60-second cache; eventupload log lines starting `eventupload: could not read` |
| Top lists "not computed yet" | ranker log group; `aws lambda invoke` it by hand (9.1); its role may only touch `BinaryDay#`, `BinarySeen#`, `TopSnapshot#` and `Top#` |
| Budget shows no spend | the `Name` cost allocation tag is not active yet (2.6), or was activated less than 24 hours ago |
| rudolph-web 500 on every page | `/aws/lambda/<web prefix>_rudolph_web`: `config: ... is required` (second apply missing), SSM parameter errors |
| rudolph-web 503 | `RUDOLPH_WEB_DYNAMODB_TABLE` or `RUDOLPH_WEB_EVENTS_TABLE` not set on the function |
| rudolph-web 500 on data pages | a KMS `AccessDenied` in its log: set `dynamodb_kms_key_arn` (step 4.2); a missing index: Rudolph deployed without it |
| rudolph-web: no forms, or 404 on `/admin/audit` and the form POSTs | `enable_editing` is `false` (or the apply in 10.2 did not run); forms are shown to `Rudolph.Admin` only |
| rudolph-web 403 on a form POST | the session is a viewer's (expected for the viewer in 10.2), or the request is cross-origin: the browser's `Origin` does not match `base_url` |
| rudolph-web 400 on a form | Rudolph refused the input, as the CLI would (the page gives the reason); 404: no such rule, or Rudolph has nothing for that machine ID |
| rudolph-web 500 after a form, "could not be recorded in the audit trail" | the change was saved but not audited: an `AccessDenied` on `Audit#` in its log means the role lacks `AppendRudolphAudit` |
| `rudolph` warns "the change was made but not audited" | the CLI's credentials cannot `dynamodb:PutItem` on `Audit#*` ([CLI permissions](deploy.md#cli-permissions)) |
| Santa side | `sudo santactl sync --debug`; `santactl status`; `log show --predicate 'process == "santad" OR process == "santasyncservice"' --last 15m` |

Request counts by route and status, in Logs Insights over the handler log groups:

```
parse @message /(?<method>[A-Z]+) (?<route>\S+) machine=(?<machine>\S+) status=(?<status>\d+)/
| filter ispresent(status)
| stats count() by route, status
```

## Doc issues found

Found while writing or reviewing this runbook, by comparing the docs with the code (of both repositories) and with the vendored Santa sources in `testdata/`. Each was fixed, if at all, in the doc or code it names, not in this runbook; statuses as of 2026-11-09.

1. **`EnableCleanSyncEventUpload` is not Santa's key.** [sync.md](sync.md) (and comments in `internal/testutil/fakesanta`) tell admins to set `EnableCleanSyncEventUpload` in the profile. Santa 2025.12, 2026.8 and 2026.9 read `SyncEnableCleanSyncEventUpload` (`kSyncEnableCleanSyncEventUpload` in `SNTConfigurator.mm`); the documented key does nothing. Fixed 2026-11-07.
2. **The example Santa profile is out of date.** `configs/santa-configuration.mobileconfig`, which [configuring Santa](configuring-santa.md) points to as the example, sets its preferences under `com.google.santa`, while the same page says Santa 2026.8 reads `com.northpolesec.santa` (confirmed in `SNTConfigurator.mm`), and it has no `SyncExtraHeaders`, without which every sync is refused with 401. Fixed 2026-11-07.
3. **"Received 0 rules" on the first sync.** [configuring Santa](configuring-santa.md) says a working first sync receives 0 rules; on a deployment with no rules, the clean sync sends one rule, the `REMOVE` of the placeholder identifier (`internal/handlers/ruledownload/clean_sync.go`). Verify what Santa prints in 5.3 and correct the doc. Fixed 2026-11-07.
4. **No event count in the eventupload log.** rudolph-web's `docs/data-layer-design.md` says to measure events per Mac per day by counting `events` per upload in the Lambda logs. eventupload logs only the router line (route, machine, status, duration) and `dropped` lines; step 12.1 counts uploads in the logs and stored events in the table instead. Measuring uploaded events with `EnableAllEventUpload` on would need a log line or metric that does not exist. Fixed 2026-11-07.
5. **Module outputs not exposed by the example environment.** rudolph-web's `docs/deploy.md` refers to Rudolph's `events_dynamodb_table_name` output and the table's KMS key; the `santa_api` module has `events_dynamodb_table_name`, `store_kms_key_arn` and `api_gateway_id` outputs, but `deployments/environments/example/_outputs.tf` exports only `sync_base_url` and `events_dynamodb_table_arn` (step 2.7 adds them locally). Fixed 2026-11-07.
6. **`lambda_errors` likely fires on a wrong bearer token.** The authoriser rejects a token that does not match by returning the `Unauthorized` error (`internal/handlers/authorizer/authorizer.go`), which Lambda counts as a function error; [deploy.md](deploy.md#alarms) describes `lambda_errors` as firing when a function "returned an error" and only mentions rejected tokens under `api_4xx`. To be confirmed in 11.3; if it fires, scanners with a made-up token page whoever receives the alarms. Fixed 2026-11-07.
7. **Audit D3 is out of date, and access logging may still not work.** [shortcomings.md](audit/shortcomings.md) D3 describes an ACL on new buckets; the code has since removed the source bucket's ACL and added ownership controls before the logging bucket's ACL. The remaining risk is different: the logging bucket's default encryption is SSE-KMS with a customer managed key, which AWS documents as unsupported for server access log destinations, so logs may never be delivered. The logging buckets now use SSE-S3 (2026-11-07), and then ACLs disabled with a bucket policy granting `logging.s3.amazonaws.com` instead of the ACL; audit D3 now describes both. Fixed 2026-11-09. That access logs are actually delivered is still to be confirmed in 11.2.
8. **`provided.al2` runtime.** Rudolph's Lambdas (`modules/lambda/api-handler/lambda.tf`, `ranker.tf`) use `provided.al2`, while rudolph-web uses `provided.al2023`. Amazon Linux 2 reached end of support on 30 June 2026; if AWS has started blocking creation of `provided.al2` functions, the apply in 3.3 fails. No doc mentions the runtime or this risk. Fixed 2026-11-07.
9. **The costs floor omits fixed monthly items.** [costs.md](costs.md) gives a floor of about US$0.10 a day for one Mac and lists the alarms (about US$2.50 a month), but not the three KMS customer managed keys (US$1 a month each), the hosted zone or point-in-time recovery; the real floor is likely higher. Step 12.4 measures it. Fixed 2026-11-07.
10. **Example environment placeholders.** `deployments/environments/example/config.auto.tfvars.json` gives `eventupload_handler` as `[KINESIS|FIREHOSE]` with `eventupload_autocreate_policies: true`, although `NONE` is valid and is the simplest start, and it leaves out `authorizer_result_ttl_in_seconds`, which the example's README shows. Fixed 2026-11-07.
11. **rudolph-web's measurement note names the wrong field.** rudolph-web's `docs/data-layer-design.md` ("Event store options") says to replace its per-Mac estimate by counting `events` per upload in the Lambda logs. Rudolph's eventupload line is `eventupload: machine=<id> received=<n> stored=<m>` (`internal/handlers/eventupload/handler.go`); step 12.1 sums `received` and `stored`. Open (rudolph-web). Fixed 2026-11-09.
12. **Stale-list warning fixed at 3 hours.** [deploy.md](deploy.md#ranker-schedule) said rudolph-web warns when a top list is more than 3 hours old, so a working-hours ranker schedule shows the warning every night and weekend. rudolph-web now takes the threshold from `top_list_stale_after` (`1h` to `168h`, default `3h`). Fixed 2026-11-09.
13. **rudolph-web's function description says read-only.** The Lambda function's description is "Read-only admin web view for Rudolph" (`deployments/terraform_modules/rudolph_web/lambda.tf`) also when `enable_editing` is true, which the Lambda console then shows. Cosmetic; nothing reads it. Open (rudolph-web). Fixed 2026-11-09.
