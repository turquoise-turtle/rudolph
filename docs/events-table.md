# Execution events table

The eventupload Lambda writes execution events to a second DynamoDB table, `PREFIX_rudolph_events` (resource `aws_dynamodb_table.events` in `deployments/terraform_modules/santa_api/modules/store/events.tf`). A separate, read-only web view (rudolph-web) lists recent events from it and shows the binaries blocked or allowed most often. Apart from the hourly ranker (see [Top binaries](#top-binaries)), Rudolph itself never reads this table.

The table is written independently of the event sinks: it is filled whether eventupload forwards to Firehose, Kinesis, a Lambda, several of them or none (`eventupload_handler = "NONE"`). The Lambda finds it through the `EVENTS_DYNAMODB_NAME` environment variable; when that is not set (a build run outside the `santa_api` module), nothing is written.

## What is stored

For now only the executions an administrator works through before moving Macs to lockdown are stored:

- every `BLOCK_*` decision (`BLOCK_UNKNOWN`, `BLOCK_BINARY`, `BLOCK_CERTIFICATE`, `BLOCK_SCOPE`, `BLOCK_TEAMID`, `BLOCK_SIGNINGID`, `BLOCK_CDHASH`, `BLOCK_BINARY_MISMATCH`, and any later `BLOCK_*` decision);
- `ALLOW_UNKNOWN` (an unknown binary allowed because the Mac is in monitor mode).

Executions allowed by a rule (`ALLOW_BINARY`, `ALLOW_CERTIFICATE`, `ALLOW_TEAMID`, `ALLOW_SIGNINGID`, `ALLOW_CDHASH`) are stored only for rules flagged for collection (see [Collecting allowed executions per rule](#collecting-allowed-executions-per-rule)): storing all of them would multiply the table's cost about tenfold. `ALLOW_SCOPE`, `ALLOW_PLATFORM`, `BUNDLE_BINARY`, file access events and audit events are not stored.

By default Santa uploads only blocked executions and, on Macs in monitor mode, `ALLOW_UNKNOWN`, so with the default configuration the table holds almost everything Santa uploads.

Some events are never stored. They are dropped, the eventupload log gets one line containing `dropped` with the count, the machine ID and the reason (never the event), the rest of the upload is stored and the upload succeeds:

- events without a file SHA-256 or an execution time, which cannot be keyed;
- events executed more than a day after the upload or more than 90 days before it (a wrong clock or a forged upload): a future date would keep an event at the top of every newest-first list for 90 days, and an older event would expire at once;
- events DynamoDB rejects as invalid, which would be rejected on every retry;
- everything beyond the first 1,000 stored events of one upload. Santa uploads in batches of the preflight's `batch_size` (50 by default), so only a misconfigured or forged client reaches this; it bounds the DynamoDB calls and time of one upload.

A long field is not a reason to drop an event: every stored string is cut to at most 4,096 bytes (`Decision` and `TeamID`, which are index keys, to 1,024) on a UTF-8 character boundary. Santa's fields have no length limit, and an item over DynamoDB's 400 KB limit would be refused on every retry.

## Collecting allowed executions per rule

Santa has no per-rule upload switch, so the writer decides. A rule row (global or machine-specific) may carry `CollectAllowedEvents` (boolean, absent when off), set with `rudolph rule update ... --collect-allowed-events=true|false` (see [Collecting the executions a rule allows](rules.md#collecting-the-executions-a-rule-allows), which also has a worked example). An execution allowed by a rule is stored, and counted as `Allowed` on its day counter, when the rule that allowed it is flagged among the global rules or the uploading Mac's own machine rules. The rule is found from the decision and the event's own fields:

| Decision | Rule sort key |
|---|---|
| `ALLOW_BINARY` | `Binary#<file_sha256>` |
| `ALLOW_CERTIFICATE` | `Cert#<signing_chain[0].sha256>` (the leaf certificate) |
| `ALLOW_TEAMID` | `TeamID#<team_id>` |
| `ALLOW_SIGNINGID` | `SigningID#<signing_id>` |
| `ALLOW_CDHASH` | `CDHash#<cdhash>` |

The writer does not scan the rules. The CLI keeps an index of the flagged rules in one item of the store table, PK `EventCollection`, SK `Rules`: a string set `Global` of flagged global rule sort keys, and a string set `Machine#<machine ID>` per Mac with flagged machine rules. It changes the rule row and the index in one transaction, with `ADD` and `DELETE` on the sets, so concurrent CLI runs never lose each other's changes. Removing a rule (`rudolph rule remove`, or `REMOVE` in `rudolph rules import`) or creating it again with `rudolph rule ...` takes it out of the index; rewriting it with `rudolph rules import` keeps its flag.

The index is a hint, not the truth: a machine rule's row can expire or be deleted by the TTL while its entry stays. So the eventupload Lambda (store table named by `DYNAMODB_NAME`):

- reads the index only for an upload holding at least one allowed-by-rule event (an upload with none makes no extra call);
- confirms each matching rule on its row (`GetItem`, strongly consistent) before storing anything: the row must exist and be flagged, and a machine rule must still apply (not expired, not marked for removal, not a `REMOVE`);
- takes a rule whose row is gone or not flagged out of the index, in a transaction that checks the row has not been flagged again meanwhile (best effort);
- keeps the index and each confirmation for up to 60 seconds in a warm Lambda, so a change takes up to a minute to apply;
- matches hash identifiers (binary, certificate and CDHash rules) case-insensitively, as older versions stored some in upper case;
- never fails an upload because one of these reads failed: it logs it, skips the allowed-by-rule events concerned, and stores the rest.

Santa uploads allowed-by-rule executions only when `EnableAllEventUpload` is on (`rudolph config update --machine <id> --enable-all-event-upload` for a few Macs, or `--global`); the CLI warns when a rule is flagged while it is off in the global configuration.

## Items

| Item | PK | SK | Attributes | Expires |
|---|---|---|---|---|
| Event | `Exec#<machine ID>` | `<TS>#<file SHA-256>#<pid>` | `MachineID`, `TS`, `Fleet` (always `Exec`), `Decision` (Santa's name, e.g. `BLOCK_BINARY`), `FileSHA256`, `FilePath`, `FileName`, `ExecutingUser`, `TeamID`, `SigningID`, `CDHash` | 90 days after upload |
| Day counter | `BinaryDay#<UTC date of the execution>` (`YYYY-MM-DD`) | `<file SHA-256>` | `Blocked`, `Allowed` (numbers; absent until the first such event), `FileName`, `SigningID`, `TeamID` | 100 days after its first event |
| Machine marker | `BinarySeen#<file SHA-256>` | `<machine ID>` | `LastSeenDay` (UTC date, `YYYY-MM-DD`, of the Mac's latest upload of an event of the binary) | 100 days after the Mac last ran the binary |
| Top list snapshot row | `TopSnapshot#<kind>#<window>#<snapshot ID>` | `<rank>` (6 digits from `000001`) | `FileSHA256`, `Count`, `Machines` (numbers), `FileName`, `SigningID`, `TeamID` | 2 days after it was written |
| Top list pointer | `Top#<kind>#<window>` | `Current` | `Snapshot` (the snapshot ID), `GeneratedAt` (RFC 3339) | never; replaced every hour |

`TS` is the execution time in microseconds since the Unix epoch, rounded and zero-padded to 20 digits, so it sorts as a string. Empty attributes are left out (DynamoDB rejects an empty string in an index key such as `TeamID`). `ExpiresAfter` holds the expiry in Unix seconds; the table's TTL deletes expired items, free of charge, usually within a few days of expiry.

Indexes, all projecting every attribute and with `TS` as the sort key, so a time window is a range on `TS`:

- `Fleet_TS` (`Fleet`): every event of the fleet, newest first.
- `Decision_TS` (`Decision`): events with one decision.
- `TeamID_TS` (`TeamID`): events of one team ID. Sparse: unsigned binaries have no team ID.

Day counters, machine markers and the top lists have no `TS`, so they are in no index.

## How events are written

- The events of one upload that add to the same day counter (same binary, same UTC day) are written together with that counter in `TransactWriteItems` transactions of up to 99 events plus the counter update, and at most about 3 MB (DynamoDB allows 100 items and 4 MB). Each event is put only if it does not exist yet (`attribute_not_exists(PK)`), and the counter gets `ADD Blocked` or `ADD Allowed` with the number of events written (the display attributes and the expiry are set only if absent). An event is therefore never stored without being counted, nor counted twice. When Santa uploads events again that are already stored, DynamoDB cancels the transaction and says which events failed their condition; the transaction is sent again without them, adding only the others; DynamoDB may name further events on the next attempt, so a batch can take a few calls, and a batch that is entirely stored already needs no second call. An event DynamoDB refuses as invalid is dropped from the batch the same way and logged.
- Transactions on the same counter run one after another, since concurrent transactions on one item cancel each other; different counters are written in parallel, at most 16 calls at a time. A transaction cancelled by a conflicting write from another upload or by throttling is retried after a short wait, up to 5 attempts in all. Each attempt carries its own client request token, derived from its contents, the upload's expiries and the attempt number: the AWS SDK's own retries of an attempt whose response was lost reuse the token, so DynamoDB does not apply the transaction twice.
- Each binary of the upload then gets its machine marker (`PutItem`, with `LastSeenDay` set to the UTC date of the upload), also for events stored by an earlier upload, so a marker lost with an earlier attempt is made up for. A crash-looping binary that fills a 500-event upload therefore takes about a dozen calls, and any upload of N events at most about 2N, besides retries.
- The writes run alongside the sinks and share their deadline (the Lambda deadline minus 1.5 seconds, see [Delivery to the event sink](sync.md#delivery-to-the-event-sink)). After the first failed write no new writes start, but writes already running finish. A failed write, or running out of time, answers HTTP 500, so Santa keeps the events and uploads them again at its next sync; events already stored are skipped then.
- Santa retries the whole upload, not just the events this table missed, so after a table failure the Firehose, Kinesis or Lambda sinks receive the upload's records again. Sinks can already receive records twice when one sink fails after another succeeded; consumers of the sinks must tolerate duplicates.

## Top binaries

The web view's lists of the binaries blocked or allowed most often are built every hour by the ranker, a Lambda of its own (`PREFIX_rudolph_ranker`, code in `cmd/ranker` and `internal/ranker`) run by an EventBridge schedule. A page view then reads one pointer (`GetItem`) and one snapshot (`Query`) instead of up to 90 days of day counters.

- There are eight lists: kind `Blocked` or `Allowed`, each over windows of 1, 7, 30 and 90 days ending today (UTC; the 1-day list is today so far).
- `Count` is the sum of that kind over the window's day counters. `Machines` is the number of the binary's machine markers whose `LastSeenDay` is inside the window. `FileName`, `SigningID` and `TeamID` come from the newest day counter that has them.
- Rows are ordered by `Count`, highest first, then by `FileSHA256`; binaries with a count of 0 are left out, and each list holds at most 500 rows.
- Each run writes every list under a new snapshot ID (it sorts by time), then moves each pointer to it. A reader therefore never sees a half-written snapshot, and one holding the previous ID can still read it until its rows expire two days later. A failed run leaves the pointers on the previous snapshot; the next hourly run starts again from scratch, so failed runs are not retried.
- Markers written before `LastSeenDay` existed have none and count in no window until the Mac uploads an event of that binary again.

How a run reads and writes, and what it costs:

- It reads each of the 90 day partitions (`BinaryDay#<date>`) once, with a paginated `Query`, and uses them for every window and both kinds. It never scans.
- It then reads the machine markers (`Query` on `BinarySeen#<file SHA-256>`) only of the binaries that made a list, once each even when a binary is in several lists: at most 2 × 4 × 500 = 4,000 queries, usually far fewer as the windows overlap.
- It writes the snapshot rows with `BatchWriteItem` (25 rows a call, retrying unprocessed rows), then the eight pointers with `PutItem`. Calls run at most 16 at a time.
- So a run costs about 90 day-partition queries (each half a read unit per 4 KB of counters), up to about 4,000 marker queries and up to about 4,000 row writes. With full lists every hour that is roughly 3 million write units and a few million read units a month, about US$2 a month at us-east-1 on-demand prices (US$0.625 per million write units), plus well under a dollar for the Lambda and the two days of snapshots kept. Smaller fleets with shorter lists pay proportionally less.
- Its role may only `Query` the `BinaryDay#*` and `BinarySeen#*` partitions and `PutItem`/`BatchWriteItem` the `TopSnapshot#*` and `Top#*` partitions of this table (`dynamodb:LeadingKeys`), nothing else.

## Costs

The cost depends on how many events Santa uploads, not on how often binaries run. Each stored event costs five to eight write units: the item costs two because it is written in a transaction, plus three index entries; the day counter update (also two units in a transaction) and the machine marker are shared by the events of one binary in an upload, so they add three more per event when every event is of a different binary and almost nothing for a crash-looping binary and about 1 KB of storage for 90 days. rudolph-web's `docs/data-layer-design.md`, sections "Event store options" and "D+ in detail", has the estimates by fleet size and upload setting (they assume about six write units per event, within that range); in short, with Santa's default upload setting it is a few US dollars a month for 1,000 Macs in monitor mode and under a dollar in lockdown mode. Turning on `EnableAllEventUpload` multiplies uploads about tenfold; only the events of flagged rules are stored, so the rest raise Lambda and sink costs (about US$0.30 a month for 1,000 Macs for the Lambda) but not this table's. Each flagged rule's stored executions cost the same as any other stored event, so flag a broad rule such as a widely used team ID only for as long as you need it.

Point-in-time recovery is on, as for the store table, and is billed by table size.

## Access for the web view

The `santa_api` module outputs `events_dynamodb_table_name`, `events_dynamodb_table_arn` and `store_kms_key_arn`. The web view's role needs `dynamodb:Query` and `dynamodb:GetItem` on the table ARN and `<table ARN>/index/*`. The table is encrypted with the same customer managed KMS key as the store table; that key's policy already lets any principal in the account use it through DynamoDB, so no extra KMS grant is needed unless your organisation has tightened the key policy.

The ranker writes only the top lists (see [Top binaries](#top-binaries)). The eventupload Lambda is the only other writer: it gets `dynamodb:PutItem` and `dynamodb:UpdateItem` (which also cover its transactions) on this table only (policy `PREFIX_rudolph_events_write_policy`), and no access to read it. The same policy lets it, on the store table, `GetItem` the `EventCollection` item and the rule rows (`dynamodb:LeadingKeys` `EventCollection`, `GlobalRules`, `MachineRules#*`), `UpdateItem` the `EventCollection` item only, and `ConditionCheckItem` the rule rows; nothing else of that table.
