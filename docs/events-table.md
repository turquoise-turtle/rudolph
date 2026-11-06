# Execution events table

The eventupload Lambda writes execution events to a second DynamoDB table, `PREFIX_rudolph_events` (resource `aws_dynamodb_table.events` in `deployments/terraform_modules/santa_api/modules/store/events.tf`). A separate, read-only web view (rudolph-web) lists recent events from it and ranks the binaries blocked or allowed on each day. Rudolph itself never reads this table.

The table is written independently of the event sinks: it is filled whether eventupload forwards to Firehose, Kinesis, a Lambda, several of them or none (`eventupload_handler = "NONE"`). The Lambda finds it through the `EVENTS_DYNAMODB_NAME` environment variable; when that is not set (a build run outside the `santa_api` module), nothing is written.

## What is stored

For now only the executions an administrator works through before moving Macs to lockdown are stored:

- every `BLOCK_*` decision (`BLOCK_UNKNOWN`, `BLOCK_BINARY`, `BLOCK_CERTIFICATE`, `BLOCK_SCOPE`, `BLOCK_TEAMID`, `BLOCK_SIGNINGID`, `BLOCK_CDHASH`, `BLOCK_BINARY_MISMATCH`, and any later `BLOCK_*` decision);
- `ALLOW_UNKNOWN` (an unknown binary allowed because the Mac is in monitor mode).

Executions allowed by a rule (`ALLOW_BINARY`, `ALLOW_CERTIFICATE`, `ALLOW_TEAMID`, `ALLOW_SIGNINGID`, `ALLOW_CDHASH`), `ALLOW_SCOPE`, `ALLOW_PLATFORM`, `BUNDLE_BINARY`, file access events and audit events are not stored. Allowed-by-rule events will be stored per rule once a rule can be flagged for collection; storing all of them would multiply the table's cost about tenfold.

By default Santa uploads only blocked executions and, on Macs in monitor mode, `ALLOW_UNKNOWN`, so with the default configuration the table holds almost everything Santa uploads.

An event without a file SHA-256 or an execution time cannot be keyed, and an event DynamoDB rejects as invalid would be rejected on every retry. Such events are dropped and the eventupload log gets one line containing `dropped` with the count and machine ID (never the event); the rest of the upload is stored and the upload succeeds.

## Items

| Item | PK | SK | Attributes | Expires |
|---|---|---|---|---|
| Event | `Exec#<machine ID>` | `<TS>#<file SHA-256>#<pid>` | `MachineID`, `TS`, `Fleet` (always `Exec`), `Decision` (Santa's name, e.g. `BLOCK_BINARY`), `FileSHA256`, `FilePath`, `FileName`, `ExecutingUser`, `TeamID`, `SigningID`, `CDHash` | 90 days after upload |
| Day counter | `BinaryDay#<UTC date of the execution>` (`YYYY-MM-DD`) | `<file SHA-256>` | `Blocked`, `Allowed` (numbers; absent until the first such event), `FileName`, `SigningID`, `TeamID` | 100 days after its first event |
| Machine marker | `BinarySeen#<file SHA-256>` | `<machine ID>` | none | 100 days after the Mac last ran the binary |

`TS` is the execution time in microseconds since the Unix epoch, rounded and zero-padded to 20 digits, so it sorts as a string. Empty attributes are left out (DynamoDB rejects an empty string in an index key such as `TeamID`). `ExpiresAfter` holds the expiry in Unix seconds; the table's TTL deletes expired items, free of charge, usually within a few days of expiry.

Indexes, all projecting every attribute and with `TS` as the sort key, so a time window is a range on `TS`:

- `Fleet_TS` (`Fleet`): every event of the fleet, newest first.
- `Decision_TS` (`Decision`): events with one decision.
- `TeamID_TS` (`TeamID`): events of one team ID. Sparse: unsigned binaries have no team ID.

Day counters and machine markers have no `TS`, so they are in no index.

## How events are written

- Each event is written with `PutItem` only if it does not exist yet (`attribute_not_exists(PK)`). Santa uploads again whatever it could not confirm, so the same event can arrive more than once; it is stored once.
- Only newly stored events bump the day counters and machine markers. The events of one upload for the same binary on the same day are combined into one `UpdateItem` (`ADD Blocked`/`Allowed`; the display attributes and the expiry are set only if absent), and each binary gets one marker per upload. An upload of N events therefore makes at most 3N DynamoDB calls, at most 16 at a time.
- The writes run alongside the sinks and share their deadline (the Lambda deadline minus 1.5 seconds, see [Delivery to the event sink](sync.md#delivery-to-the-event-sink)). A failed write, or running out of time, answers HTTP 500, so Santa keeps the events and uploads them again at its next sync; events already stored are skipped then.
- Counters are not updated in the same transaction as the events. If the counter updates fail after the events were written, the retry finds the events already stored and does not count them: a day counter can then be slightly low. Event items are always exact.

## Costs

The cost depends on how many events Santa uploads, not on how often binaries run. Each stored event costs about six write units (the item, three index entries, its share of a day counter and a machine marker) and about 1 KB of storage for 90 days. rudolph-web's `docs/data-layer-design.md`, sections "Event store options" and "D+ in detail", has the estimates by fleet size and upload setting; in short, with Santa's default upload setting it is a few US dollars a month for 1,000 Macs in monitor mode and under a dollar in lockdown mode. Turning on `EnableAllEventUpload` multiplies uploads about tenfold; while allowed-by-rule events are not stored, that raises Lambda and sink costs but not this table's.

Point-in-time recovery is on, as for the store table, and is billed by table size.

## Access for the web view

The `santa_api` module outputs `events_dynamodb_table_name`, `events_dynamodb_table_arn` and `store_kms_key_arn`. The web view's role needs `dynamodb:Query` and `dynamodb:GetItem` on the table ARN and `<table ARN>/index/*`. The table is encrypted with the same customer managed KMS key as the store table; that key's policy already lets any principal in the account use it through DynamoDB, so no extra KMS grant is needed unless your organisation has tightened the key policy.

The eventupload Lambda is the only writer: it gets `dynamodb:PutItem`, `dynamodb:UpdateItem` and `dynamodb:BatchWriteItem` on this table only (policy `PREFIX_rudolph_events_write_policy`), and no access to read it.
