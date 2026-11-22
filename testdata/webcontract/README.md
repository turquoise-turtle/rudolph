# Web contract: rows rudolph-web reads

`rows.json` is every DynamoDB item Rudolph writes for a small deployment, in DynamoDB JSON (the wire format of the
DynamoDB API and of DynamoDB exports): a global config, a machine config, global rules of every rule type and policy
with their feed rows, machine rules (applying, removed while overriding a global rule, removed without one, expired
but not yet deleted by TTL), and a Mac that synced (sensor data, sync state, delivery tracking items).

It is generated through Rudolph's real code paths by `TestWEBCONTRACT_StoredRowsMatchTheFixture`
(`internal/handlers/webcontract_integration_test.go`), which fails when an attribute in this file disappears, changes
type or changes its value. New attributes and new rows do not fail it. To regenerate after an intended change:

```
WEBCONTRACT_UPDATE=1 DYNAMODB_LOCAL_DIR=$PWD/.dynamodb-local go test -tags integration -run TestWEBCONTRACT ./internal/handlers/
```

and tell the rudolph-web maintainers what changed.

Values that differ on every run are normalized: RFC 3339 times (also as the prefix of feed sort keys) become
`<timestamp>`; `LastSeenDay` (the UTC date of the last preflight, `2006-01-02`), the sync state's `FirstSeenDay` and the unknown markers' `LastDay` (UTC dates too) become `<day>`; `ExpiresAfter` (Unix seconds, also the table's TTL attribute), `SyncSeq`, `CompletedSyncs[].Seq` and
`RemovalFirstSentInSync` (sync numbers, at least the sync's preflight time in Unix milliseconds) become `<number>`; the
`MachineRulesTracked` digest becomes `<digest>` (the literal `dirty` is kept).

## What rudolph-web must be able to decode

Keys (`PK`, `SK`):

| Row | PK | SK |
|---|---|---|
| Global config | `GlobalConfig` | `Config` |
| Machine config | `Machine#<machine id>` | `Config` |
| Sensor data (what the Mac last reported) | `Machine#<machine id>` | `Current` |
| Sync state | `Machine#<machine id>` | `SyncState` |
| Global rule | `GlobalRules` | `<RuleType>#<identifier>` (`Binary`, `Cert`, `TeamID`, `SigningID`, `CDHash`) |
| Rules feed entry | `RulesFeed` | `<RFC 3339 time>#<RuleType>#<identifier>` |
| Machine rule | `MachineRules#<machine id>` | `<RuleType>#<identifier>` |
| Delivery tracking | `MachineRuleDelivery#<machine id>` | `<RuleType>#<identifier>` |
| Group registry (docs/groups.md) | `GroupRegistry` | `Current` |
| Group rule | `GroupRules#<group>` | `<RuleType>#<identifier>` |
| A Mac's groups (assigned, and reported groups Rudolph counts) | `Machine#<machine id>` | `Groups` |
| A Mac's reported groups as Santa sent them | `Machine#<machine id>` | `ReportedGroups` |
| A group's member (index of the Macs' `Groups` items) | `GroupMembers#<group>` | `<machine id>` |
| Audit entry ([docs/cli.md](../../docs/cli.md#the-audit-trail)) | `Audit#<YYYY-MM-DD>` (UTC day) | `<Unix nanoseconds, 19 digits>#<random hex>` |
| Unknown marker (events table, written by eventupload) | `UnknownSeen#<machine id>` | `<file SHA-256>` |

Machine IDs are canonical UUIDs in the case the Mac reports them; the same UUID in another case is another machine.

Encodings:

- `RuleType` (number): 1 BINARY, 2 CERTIFICATE, 3 SIGNINGID, 4 TEAMID, 5 CDHASH.
- `Policy` (number): 1 ALLOWLIST, 2 BLOCKLIST, 3 SILENT_BLOCKLIST, 4 REMOVE, 5 ALLOWLIST_COMPILER, 6
  ALLOWLIST_TRANSITIVE (legacy, never sent to Macs), 7 CEL, 8 SILENT_GUI_BLOCKLIST, 9 SILENT_TTY_BLOCKLIST.
- `ClientMode` (number): 1 MONITOR, 2 LOCKDOWN, 3 STANDALONE. The sensor data's `ClientMode` is what the Mac reported.
- `DataType` (string): `GlobalConfig`, `MachineConfig`, `SensorData`, `SyncState`, `RulesFeed`, `GroupRegistry`, `GroupRule`, `Audit`, and others for newer rows.
- Audit entries carry `Actor` (`cli:<user>@<host>` or `web:<Entra user>`), `Action` (e.g. `rule.add`), `Target` (`global`, `machine:<id>`, `group:<name>`, `ungrouped`), `Summary` and `RecordedAt` (RFC 3339 with nanoseconds), and never expire. Write them only through `pkg/model/audit` (`Record`/`RecordContext`, a conditional put that never overwrites) and read them newest first with `List`/`ListContext`.
- Group registry: `Groups` maps each group name to `Description`, `AcceptReported` (only then do reported groups count), `RulesVersion`, `CreatedAt`, and optionally `ClientMode` (a number, as above) and `OverrideFileAccessAction` (a string, as below): the group's client mode and file access override, absent when the group has none ([docs/groups.md](../../docs/groups.md)). A value that cannot be decoded counts as none. A Mac's `Groups` item holds `Assigned` (a string set) and `Reported` (registered groups among those it reported); a Mac in neither is in the reserved group `rudolph:ungrouped`. Group rule rows carry the rule fields of a global rule plus `Group`, and `CollectAllowedEvents` (BOOL `true`, absent when off) on an allow (`ALLOWLIST` or `ALLOWLIST_COMPILER`) whose allowed executions are collected, as on global and machine rules: set and clear it only through Rudolph's `groups.SetCollectAllowedEvents`, which keeps the collection index in step, and show the command `rudolph rule update -t <type> -i <identifier> --group <name> --collect-allowed-events=true|false` (`--ungrouped` for `rudolph:ungrouped`). Flagging changes neither the group's `RulesVersion` nor what Macs download. (The fixture's flagged group rule is `GroupRules#eng` / `TeamID#ABCDE12345`. Flagging also adds the rule's sort key to the `Group#<name>` string set of the events table's collection index item (`EventCollection` / `Rules`), which is Rudolph's own and not for rudolph-web to read.) Group member rows (`DataType` `GroupMember`) mirror the Mac's `Groups` item: `Assigned` and `Reported` are BOOL `true` when the group is in that set and absent otherwise, with `UpdatedAt`; Rudolph writes them in the same transaction as the `Groups` item and deletes a row with neither flag.
- Sync state `FirstSeenDay` (string, `2006-01-02`): the UTC date of the Mac's first sync on record; absent on rows written before it existed.
- Unknown marker (events table): one per Mac and binary with a `BLOCK_UNKNOWN` or `ALLOW_UNKNOWN` execution, `LastDay` (UTC date of the Mac's latest upload of one), `ExpiresAfter` (35 days after that upload), and `FileName`, `SigningID`, `TeamID` when usable (`pkg/counterfields`). Count "ran in the last N days" as `LastDay >= today - (N - 1)`; expired markers may linger until DynamoDB's TTL deletes them.
- `OverrideFileAccessAction`: `NONE`, `AUDIT_ONLY`, `DISABLE`; older rows may hold legacy spellings (`AuditOnly`).
- `RemountUsbMode`: a list of strings; older rows may hold a comma separated string.
- Optional rule fields: `CustomMessage`, `CustomURL`, `EventDetailButtonLabel`, `NotificationAppName`, `CELExpr`.
  Rows from the original Rudolph may have `SHA256` instead of `Identifier`.

Tolerate, rather than require:

- attributes that are absent: optional config settings mean "not configured" (inherited from the global config by a
  machine config, otherwise not sent to Santa), and older rows lack newer attributes. A machine config may hold any
  subset of the settings, or none, including `ClientMode`, `BatchSize`, `FullSyncInterval` and `UploadLogsUrl`: every
  setting it lacks follows the global config;
- a `CleanSync` attribute on the global config, written by earlier versions: it is ignored, as only a machine can be
  asked for a clean sync;
- new attributes and new row kinds;
- the reported primary user groups, which live on their own `Machine#<id>` / `ReportedGroups` item (`PrimaryUserGroups`, `Time`), written only when they change (they used to be on sensor data), and the sync state's tracking
  internals (`MachineRulesTracked`, `CompletedSyncs`, `SyncSeq`), which are not a stable interface.

A machine rule is in force when it has no `DeleteOnNextSync`, its `Policy` is not REMOVE and `ExpiresAfter` is absent,
0 or in the future. A rule marked `DeleteOnNextSync` is being removed (its `Policy` shows what the Mac falls back to),
and should be shown as such, not as in force. A removal that ruledownload already processed has no row left: whether
the Mac still has to be told is in the delivery tracking items (`RemovalFirstSentInSync` set while the removal is being
delivered).

## Fleet health and readiness rows (`fleet-rows.json`)

`fleet-rows.json` holds the rows the fleet job (`internal/fleet`, run daily by `cmd/fleet`) writes to the events table for one fixed scenario at a fixed time: the pointer and one row of each list rudolph-web pages. It is checked by `TestWEBCONTRACT_FleetRows` (`internal/fleet/webcontract_integration_test.go`) and regenerated the same way, with `WEBCONTRACT_UPDATE=1 ... go test -tags integration -run TestWEBCONTRACT ./internal/fleet/`. The snapshot ID becomes `<snapshot>`.

| Row | PK | SK |
|---|---|---|
| Pointer and counts (never expires) | `FleetPointer` | `Current` |
| Health list row | `FleetSnapshot#<snapshot>#Health#<Stale\|Incomplete\|Mode\|Rules\|SIP\|Version>` | `<order>#<machine id>` (Stale, Incomplete, Version) or `<machine id>` (Mode, Rules, SIP) |
| Readiness row | `FleetSnapshot#<snapshot>#Ready#All` or `FleetSnapshot#<snapshot>#Ready#Group#<group>` | `<bucket>#<Pending30, 6 digits>#<machine id>` |

- Pointer: `Snapshot`, `GeneratedAt`, `Macs`, `Attention`, `Health` (map of the six categories to counts), `VersionUnknown`, `MinSantaVersion`, `StaleDays` (the stale threshold the run used), `Holiday` (BOOL) and `HolidayUntil` (only while holiday mode is on), `Readiness` (map of `All`, every registered group and `rudolph:ungrouped` to `Macs`, `Ready`, `Few`, `Many`, `Unmeasured`, `Locked`), `Capped`.
- Health row orders: Stale by the last completed sync as `20060102T150405Z` (`00000000T000000Z` when none), Incomplete by `PreflightAt` in the same form, Version by the release as `2026.08`; all oldest first. Modes are written as `MONITOR`, `LOCKDOWN`, `STANDALONE`; `Source` is `temporary`, `own`, `group:<name>`, `global` or `default`; SIP `Flags` is hex (`0x7f`).
- Buckets: `0` Ready, `1` Few (1 to 9 pending), `2` Many (10 or more, or capped), `3` Unmeasured (`Why`: `stale`, `incomplete`, `upload-off`, `no-rules`, `observed`), `4` Locked (no unknown counts: the job does not read a locked Mac's markers).
- List rows expire 3 days after they are written (`ExpiresAfter`); a pointer whose rows are gone means the job has not run since.
