# Events table

The eventupload Lambda writes execution events and file access events to a second DynamoDB table, `PREFIX_rudolph_events` (resource `aws_dynamodb_table.events` in `deployments/terraform_modules/santa_api/modules/store/events.tf`). A separate, read-only web view (rudolph-web) lists recent events from it and shows the binaries blocked or allowed most often, and those no rule decided. Apart from the ranker (see [Top binaries](#top-binaries)) and the fleet job (see [Fleet health and readiness rows](#fleet-health-and-readiness-rows)), Rudolph itself never reads this table.

The table is written independently of the event sinks: it is filled whether eventupload forwards to Firehose, Kinesis, a Lambda, several of them or none (`eventupload_handler = "NONE"`). The Lambda finds it through the `EVENTS_DYNAMODB_NAME` environment variable; when that is not set (a build run outside the `santa_api` module), nothing is written.

## What is stored

Two kinds of event are stored: the executions an administrator works through before moving Macs to lockdown, and every file access event (see [File access events](#file-access-events)).

Executions stored:

- every `BLOCK_*` decision (`BLOCK_UNKNOWN`, `BLOCK_BINARY`, `BLOCK_CERTIFICATE`, `BLOCK_SCOPE`, `BLOCK_TEAMID`, `BLOCK_SIGNINGID`, `BLOCK_CDHASH`, `BLOCK_BINARY_MISMATCH`, and any later `BLOCK_*` decision);
- `ALLOW_UNKNOWN` (an unknown binary allowed because the Mac is in monitor mode).

Executions allowed by a rule (`ALLOW_BINARY`, `ALLOW_CERTIFICATE`, `ALLOW_TEAMID`, `ALLOW_SIGNINGID`, `ALLOW_CDHASH`) are stored only for rules flagged for collection (see [Collecting allowed executions per rule](#collecting-allowed-executions-per-rule)): storing all of them would multiply the table's cost about tenfold. `ALLOW_SCOPE`, `ALLOW_PLATFORM`, `BUNDLE_BINARY` and audit events are not stored.

By default Santa uploads only blocked executions and, on Macs in monitor mode, `ALLOW_UNKNOWN`, so with the default configuration the table holds almost everything Santa uploads.

Some events are never stored. They are dropped, the eventupload log gets one line containing `dropped` with the count, the machine ID and the reason (never the event), the rest of the upload is stored and the upload succeeds:

- executions without a file SHA-256 or an execution time, which cannot be keyed;
- file access events without an access time or a target, or whose decision is missing, `FILE_ACCESS_DECISION_UNKNOWN` or not a well-formed `FILE_ACCESS_DECISION_` name, that is `FILE_ACCESS_DECISION_` followed by 1 to 64 upper-case letters `A`-`Z` or underscores (case-sensitive; every name Santa sends has that shape, and a value a newer Santa adds still does): the decision is the `Decision_TS` partition key, so an execution decision such as `BLOCK_BINARY` would list the event among the fleet's executions, a trailing space, NUL or newline would let `FILE_ACCESS_DECISION_UNKNOWN` pass as a known decision, a bidi or other invisible character would hide in the web view, and a long suffix would make a partition of its own per Mac;
- events executed or accessed more than a day after the upload or more than 90 days before it (a wrong clock or a forged upload): a future date would keep an event at the top of every newest-first list for 90 days, and an older event would expire at once;
- events DynamoDB rejects as invalid, which would be rejected on every retry;
- everything beyond the first 1,000 stored events of one upload, executions and file access events together (executions first). Santa uploads in batches of the preflight's `batch_size` (50 by default), so only a misconfigured or forged client reaches this; it bounds the DynamoDB calls and time of one upload.

A long field is not a reason to drop an event: every stored string, each path of a file access event's process chain included, is cut to at most 4,096 bytes (`Decision` and `TeamID`, which are index keys, to 1,024) on a UTF-8 character boundary. Santa's fields have no length limit, and an item over DynamoDB's 400 KB limit would be refused on every retry.

### File access events

File access events come from Santa's File Access Authorization (FAA): a process opened a path that an FAA rule watches. Santa uploads them to any configured sync server, a denied access at once and an audit-only match with its next upload, so there is nothing to turn on in Rudolph. The FAA policy itself (the rules, their paths and the processes they allow) comes from the Mac's configuration profile, as `FileAccessPolicy` or `FileAccessPolicyPlist` in the `com.northpolesec.santa` payload, not from Rudolph: sync v1 has no way to deliver FAA rules. Rudolph only sends `override_file_access_action` (`NONE`, `AUDIT_ONLY` or `DISABLE`, see [Reconfiguring the Sensor](sync.md#reconfiguring-the-sensor)).

Every usable file access event is stored, whatever its well-formed decision (`FILE_ACCESS_DECISION_DENIED`, `FILE_ACCESS_DECISION_DENIED_INVALID_SIGNATURE`, `FILE_ACCESS_DECISION_AUDIT_ONLY` or one a newer Santa adds), in a partition of its own per Mac. File access events have no day counter or machine marker, so they never reach the top lists, and they are forwarded to the event sinks exactly as before.

## Collecting allowed executions per rule

Santa has no per-rule upload switch, so the writer decides. A rule row (global, machine-specific or of a device group) may carry `CollectAllowedEvents` (boolean, absent when off), set with `rudolph rule update ... --collect-allowed-events=true|false` (see [Collecting the executions a rule allows](rules.md#collecting-the-executions-a-rule-allows), which also has a worked example). An execution allowed by a rule is stored, and counted as `Allowed` on its day counter, when the rule that allowed it is flagged among the global rules, the uploading Mac's own machine rules, or the rules of the Mac's groups (see [Group rules](#group-rules)). The rule is found from the decision and the event's own fields:

| Decision | Rule sort key |
|---|---|
| `ALLOW_BINARY` | `Binary#<file_sha256>` |
| `ALLOW_CERTIFICATE` | `Cert#<signing_chain[0].sha256>` (the leaf certificate) |
| `ALLOW_TEAMID` | `TeamID#<team_id>` |
| `ALLOW_SIGNINGID` | `SigningID#<signing_id>` |
| `ALLOW_CDHASH` | `CDHash#<cdhash>` |

The writer does not scan the rules. The CLI keeps an index of the flagged rules in one item of the store table, PK `EventCollection`, SK `Rules`: a string set `Global` of flagged global rule sort keys, a string set `Machine#<machine ID>` per Mac with flagged machine rules, and a string set `Group#<group name>` per device group with flagged group rules (`Group#rudolph:ungrouped` for the Macs in no group). It changes the rule row and the index in one transaction, with `ADD` and `DELETE` on the sets, so concurrent CLI runs never lose each other's changes. Removing a rule (`rudolph rule remove`, or `REMOVE` in `rudolph rules import`) or creating it again with `rudolph rule ...` takes it out of the index; rewriting it with `rudolph rules import` keeps its flag. Deleting a group with `rudolph group delete --force` removes its whole set.

The index is a hint, not the truth: a machine rule's row can expire or be deleted by the TTL while its entry stays. So the eventupload Lambda (store table named by `DYNAMODB_NAME`):

- reads the index only for an upload holding at least one allowed-by-rule event (an upload with none makes no extra call);
- confirms each matching rule on its row (`GetItem`, strongly consistent) before storing anything: the row must exist and be flagged, and a machine rule must still apply (not expired, not marked for removal, not a `REMOVE`);
- takes a rule whose row is gone or not flagged out of the index, in a transaction that checks the row has not been flagged again meanwhile (best effort);
- keeps the index and each confirmation for up to 60 seconds in a warm Lambda, so a change takes up to a minute to apply;
- matches hash identifiers (binary, certificate and CDHash rules) case-insensitively, as older versions stored some in upper case;
- never fails an upload because one of these reads failed: it logs it, skips the allowed-by-rule events concerned, and stores the rest.

### Group rules

A flagged group rule collects only where it is the rule that decided the execution, following the [precedence of group rules](groups.md#precedence). An allowed-by-rule execution whose rule sort key is in some `Group#<group>` set is stored when all of these hold:

- the group is registered and is one of the uploading Mac's effective groups, counted as for group rules and client modes: its assigned groups, the groups it reports while they accept reported membership, or `rudolph:ungrouped` when it has neither;
- the group rule row (`GroupRules#<group>` / `<rule sort key>`) exists, is flagged and is an allow (`ALLOWLIST` or `ALLOWLIST_COMPILER`);
- the Mac has no machine rule for the key that applies (not expired, not being removed): a machine rule beats any group rule, and then only its own flag counts;
- no other group of the Mac has a rule for the key that beats an allow: any of the four block policies, or a `CEL` rule.

A flagged global rule collects whatever the Mac's groups say, and flagged machine rules behave as before.

What it reads, all with `GetItem` on the store table and strongly consistent:

- nothing new while the index holds no `Group#` set, and nothing for an execution whose rule no group has flagged;
- otherwise, once per upload, the Mac's groups item (`Machine#<machine ID>` / `Groups`) and the group registry (`GroupRegistry` / `Current`, kept for up to 60 seconds in a warm Lambda);
- then, for each flagged rule of one of the Mac's groups that matches, the group rule row, the Mac's machine rule row for the key and the rule rows of the Mac's other groups for the key. Each row is read at most once per upload and kept for up to 60 seconds, whether it exists or not.

So an upload of a Mac in one group, holding executions of one flagged group rule, costs at most four reads more than one without (about four read units: each is strongly consistent, and the registry is the only item that can exceed 4 KB), and nothing more for the rule's further executions in it; each further group of the Mac adds one read per flagged key.

A failed read is logged with the machine ID, never the rule, and skips only the group rules' executions it concerns: every group rule's when the groups item or the registry cannot be read, one rule's when a rule row cannot. Executions collected by a global or machine rule, and everything else in the upload, are stored, and the upload succeeds. A group rule whose row is gone or not flagged is taken out of its `Group#<group>` set, in a transaction that checks `GroupRules#<group>` / `<rule sort key>` has not been flagged again meanwhile (best effort).

Santa uploads allowed-by-rule executions only when `EnableAllEventUpload` is on (`rudolph config update --machine <id> --enable-all-event-upload` for a few Macs, or `--global`); the CLI warns when a rule is flagged while it is off in the global configuration.

## Items

| Item | PK | SK | Attributes | Expires |
|---|---|---|---|---|
| Event | `Exec#<machine ID>` | `<TS>#<file SHA-256>#<pid>` | `MachineID`, `TS`, `Fleet` (always `Exec`), `Decision` (Santa's name, e.g. `BLOCK_BINARY`), `FileSHA256`, `FilePath`, `FileName`, `ExecutingUser`, `TeamID`, `SigningID`, `CDHash`, `IdentityUnverified` (boolean, only when Santa 2026.9 reported `identity_unverified`: Santa could not confirm that the file it evaluated is the image the kernel loaded, so the file hash is report-only) | 90 days after upload |
| File access event | `FileAccess#<machine ID>` | `<TS>#<pid>#<16 hex digits>`: `TS` is the access time, `pid` the first process's (0 without a process chain), and the hex digits the start of the SHA-256 of the rule name, the target and the decision, so that two accesses by one process at one instant are two items and the same event uploaded again is the same item | `MachineID`, `TS`, `Fleet` (always `FileAccess`), `Decision` (Santa's name, e.g. `FILE_ACCESS_DECISION_DENIED`), `RuleName`, `RuleVersion`, `Target` (the path accessed); of the first process in the chain (the one that accessed the path): `ProcessPath`, `ProcessName` (the last element of `ProcessPath`), `ProcessSHA256`, `ProcessSigningID`, `ProcessTeamID`, `ProcessCDHash`, `PID` (number); `ParentPath` (the second process's path); `ProcessChain` (list of every process's path, accessing process first, then each parent, at most 8) | 90 days after upload |
| Day counter | `BinaryDay#<UTC date of the execution>` (`YYYY-MM-DD`) | `<file SHA-256>` | `Blocked`, `Allowed`, `UnknownBlocked`, `UnknownAllowed` (numbers; each absent until its first event), `FileName`, `SigningID`, `TeamID` | 100 days after its first event |
| Machine marker | `BinarySeen#<file SHA-256>` | `<machine ID>` | `LastSeenDay` (UTC date, `YYYY-MM-DD`, of the Mac's latest upload of an event of the binary) | 100 days after the Mac last ran the binary |
| Unknown marker | `UnknownSeen#<machine ID>` | `<file SHA-256>` | `LastDay` (UTC date, `YYYY-MM-DD`, of the Mac's latest upload holding a `BLOCK_UNKNOWN` or `ALLOW_UNKNOWN` execution of the binary), `FileName`, `SigningID`, `TeamID` (only usable values, as on a day counter) | 35 days after that upload |
| Top list snapshot row | `TopSnapshot#<kind>#<window>#<snapshot ID>` | `<rank>` (6 digits from `000001`) | `FileSHA256`, `Count`, `Machines` (numbers; `Machines` on the first 100 rows only), `FileName`, `SigningID`, `TeamID`, `GlobalRule` (unknown lists only, on the first 100 rows, when a global rule covers the binary) | 8 days after it was written |
| Top list pointer | `Top#<kind>#<window>` | `Current` | `Snapshot` (the snapshot ID), `GeneratedAt` (RFC 3339) | never; replaced every hour |
| Fleet health list row | `FleetSnapshot#<snapshot ID>#Health#<category>` | `<order>#<machine ID>`, or `<machine ID>` alone | see [Fleet health and readiness rows](#fleet-health-and-readiness-rows) | 3 days after it was written |
| Readiness row | `FleetSnapshot#<snapshot ID>#Ready#All` and `FleetSnapshot#<snapshot ID>#Ready#Group#<group>` | `<bucket>#<Pending30, 6 digits>#<machine ID>` | see below | 3 days after it was written |
| Fleet pointer | `FleetPointer` | `Current` | `Snapshot`, `GeneratedAt` and every count, see below | never; replaced every day |

`TS` is the execution (or access) time in microseconds since the Unix epoch, rounded and zero-padded to 20 digits, so it sorts as a string. Empty attributes are left out (DynamoDB rejects an empty string in an index key such as `TeamID`). `ExpiresAfter` holds the expiry in Unix seconds; the table's TTL deletes expired items, free of charge, usually within a few days of expiry.

Indexes, all projecting every attribute and with `TS` as the sort key, so a time window is a range on `TS`:

- `Fleet_TS` (`Fleet`): every event of the fleet, newest first: executions under `Exec`, file access events under `FileAccess`.
- `Decision_TS` (`Decision`): events with one decision. File access decisions (`FILE_ACCESS_DECISION_*`) are values no execution has, so they list file access events only.
- `TeamID_TS` (`TeamID`): executions of one team ID. Sparse: unsigned binaries have no team ID, and file access events keep their process's team ID in `ProcessTeamID`, outside this index.

Day counters, machine markers, unknown markers, the top lists and the fleet job's rows have no `TS`, so they are in no index.

## How events are written

- The events of one upload that add to the same day counter (same binary, same UTC day) are written together with that counter in `TransactWriteItems` transactions of up to 99 events plus the counter update, and at most about 3 MB (DynamoDB allows 100 items and 4 MB). Each event is put only if it does not exist yet (`attribute_not_exists(PK)`), and the counter gets `ADD Blocked` or `ADD Allowed` with the number of events written, and in the same `ADD`, `UnknownBlocked` for the `BLOCK_UNKNOWN` events among them and `UnknownAllowed` for the `ALLOW_UNKNOWN` ones (the display attributes and the expiry are set only if absent; see [Display fields of a day counter](#display-fields-of-a-day-counter) for which values a counter takes). `Blocked` and `Allowed` still count every stored decision, unknown ones included; the unknown counts tell the ranker which executions no rule of any scope decided on that Mac at that time. An event is therefore never stored without being counted, nor counted twice. When Santa uploads events again that are already stored, DynamoDB cancels the transaction and says which events failed their condition; the transaction is sent again without them, adding only the others; DynamoDB may name further events on the next attempt, so a batch can take a few calls, and a batch that is entirely stored already needs no second call. An event DynamoDB refuses as invalid is dropped from the batch the same way and logged.
- Transactions on the same counter run one after another, since concurrent transactions on one item cancel each other; different counters are written in parallel, at most 16 calls at a time. A transaction cancelled by a conflicting write from another upload or by throttling is retried after a short wait, up to 5 attempts in all. Each attempt carries its own client request token, derived from its contents, the upload's expiries and the attempt number: the AWS SDK's own retries of an attempt whose response was lost reuse the token, so DynamoDB does not apply the transaction twice.
- Each file access event is a plain `PutItem`, written only if it does not exist yet (`attribute_not_exists(PK)`); one already stored by an earlier upload is skipped, not an error, and one DynamoDB refuses as invalid is dropped and logged. These puts share the pool of at most 16 calls at a time with the execution transactions.
- Each binary of the upload then gets its machine marker (`PutItem`, with `LastSeenDay` set to the UTC date of the upload), also for events stored by an earlier upload, so a marker lost with an earlier attempt is made up for. A crash-looping binary that fills a 500-event upload therefore takes about a dozen calls, and any upload of N events at most about 3N, besides retries.
- In the same pool, each binary with a stored `BLOCK_UNKNOWN` or `ALLOW_UNKNOWN` execution in the upload gets its unknown marker under the Mac (see [Unknown markers](#unknown-markers)): one `PutItem` per binary, however many of its events the upload holds, and again for events an earlier upload stored.
- The writes run alongside the sinks and share their deadline (the Lambda deadline minus 1.5 seconds, see [Delivery to the event sink](sync.md#delivery-to-the-event-sink)). After the first failed write no new writes start, but writes already running finish. A failed write, or running out of time, answers HTTP 500, so Santa keeps the events and uploads them again at its next sync; events already stored are skipped then.
- Santa retries the whole upload, not just the events this table missed, so after a table failure the Firehose, Kinesis or Lambda sinks receive the upload's records again. Sinks can already receive records twice when one sink fails after another succeeded; consumers of the sinks must tolerate duplicates.

### Display fields of a day counter
A day counter's `FileName`, `SigningID` and `TeamID` are there for the top lists, and the ranker holds them in memory for every binary of 90 days, so a counter only takes values the ranker can use:

| Attribute | Usable when | Why |
|---|---|---|
| `FileName` | at most 255 bytes | the longest file name macOS allows |
| `SigningID` | at most 255 bytes | real signing IDs (team ID, colon, bundle identifier) are far shorter |
| `TeamID` | exactly 10 upper-case letters or digits | every Apple team ID has this form |

- Each attribute comes from the first event of the upload, for that binary and day, whose value is usable; a counter that already has the attribute keeps it. With no usable value the counter has no such attribute, and the binary still ranks, without that field.
- An unusable value is skipped, never cut: the ranker looks global rules up by signing ID and team ID, and a cut value could name another rule.
- The event items are not affected: they keep their fields cut to 4,096 bytes (1,024 for `TeamID`), as above.
- The ranker applies the same rules to every counter it reads (`pkg/counterfields` holds them for both), so counters written before this check, which can hold names of up to 4,096 bytes, are read without their unusable fields until they expire.

### Unknown markers
For Lockdown readiness ([fleet-health.md](fleet-health.md#lockdown-readiness)) the fleet job needs each Mac's unknown binaries of the last 7 and 30 days from one bounded read. Nothing else in the table is keyed by Mac and binary, so eventupload writes a marker, `UnknownSeen#<machine ID>` / `<file SHA-256>`:

- **Which binaries.** Every binary with a stored `BLOCK_UNKNOWN` or `ALLOW_UNKNOWN` execution in the upload, also when the same binary has events of other decisions. Executions blocked by a rule, collected executions allowed by a rule, and events the store drops get none.
- **`LastDay`** is the UTC date of the upload from Rudolph's clock, not the execution time the Mac reports, so a Mac cannot backdate a binary. "Ran in the last N days" is `LastDay` on or after today minus N - 1.
- **Expiry** is 35 days after the upload; each later upload of the binary moves it. Readers still filter by `LastDay`, as TTL deletion lags.
- **Display fields** follow the day counters' rules ([Display fields of a day counter](#display-fields-of-a-day-counter)): each comes from the first stored event of the binary in the upload with a usable value, and an unusable value is left out, never cut.
- **Cost.** One write unit each: the marker stays under 1 KB with every field at its limit and has no `TS`, `Fleet` or `Decision`, so it enters no index (`TestCOST_UnknownMarkersAddOneWritePerBinary`).
- **Failure.** A failed marker write answers the upload HTTP 500 like any other failed write, and Santa's retry writes the marker although the events themselves are already stored.

## Top binaries

The web view's lists of the binaries blocked or allowed most often, and of the binaries no rule decided, are built by the ranker, a Lambda of its own (`PREFIX_rudolph_ranker`, code in `cmd/ranker` and `internal/ranker`) run by an EventBridge Scheduler schedule: every hour by default, or as `ranker_schedule_expression` and `ranker_schedule_timezone` set it (see [Ranker schedule](deploy.md#ranker-schedule)). A page view then reads one pointer (`GetItem`) and one snapshot (`Query`) instead of up to 90 days of day counters.

- There are twenty lists, five kinds each over windows of 1, 7, 30 and 90 days ending today (UTC; the 1-day list is today so far):
  - `Blocked` and `Allowed`: every stored decision;
  - `Unknown` (`UnknownBlocked` + `UnknownAllowed`), `UnknownBlocked` and `UnknownAllowed`: Santa's `BLOCK_UNKNOWN` (lockdown) and `ALLOW_UNKNOWN` (monitor) decisions only, the binaries an admin may want to allow or block next. The web view's "all / blocked / allowed" filter reads one of them.
- The unknown lists fill only from the day this release is deployed: older day counters have no unknown counts, so a 90-day unknown list is complete 90 days after the deploy.
- `Count` is the sum of that kind over the window's day counters, so it goes by the **execution** date. `FileName`, `SigningID` and `TeamID` come from the newest day counter that has a usable one (see [Display fields of a day counter](#display-fields-of-a-day-counter)); an unusable value is dropped as the counter is read, so the row is written without that field.
- `Machines` is the number of the binary's machine markers whose `LastSeenDay` is inside the window, so it goes by the **upload** date: a Mac that was offline for days and then uploaded an old execution counts in the 1-day window. Markers do not record the decision, so `Machines` counts the Macs that uploaded any stored execution of the binary, whether it was blocked or allowed there (allowed executions are stored for `ALLOW_UNKNOWN` and for flagged rules). It is set on the first 100 rows of each list only, as counting a binary's Macs reads one marker per Mac; the other rows leave it out.
- Each list holds the 500 binaries with the highest `Count` (binaries with a count of 0 are left out). The `Blocked` and `Allowed` lists are ordered by `Count`, highest first, then by `FileSHA256`.
- In the unknown lists, the first 100 rows (the ones with `Machines`) are ordered for deciding what rule to add: binaries without a global rule first, then `Machines` (highest first: many Macs running a binary say more than one Mac running it in a loop), then `Count` (highest first), then `FileSHA256`. Rows past 100 keep `Count` order, and the reordering never moves a row into or out of the first 100.
- `GlobalRule`, on the first 100 rows of the unknown lists, is the sort key of a global rule that now covers the binary (`Binary#<SHA-256>`, `SigningID#<signing ID>` or `TeamID#<team ID>`, the first that exists in that order). Only usable signing and team IDs are looked up: a signing ID over 255 bytes, or a team ID that is not 10 upper-case letters or digits, was dropped when the counter was read, so the lookup goes on to the next candidate. (The ranker also still refuses to look up a rule key over DynamoDB's 1,024-byte sort key limit, which no usable value reaches.) The decision was unknown when the Mac ran the binary; a row with `GlobalRule` has had a rule added since. Certificate and CDHash rules can't be matched, as the day counters don't hold them, and machine and group rules don't count: the admin reading the list decides for the whole fleet. Without the store table's name (`DYNAMODB_NAME`) the ranker logs that and leaves `GlobalRule` out.
- Each run writes every list under a new snapshot ID, which sorts by time, then moves each pointer to it, only if the pointer is absent or names an older snapshot: runs can overlap (EventBridge Scheduler may invoke it twice, an operator may invoke the Lambda by hand), and a run that started earlier must not move the pointer back. A reader therefore never sees a half-written snapshot. Snapshot rows expire 8 days after they were written, so a ranker that stops for a while never leaves a pointer naming deleted rows; the web view shows `GeneratedAt`. A failed run leaves the pointers on the previous snapshot; the next scheduled run starts again from scratch, so failed runs are not retried.
- Markers written before `LastSeenDay` existed have none and count in no window until the Mac uploads an event of that binary again.

How a run reads and writes:

- It reads each of the 90 day partitions (`BinaryDay#<date>`) once, with a paginated `Query`, and folds each page into one running total per binary for every window and kind; it never holds the counter rows, and it never scans.
- Memory: the total of one binary takes about 350 bytes with ordinary names, and at most about 800 when its file name and signing ID are both 255 bytes (20 counts, the 32 bytes of its SHA-256 and the three display fields). 100,000 distinct binaries in 90 days therefore take about 35 MB; the lists themselves are picked without a second copy. The Lambda has 256 MB, of which the runtime and the AWS SDK take a few tens: room for about 500,000 binaries with ordinary names, or about 200,000 whose names are all at the limit, far more than a fleet of a few thousand Macs sees in 90 days. Nothing bounds the number of distinct binaries: a very large fleet, or someone with the sync token uploading invented hashes (1,000 per upload, so a few hundred uploads), can still get there, and a run that exceeds the memory fails, leaving the previous lists in place, until enough counters expire or `memory_size` in `ranker.tf` is raised (512 MB doubles the room for about twice the ranker's Lambda cost, a few cents a month).
- Each run logs one line, `ranker: binaries=<n>`, the number of distinct binaries it folded. Compare it with `Max Memory Used` on the run's `REPORT` line; if that nears 256 MB, raise the memory.
- It then reads the machine markers (`Query` on `BinarySeen#<file SHA-256>`, projecting only `LastSeenDay`) of the binaries in the first 100 rows of any list, once per binary even when it is in several lists: at most 5 × 4 × 100 = 2,000 binaries, usually a few hundred as the windows overlap and the unknown lists mostly repeat binaries already in the blocked and allowed ones. It counts the Macs per window while paging and keeps nothing else.
- For the binaries in the first 100 rows of the unknown lists (each once, however many lists it is in) it looks up global rules with eventually consistent `GetItem` calls on the store table's `GlobalRules` partition, at most three per binary and stopping at the first rule found: up to about 300 point reads a run in practice.
- It writes the snapshot rows with `BatchWriteItem` (25 rows a call, retrying unprocessed rows), then the twenty pointers with a conditional `PutItem`. Calls run at most 16 at a time.
- Its role may only `Query` the `BinaryDay#*` and `BinarySeen#*` partitions and `PutItem`/`BatchWriteItem` the `TopSnapshot#*` and `Top#*` partitions of this table (`dynamodb:LeadingKeys`), and `GetItem` the `GlobalRules` partition of the store table (no other partition, no index, no `Query`, `Scan` or writes there), and write its own log group, nothing else. The store table's KMS key needs no statement of its own: its key policy lets the account's principals use it through DynamoDB (and only through DynamoDB), and names the ranker's role among Rudolph's Lambdas when `store_kms_key_user_arns` narrows it.

What a run costs, at us-east-1 on-demand prices (US$0.125 per million read units, US$0.625 per million write units; an eventually consistent `Query` costs half a read unit per 4 KB read), with 720 runs a month. The number of distinct binaries a day and how widely the listed binaries run are guesses; the shape of the estimate matters more than the figures.

| | 1,000 Macs | 100,000 Macs |
|---|---|---|
| Day counters (~250 B each) | ~2,000 binaries a day: 90 × 0.5 MB ≈ 5,500 read units | ~50,000 binaries a day: 90 × 12 MB ≈ 140,000 read units |
| Machine markers (~60 B each, one per Mac) | a few hundred binaries, at most 60 KB (~8 read units) each: ≈ 1,000 read units | ~300 binaries at ~20,000 Macs (1.2 MB, ~150 read units) each: ≈ 45,000 read units; at worst 2,000 binaries on all 100,000 Macs (6 MB, ~750 read units each): 1,500,000 |
| Global rule lookups (`GetItem`, half a read unit each) | up to ~300: ≈ 150 read units | up to ~300: ≈ 150 read units |
| Snapshot rows (≤ 1 KB each) | ~7,500 rows: 7,500 write units | 10,000 rows: 10,000 write units |
| Per run | ≈ US$0.006 | ≈ US$0.029, at worst ≈ US$0.21 |
| Per month | ≈ US$4 (reads ~US$0.60, writes ~US$3.40) | ≈ US$21 (reads ~US$17, writes ~US$4.50), at worst ≈ US$150 |

Storage of 8 days of snapshots (up to ~600 MB) adds about US$0.15 a month and the Lambda (256 MB, arm64, seconds to a couple of minutes a run) under US$0.30, about 2 cents at ten seconds a run. Reading the day counters dominates at scale and grows with the number of distinct binaries, not with how often they run. The unknown lists cost about 2.5 times the snapshot writes of the blocked and allowed lists alone (twenty lists instead of eight) plus the rule lookups, which are negligible (about US$0.01 a month); at a small fleet that more than doubles the ranker's cost, which is still a few dollars a month.

## Fleet health and readiness rows

The fleet job (`PREFIX_rudolph_fleet`, `cmd/fleet` and `internal/fleet`) writes one snapshot a day: list rows, then one pointer. What the categories and buckets mean, and how a run works, is in [fleet-health.md](fleet-health.md). `testdata/webcontract/fleet-rows.json` pins the rows rudolph-web reads.

Pointer, `FleetPointer` / `Current`:

| Attribute | Meaning |
|---|---|
| `Snapshot`, `GeneratedAt` | the snapshot ID the lists are under, and when the run started (RFC 3339) |
| `Macs` | Macs walked: every Mac with sensor data |
| `Attention` | Macs in at least one health category |
| `Health` | map of `Stale`, `Incomplete`, `Mode`, `Rules`, `SIP` and `Version` to the number of Macs listed |
| `VersionUnknown` | Macs whose Santa version is empty or cannot be parsed |
| `MinSantaVersion` | the release below which the run listed a Mac under `Version` |
| `StaleDays`, `Holiday`, `HolidayUntil` | the stale threshold the run used, whether it was a holiday's, and the holiday's last day (only while one is on) |
| `Readiness` | map of `All`, every registered group and `rudolph:ungrouped` to `Macs`, `Ready`, `Few`, `Many`, `Unmeasured` and `Locked` |
| `Capped` | Macs with more than 1,000 unknown markers |

Health list rows, `FleetSnapshot#<snapshot ID>#Health#<category>`. Every row has `MachineID`, `Hostname` and `SerialNum` (left out when the Mac reports none), `LastSeen` (the time of its last preflight), `LastCompleted` (its last completed sync; left out when there is none) and `ExpiresAfter`:

| Category | Sort key | Further attributes |
|---|---|---|
| `Stale` | last completed sync as `20060102T150405Z` (`00000000T000000Z` when none), `#`, machine ID | `Never` (boolean) |
| `Incomplete` | preflight time in the same form, `#`, machine ID | `SyncsWithoutPostflight`, `Never` |
| `Mode` | machine ID | `Reported`, `Intended` (`MONITOR`, `LOCKDOWN`, `STANDALONE`), `Source` (`temporary`, `own`, `group:<name>`, `global`, `default`) |
| `Rules` | machine ID | `Reason` (`NotApplied` or `Outlier`), `RulesReceived`, `RulesProcessed`, `Hash`, `ExpectedHash` (outliers only) |
| `SIP` | machine ID | `Flags` (hex, `0x7f`) |
| `Version` | release as `2026.08`, `#`, machine ID | `SantaVersion` |

Readiness rows, `FleetSnapshot#<snapshot ID>#Ready#All` and one per effective group of the Mac under `...#Ready#Group#<group>`:

- Sort key `<bucket>#<Pending30, 6 digits>#<machine ID>`; buckets `0` Ready, `1` Few, `2` Many, `3` Unmeasured, `4` Locked. One bucket is a `begins_with` range, so a page is one `Query` without a filter.
- `MachineID`, `Hostname`, `SerialNum`, `ReportedMode`, `IntendedMode`, `ObservedDays`, `Capped` (boolean), `ExpiresAfter`.
- `Unknown30`, `Pending30`, `Unknown7`, `Pending7`: absent on Locked rows, whose markers the job does not read.
- `Why` on Unmeasured rows only: `stale`, `incomplete`, `upload-off`, `no-rules` or `observed`.

Rows expire 3 days after they are written, and the pointer never does: a pointer whose rows are gone means the job has not run since `GeneratedAt`. Rows repeat hostname and serial number from the store table so that a list needs no read per row; the events table has the same KMS key and point-in-time recovery as the store table.

## Costs

The cost depends on how many events Santa uploads, not on how often binaries run. Each stored event costs five to eight write units: the item costs two because it is written in a transaction, plus three index entries; the day counter update (also two units in a transaction) and the machine marker are shared by the events of one binary in an upload, so they add three more per event when every event is of a different binary and almost nothing for a crash-looping binary, and about 1 KB of storage for 90 days. An unknown execution adds one more write unit for its binary's unknown marker, shared in the same way (see [Unknown markers](#unknown-markers) and [costs.md](costs.md#fleet-health-and-lockdown-readiness)). rudolph-web's `docs/data-layer-design.md`, sections "Event store options" and "D+ in detail", has the estimates by fleet size and upload setting (they assume about six write units per event, within that range); in short, with Santa's default upload setting it is a few US dollars a month for 1,000 Macs in monitor mode and under a dollar in lockdown mode. Turning on `EnableAllEventUpload` multiplies uploads about tenfold; only the events of flagged rules are stored, so the rest raise Lambda and sink costs (about US$0.30 a month for 1,000 Macs for the Lambda) but not this table's. Each flagged rule's stored executions cost the same as any other stored event, so flag a broad rule such as a widely used team ID only for as long as you need it. Flagged group rules add a few strongly consistent reads of the store table to the uploads that hold their executions (see [Group rules](#group-rules)), and none to any other upload.

A file access event costs about 3 write units: 1 for the item (a plain `PutItem` of up to 1 KB) and 1 each for its `Fleet_TS` and `Decision_TS` entries; a long process chain or long paths make the item, and so each of the three writes, larger than 1 KB and cost a unit more per KB. It takes about 1 KB of storage for 90 days. How many there are depends entirely on the FAA policy in the Macs' configuration profiles: a rule that a common process trips many times a day can upload far more events than executions, so check `file_access_stored` in the eventupload log (see [Eventupload](sync.md#eventupload)) after rolling out a new FAA policy.

Point-in-time recovery is on, as for the store table, and is billed by table size.

## Access for the web view

The `santa_api` module outputs `events_dynamodb_table_name`, `events_dynamodb_table_arn` and `store_kms_key_arn`. The web view's role needs `dynamodb:Query` and `dynamodb:GetItem` on the table ARN and `<table ARN>/index/*`. The table is encrypted with the same customer managed KMS key as the store table; while `store_kms_key_user_arns` is empty, that key's policy lets any principal in the account use it through DynamoDB, so no extra KMS grant is needed unless your organisation has tightened the key policy. Once `store_kms_key_user_arns` is set, the web view's role must be listed in it (or granted the key through IAM).

The ranker writes only the top lists, and on the store table only reads global rules (see [Top binaries](#top-binaries)). The fleet job writes only `FleetSnapshot#*` and `FleetPointer`, and reads only `UnknownSeen#*` here (see [Permissions](fleet-health.md#permissions)). The eventupload Lambda is the only other writer: it gets `dynamodb:PutItem` and `dynamodb:UpdateItem` (which also cover its transactions) on this table only, and only on its own partitions, `Exec#*`, `FileAccess#*`, `BinaryDay#*`, `BinarySeen#*` and `UnknownSeen#*` (`dynamodb:LeadingKeys`, which must be present: policy `PREFIX_rudolph_events_write_policy`), so it cannot write the ranker's `TopSnapshot#*` and `Top#*` rows or the fleet job's; it has no access to read the table. A new kind of item written by eventupload needs its partition added to that policy, or every write of it fails with `AccessDeniedException` in production (DynamoDB Local, which the tests use, does not check IAM). The same policy lets it, on the store table, `GetItem` the `EventCollection` item, the rule rows, the group registry and the uploading Mac's groups item (`dynamodb:LeadingKeys` `EventCollection`, `GlobalRules`, `GroupRegistry`, `GroupRules#*`, `Machine#*`, `MachineRules#*`; IAM cannot narrow `Machine#*` to the `Groups` sort key, so the Mac's other items are readable too), `UpdateItem` the `EventCollection` item only, and `ConditionCheckItem` the rule rows (`GlobalRules`, `GroupRules#*`, `MachineRules#*`); nothing else of that table, and no write to group rules, the registry or a Mac's partition.
