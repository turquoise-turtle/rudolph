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
`<timestamp>`; `LastSeenDay` (the UTC date of the last preflight, `2006-01-02`) becomes `<day>`; `ExpiresAfter` (Unix seconds, also the table's TTL attribute), `SyncSeq`, `CompletedSyncs[].Seq` and
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
| Audit entry ([docs/cli.md](../../docs/cli.md#the-audit-trail)) | `Audit#<YYYY-MM-DD>` (UTC day) | `<Unix nanoseconds, 19 digits>#<random hex>` |

Machine IDs are canonical UUIDs in the case the Mac reports them; the same UUID in another case is another machine.

Encodings:

- `RuleType` (number): 1 BINARY, 2 CERTIFICATE, 3 SIGNINGID, 4 TEAMID, 5 CDHASH.
- `Policy` (number): 1 ALLOWLIST, 2 BLOCKLIST, 3 SILENT_BLOCKLIST, 4 REMOVE, 5 ALLOWLIST_COMPILER, 6
  ALLOWLIST_TRANSITIVE (legacy, never sent to Macs), 7 CEL, 8 SILENT_GUI_BLOCKLIST, 9 SILENT_TTY_BLOCKLIST.
- `ClientMode` (number): 1 MONITOR, 2 LOCKDOWN, 3 STANDALONE. The sensor data's `ClientMode` is what the Mac reported.
- `DataType` (string): `GlobalConfig`, `MachineConfig`, `SensorData`, `SyncState`, `RulesFeed`, `GroupRegistry`, `GroupRule`, `Audit`, and others for newer rows.
- Audit entries carry `Actor` (`cli:<user>@<host>` or `web:<Entra user>`), `Action` (e.g. `rule.add`), `Target` (`global`, `machine:<id>`, `group:<name>`, `ungrouped`), `Summary` and `RecordedAt` (RFC 3339 with nanoseconds), and never expire. Write them only through `pkg/model/audit` (`Record`/`RecordContext`, a conditional put that never overwrites) and read them newest first with `List`/`ListContext`.
- Group registry: `Groups` maps each group name to `Description`, `AcceptReported` (only then do reported groups count), `RulesVersion`, `CreatedAt`. A Mac's `Groups` item holds `Assigned` (a string set) and `Reported` (registered groups among those it reported); a Mac in neither is in the reserved group `rudolph:ungrouped`. Group rule rows carry the rule fields of a global rule plus `Group`.
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
