# IAM for operators and deployers

The `rudolph` CLI reads and writes the store table with the operator's own AWS credentials, not with a Rudolph role (see [cli.md](cli.md)). Terraform needs credentials of its own to deploy. This page gives example policies for both. Replace `REGION`, `ACCOUNT_ID` and `PREFIX` (the deployment's `prefix`); `KEY_ID` is the store tables' KMS key (the `store_kms_key_arn` output of the `santa_api` module, alias `alias/PREFIX-santa-rules-tables-sse`).

The tables are `PREFIX_rudolph_store` (rules, configurations, what Macs report, groups and the audit trail) and `PREFIX_rudolph_events` (execution and file access events and the top lists, see [events-table.md](events-table.md)). The CLI uses only the store table; the events table is in the read-only example for people who look at it in the console.

## Read-only operator

For `rudolph rules`, `rule get`, `config get`, `machine show`, `machines`, `lookup`, `group list`, `group members`, `audit`, `repair` without `--delete` and the other commands that only read. `Scan` is for `repair` and `group members --rebuild --dry-run`; `Query` on the indexes is for `machines` and `lookup`; `DescribeTable` is for the console.

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "ReadRudolphTables",
      "Effect": "Allow",
      "Action": [
        "dynamodb:BatchGetItem",
        "dynamodb:DescribeTable",
        "dynamodb:GetItem",
        "dynamodb:Query",
        "dynamodb:Scan"
      ],
      "Resource": [
        "arn:aws:dynamodb:REGION:ACCOUNT_ID:table/PREFIX_rudolph_store",
        "arn:aws:dynamodb:REGION:ACCOUNT_ID:table/PREFIX_rudolph_store/index/*",
        "arn:aws:dynamodb:REGION:ACCOUNT_ID:table/PREFIX_rudolph_events",
        "arn:aws:dynamodb:REGION:ACCOUNT_ID:table/PREFIX_rudolph_events/index/*"
      ]
    },
    {
      "Sid": "DecryptRudolphTablesThroughDynamoDB",
      "Effect": "Allow",
      "Action": "kms:Decrypt",
      "Resource": "arn:aws:kms:REGION:ACCOUNT_ID:key/KEY_ID",
      "Condition": {
        "StringEquals": {
          "kms:ViaService": "dynamodb.REGION.amazonaws.com"
        }
      }
    }
  ]
}
```

The KMS statement is only needed if your organisation has tightened the store key's policy: as deployed, the key policy already lets every principal of the account use the key through DynamoDB, so the table permissions are enough. It is limited to use through DynamoDB either way.

## Rule administrator

For every command, including those that write: rules, configurations, temporary client modes, device groups, `machine forget`, `group members --rebuild`, `rules import`, `rules backfill-policy-index` and `repair --delete`. It reads like the read-only operator (on the store table only), and writes only the partitions the CLI writes, found from the code (`pkg/model` and `internal/cli`):

| Partition key | Written by |
|---|---|
| `GlobalRules`, `RulesFeed` | global rules and the feed rows that tell Macs about each change (`rule`, `rules import`, `rules backfill-policy-index`) |
| `GlobalConfig` | the global configuration (`config set\|update --global`) |
| `Machine#*` | a Mac's configuration, temporary client mode and clean sync request (`config`), its assigned groups (`group add\|remove`), and everything `machine forget` deletes |
| `MachineRules#*` | machine rules (`rule --machine`, `machine forget`) |
| `MachineRuleDelivery#*` | delivery tracking of machine rules, deleted by `machine forget` |
| `GroupRules#*`, `GroupRegistry` | group rules and the registry of groups (`rule --group\|--ungrouped`, `group create\|update\|delete`, `config update --group\|--ungrouped`) |
| `GroupMembers#*` | a group's member rows (`group add\|remove`, `group members --rebuild`, `machine forget`) |
| `EventCollection` | the index of rules whose allowed executions are collected (`rule --collect-allowed-events`, `rule update`, `rule remove`, `machine forget`) |
| `MachineInfo#*`, `MachineConfig#*` | legacy items deleted by `repair --delete`; leave them out if you never run it |
| `Audit#*` | the audit entry every change records: `PutItem` only |

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "ReadRudolphStore",
      "Effect": "Allow",
      "Action": [
        "dynamodb:BatchGetItem",
        "dynamodb:DescribeTable",
        "dynamodb:GetItem",
        "dynamodb:Query",
        "dynamodb:Scan"
      ],
      "Resource": [
        "arn:aws:dynamodb:REGION:ACCOUNT_ID:table/PREFIX_rudolph_store",
        "arn:aws:dynamodb:REGION:ACCOUNT_ID:table/PREFIX_rudolph_store/index/*"
      ]
    },
    {
      "Sid": "WriteRudolphStore",
      "Effect": "Allow",
      "Action": [
        "dynamodb:BatchWriteItem",
        "dynamodb:DeleteItem",
        "dynamodb:PutItem",
        "dynamodb:UpdateItem"
      ],
      "Resource": "arn:aws:dynamodb:REGION:ACCOUNT_ID:table/PREFIX_rudolph_store",
      "Condition": {
        "ForAllValues:StringLike": {
          "dynamodb:LeadingKeys": [
            "GlobalRules",
            "RulesFeed",
            "GlobalConfig",
            "Machine#*",
            "MachineRules#*",
            "MachineRuleDelivery#*",
            "GroupRules#*",
            "GroupRegistry",
            "GroupMembers#*",
            "EventCollection",
            "MachineInfo#*",
            "MachineConfig#*"
          ]
        },
        "Null": {
          "dynamodb:LeadingKeys": "false"
        }
      }
    },
    {
      "Sid": "AppendRudolphAudit",
      "Effect": "Allow",
      "Action": "dynamodb:PutItem",
      "Resource": "arn:aws:dynamodb:REGION:ACCOUNT_ID:table/PREFIX_rudolph_store",
      "Condition": {
        "ForAllValues:StringLike": {
          "dynamodb:LeadingKeys": [
            "Audit#*"
          ]
        },
        "Null": {
          "dynamodb:LeadingKeys": "false"
        }
      }
    }
  ]
}
```

Notes:

- **The Null check.** `ForAllValues` is also true when a request names no partition at all, so each write statement requires `dynamodb:LeadingKeys` to be present. Without it, a write action added to the statement later that carries no key would not be limited.
- **Transactions.** `TransactWriteItems` has no IAM action of its own: IAM checks each item as a `PutItem`, `UpdateItem` or `DeleteItem` against the statements above. `BatchWriteItem` is an action of its own (`machine forget`, `group delete --force`, `group members --rebuild`).
- **Partition keys only.** `dynamodb:LeadingKeys` limits the partition key, never the sort key: `Machine#*` lets the role write any row of a Mac's partition, including the sensor data and sync state the Lambdas keep there.
- **The audit trail.** `PutItem` on `Audit#*` cannot update or delete an entry with `UpdateItem` or `DeleteItem`, but a `PutItem` can replace one. The CLI writes each entry with a conditional put (`attribute_not_exists`), so it never overwrites one; IAM does not enforce that. Reading the trail (`rudolph audit`) is a `Query`, covered by the read statement. Without `PutItem` on `Audit#*`, every change is still written, but the command warns that it was not audited and exits non-zero (see [The audit trail](cli.md#the-audit-trail)).
- **KMS.** Add the read-only example's KMS statement, with `kms:GenerateDataKey` and `kms:Encrypt` as well, only if your organisation has tightened the store key's policy.
- **The events table.** The CLI never reads or writes it. Its only writers are the eventupload Lambda and the ranker.

## Deployer

The role that runs Terraform (`make deploy`) creates and changes everything in the `santa_api` module: IAM roles and policies, Lambda, API Gateway, DynamoDB, KMS keys, S3 buckets, ACM certificates, Route 53 records, CloudWatch alarms, log groups and metric filters, EventBridge Scheduler schedules and, with a [cost budget](deploy.md#cost-budget), AWS Budgets ([deploy.md](deploy.md#amazon-web-services) lists the scheduler and Budgets actions). It also needs `iam:PassRole` on every role it hands to a service:

| Role (all under the IAM path `/rudolph/`) | Passed to |
|---|---|
| `PREFIX_rudolph_<endpoint>_role`, one per Lambda (`health`, `xsrf`, `preflight`, `eventupload`, `ruledownload`, `postflight`, `authorizer`), and `PREFIX_rudolph_ranker_role` | Lambda |
| `PREFIX_rudolph_api_gateway_authorizer` (the role API Gateway invokes the authoriser with) | API Gateway |
| `PREFIX_rudolph_ranker_scheduler_role` | EventBridge Scheduler |
| `PREFIX_rudolph_eventsupload_firehose_role` (only when Terraform creates the Firehose sink) | Kinesis Data Firehose |

Every Rudolph role is under `/rudolph/` and named after the prefix, so one statement covers them, limited to those four services:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "PassRudolphRoles",
      "Effect": "Allow",
      "Action": "iam:PassRole",
      "Resource": "arn:aws:iam::ACCOUNT_ID:role/rudolph/PREFIX_rudolph_*",
      "Condition": {
        "StringEquals": {
          "iam:PassedToService": [
            "lambda.amazonaws.com",
            "apigateway.amazonaws.com",
            "scheduler.amazonaws.com",
            "firehose.amazonaws.com"
          ]
        }
      }
    }
  ]
}
```

rudolph-web's role is under `/rudolph-web/` and is passed by rudolph-web's own deployment, not this one.
