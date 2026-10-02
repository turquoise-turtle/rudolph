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
`<timestamp>`; `ExpiresAfter` (Unix seconds, also the table's TTL attribute), `SyncSeq`, `CompletedSyncs[].Seq` and
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
| Global rule | `GlobalRules` | `<RuleType>#<identifier>` (`Binary`, `Certificate`, `TeamID`, `SigningID`, `CDHash`) |
| Rules feed entry | `RulesFeed` | `<RFC 3339 time>#<RuleType>#<identifier>` |
| Machine rule | `MachineRules#<machine id>` | `<RuleType>#<identifier>` |
| Delivery tracking | `MachineRuleDelivery#<machine id>` | `<RuleType>#<identifier>` |

Machine IDs are canonical UUIDs in the case the Mac reports them; the same UUID in another case is another machine.

Encodings:

- `RuleType` (number): 1 BINARY, 2 CERTIFICATE, 3 SIGNINGID, 4 TEAMID, 5 CDHASH.
- `Policy` (number): 1 ALLOWLIST, 2 BLOCKLIST, 3 SILENT_BLOCKLIST, 4 REMOVE, 5 ALLOWLIST_COMPILER, 6
  ALLOWLIST_TRANSITIVE (legacy, never sent to Macs), 7 CEL, 8 SILENT_GUI_BLOCKLIST, 9 SILENT_TTY_BLOCKLIST.
- `ClientMode` (number): 1 MONITOR, 2 LOCKDOWN, 3 STANDALONE. The sensor data's `ClientMode` is what the Mac reported.
- `DataType` (string): `GlobalConfig`, `MachineConfig`, `SensorData`, `SyncState`, `RulesFeed`.
- `OverrideFileAccessAction`: `NONE`, `AUDIT_ONLY`, `DISABLE`; older rows may hold legacy spellings (`AuditOnly`).
- `RemountUsbMode`: a list of strings; older rows may hold a comma separated string.
- Optional rule fields: `CustomMessage`, `CustomURL`, `EventDetailButtonLabel`, `NotificationAppName`, `CELExpr`.
  Rows from the original Rudolph may have `SHA256` instead of `Identifier`.

Tolerate, rather than require:

- attributes that are absent: optional config settings mean "not configured" (inherited from the global config by a
  machine config, otherwise not sent to Santa), and older rows lack newer attributes;
- new attributes and new row kinds;
- `PrimaryUserGroups` on sensor data (it may move to its own item to keep syncs cheap) and the sync state's tracking
  internals (`MachineRulesTracked`, `CompletedSyncs`, `SyncSeq`), which are not a stable interface.

A machine rule is in force when it has no `DeleteOnNextSync`, its `Policy` is not REMOVE and `ExpiresAfter` is absent,
0 or in the future. A rule marked `DeleteOnNextSync` is being removed (its `Policy` shows what the Mac falls back to),
and should be shown as such, not as in force. A removal that ruledownload already processed has no row left: whether
the Mac still has to be told is in the delivery tracking items (`RemovalFirstSentInSync` set while the removal is being
delivered).
