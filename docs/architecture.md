# Architecture & Design Philosophies

Rudolph is designed for Amazon Web Services. It is designed with the following principles in mind:

* Ease of deployment
* Low maintenance
* Cost Conscious

To achieve these, it uses almost exclusively serverless AWS products. Of them the most prominent are:

* Amazon API Gateway
* Amazon Lambda
* Amazon DynamoDB
* Amazon S3
* Amazon EventBridge Scheduler


## API Gateway
API Gateway houses the public hostname, endpoints, and TLS. It also implements resource validations.

All API Gateway interactions are proxied to Lambda.


## Lambda
Lambda serves as the "server" component of Rudolph. Web requests invoke Lambda functions which interact with DynamoDB and
return response structures. These structures are translated into HTTP responses that are returned to the Santa clients.

One Lambda function is not behind the API: the ranker (`PREFIX_rudolph_ranker`), which an EventBridge Scheduler schedule runs (every hour by default; see [Ranker schedule](deploy.md#ranker-schedule)) to build the web view's lists of the binaries blocked and allowed most often (see [Top binaries](events-table.md#top-binaries)).


## DynamoDB
Rudolph has two DynamoDB tables:

* The store table, `PREFIX_rudolph_store`, holds all rules (global, machine-specific and group rules), machine configurations, device groups, sync state and the sensor data each Mac reports at preflight.
* The events table, `PREFIX_rudolph_events`, holds the events the eventupload Lambda stores (blocked and unknown executions, those allowed by rules flagged for collection, and file access events) for 90 days, with per-day counters per binary and the top lists the ranker builds. Only eventupload and the ranker write it; Rudolph's sync never reads it. See [Events table](events-table.md).

Besides its primary key (`PK`, `SK`), the store table has five global secondary indexes: `SerialNum_DataType_MachineID` (`rudolph lookup serial-number`), `DataType_MachineID` (machine ID prefix lookups), `DataType_SerialKey` and `DataType_HostnameKey`, which the web view uses to list Macs by serial number and hostname (`rudolph machines` reads `DataType_SerialKey` too) (see [Sensor data row](sync.md#sensor-data-row)), and `GlobalRulePolicy_SK`, which the web view uses to list global rules by policy (see [Global rules and the policy index](rules.md#global-rules-and-the-policy-index)).

The store table also holds the audit trail: one item for every change the CLI (and, from v2, rudolph-web) makes to rules, configuration, groups or machines. Each day has its own partition, `PK = Audit#<YYYY-MM-DD>` (the UTC day), and the sort key is `SK = <Unix time in nanoseconds, 19 digits>#<random hex>`, so items sort by time and two changes in the same instant never collide. The attributes are `Actor` (`cli:<OS user>@<host name>`, or `web:<Entra user>`), `Action` (e.g. `rule.add`, `config.temporary-mode`), `Target` (`global`, `machine:<machine id>`, `group:<name>` or `ungrouped`), `Summary`, `RecordedAt` (RFC 3339 with nanoseconds) and `DataType = Audit`. Entries are written with a conditional put (`attribute_not_exists`), so none is ever overwritten, and never expire (no `ExpiresAfter`). `pkg/model/audit` writes them (`Record`, or `RecordContext` with a client of the AWS SDK; `PutItemInput` builds the conditional request for callers that send it themselves) and reads them (`List` or `ListContext`, which query the day partitions from tomorrow, so that an entry from a writer whose clock runs ahead past UTC midnight is not missed, backwards, newest first, and never scan; `ListPage` or `ListPageContext` do the same one page at a time). All four refuse a `since` more than `MaxLookback` (400 days) ago with `ErrSinceTooOld` before any query, since each day back costs a Query. A page comes with an opaque cursor for the next one, empty after the last page: base64url of `<YYYY-MM-DD>|<SK>`, the day partition and sort key of the last entry returned, so the next page resumes after that key (`ExclusiveStartKey`) in that day and carries on through the earlier days without repeats or gaps. The cursor is validated strictly (a malformed one, one whose sort key is not in its day, or one dated after tomorrow is `ErrInvalidCursor`), a page holds at most 1000 entries (a larger limit is treated as 1000), but not signed, so a caller that hands it to users, as rudolph-web does, wraps it in its own signed cursor. The Lambdas neither write nor read them. See [The audit trail](cli.md#the-audit-trail).

To modify sensor configurations or to edit rules, you would [use the Rudolph cli tool](cli.md) to make edits to this DynamoDB.
You **_would not_** go through Rudolph's public API, as that API does not have any endpoints that would implement this
use case.

A separate repository, rudolph-web, is a read-only web view of both tables: Macs, rules, configurations, groups, recent events and the top lists. It never writes; it shows `rudolph` commands to copy and paste instead.

## S3
Amazon S3 is used to store the compiled golang service binaries that are executed by the Lambda functions.


