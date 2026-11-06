# Execution events table

The eventupload Lambda writes execution events to a second DynamoDB table, `PREFIX_rudolph_events` (resource `aws_dynamodb_table.events` in `deployments/terraform_modules/santa_api/modules/store/events.tf`). A separate, read-only web view (rudolph-web) lists recent events from it and ranks the binaries blocked or allowed on each day. Rudolph itself never reads this table.

The table is written independently of the event sinks: it is filled whether eventupload forwards to Firehose, Kinesis, a Lambda, several of them or none (`eventupload_handler = "NONE"`). The Lambda finds it through the `EVENTS_DYNAMODB_NAME` environment variable; when that is not set (a build run outside the `santa_api` module), nothing is written.

## What is stored

For now only the executions an administrator works through before moving Macs to lockdown are stored:

- every `BLOCK_*` decision (`BLOCK_UNKNOWN`, `BLOCK_BINARY`, `BLOCK_CERTIFICATE`, `BLOCK_SCOPE`, `BLOCK_TEAMID`, `BLOCK_SIGNINGID`, `BLOCK_CDHASH`, `BLOCK_BINARY_MISMATCH`, and any later `BLOCK_*` decision);
- `ALLOW_UNKNOWN` (an unknown binary allowed because the Mac is in monitor mode).

Executions allowed by a rule (`ALLOW_BINARY`, `ALLOW_CERTIFICATE`, `ALLOW_TEAMID`, `ALLOW_SIGNINGID`, `ALLOW_CDHASH`), `ALLOW_SCOPE`, `ALLOW_PLATFORM`, `BUNDLE_BINARY`, file access events and audit events are not stored. Allowed-by-rule events will be stored per rule once a rule can be flagged for collection; storing all of them would multiply the table's cost about tenfold.

By default Santa uploads only blocked executions and, on Macs in monitor mode, `ALLOW_UNKNOWN`, so with the default configuration the table holds almost everything Santa uploads.

Some events are never stored. They are dropped, the eventupload log gets one line containing `dropped` with the count, the machine ID and the reason (never the event), the rest of the upload is stored and the upload succeeds:

- events without a file SHA-256 or an execution time, which cannot be keyed;
- events executed more than a day after the upload or more than 90 days before it (a wrong clock or a forged upload): a future date would keep an event at the top of every newest-first list for 90 days, and an older event would expire at once;
- events DynamoDB rejects as invalid, which would be rejected on every retry;
- everything beyond the first 1,000 stored events of one upload. Santa uploads in batches of the preflight's `batch_size` (50 by default), so only a misconfigured or forged client reaches this; it bounds the DynamoDB calls and time of one upload.

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

- Each event is written in one `TransactWriteItems` with its day counter: the event is put only if it does not exist yet (`attribute_not_exists(PK)`), and the counter gets `ADD Blocked :1` or `ADD Allowed :1` (the display attributes and the expiry are set only if absent). An event is therefore never stored without being counted, nor counted twice: Santa uploads again whatever it could not confirm, and a repeated event cancels its transaction on the condition. The transaction carries a client request token derived from the event, so the AWS SDK's own retry of a transaction whose response was lost does not apply it twice.
- Events of one upload that add to the same day counter (same binary, same UTC day) are written one after another, since concurrent transactions on one item cancel each other; a transaction cancelled by a conflicting write from another upload is retried. Different counters are written in parallel, at most 16 calls at a time.
- Each binary of the upload then gets its machine marker (`PutItem`), also for events stored by an earlier upload, so a marker lost with an earlier attempt is made up for. An upload of N events makes at most 2N DynamoDB calls, besides conflict retries.
- The writes run alongside the sinks and share their deadline (the Lambda deadline minus 1.5 seconds, see [Delivery to the event sink](sync.md#delivery-to-the-event-sink)). After the first failed write no new writes start, but writes already running finish. A failed write, or running out of time, answers HTTP 500, so Santa keeps the events and uploads them again at its next sync; events already stored are skipped then.
- Santa retries the whole upload, not just the events this table missed, so after a table failure the Firehose, Kinesis or Lambda sinks receive the upload's records again. Sinks can already receive records twice when one sink fails after another succeeded; consumers of the sinks must tolerate duplicates.

## Costs

The cost depends on how many events Santa uploads, not on how often binaries run. Each stored event costs about eight write units (the item and its counter update cost twice the usual units because they are written in a transaction, plus three index entries and a machine marker) and about 1 KB of storage for 90 days. rudolph-web's `docs/data-layer-design.md`, sections "Event store options" and "D+ in detail", has the estimates by fleet size and upload setting (they assume about six write units per event, so the write cost here is about a third higher); in short, with Santa's default upload setting it is a few US dollars a month for 1,000 Macs in monitor mode and under a dollar in lockdown mode. Turning on `EnableAllEventUpload` multiplies uploads about tenfold; while allowed-by-rule events are not stored, that raises Lambda and sink costs but not this table's.

Point-in-time recovery is on, as for the store table, and is billed by table size.

## Access for the web view

The `santa_api` module outputs `events_dynamodb_table_name`, `events_dynamodb_table_arn` and `store_kms_key_arn`. The web view's role needs `dynamodb:Query` and `dynamodb:GetItem` on the table ARN and `<table ARN>/index/*`. The table is encrypted with the same customer managed KMS key as the store table; that key's policy already lets any principal in the account use it through DynamoDB, so no extra KMS grant is needed unless your organisation has tightened the key policy.

The eventupload Lambda is the only writer: it gets `dynamodb:PutItem` and `dynamodb:UpdateItem` (which also cover its transactions) on this table only (policy `PREFIX_rudolph_events_write_policy`), and no access to read it.
