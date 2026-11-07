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
* The execution events table, `PREFIX_rudolph_events`, holds the execution events the eventupload Lambda stores (blocked and unknown executions, and those allowed by rules flagged for collection) for 90 days, with per-day counters per binary and the top lists the ranker builds. Only eventupload and the ranker write it; Rudolph's sync never reads it. See [Execution events table](events-table.md).

Besides its primary key (`PK`, `SK`), the store table has five global secondary indexes: `SerialNum_DataType_MachineID` (`rudolph lookup serial-number`), `DataType_MachineID` (machine ID prefix lookups), `DataType_SerialKey` and `DataType_HostnameKey`, which the web view uses to list Macs by serial number and hostname (`rudolph machines` reads `DataType_SerialKey` too) (see [Sensor data row](sync.md#sensor-data-row)), and `GlobalRulePolicy_SK`, which the web view uses to list global rules by policy (see [Global rules and the policy index](rules.md#global-rules-and-the-policy-index)).

To modify sensor configurations or to edit rules, you would [use the Rudolph cli tool](cli.md) to make edits to this DynamoDB.
You **_would not_** go through Rudolph's public API, as that API does not have any endpoints that would implement this
use case.

A separate repository, rudolph-web, is a read-only web view of both tables: Macs, rules, configurations, groups, recent events and the top lists. It never writes; it shows `rudolph` commands to copy and paste instead.

## S3
Amazon S3 is used to store the compiled golang service binaries that are executed by the Lambda functions.


