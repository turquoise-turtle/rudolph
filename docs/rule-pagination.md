# Paginated Ruledownload in Depth
Rudolph can house hundreds of thousands of rules. Naturally, not all of these rules can be downloaded in a single API call, or even a single sync process for that matter.

Santa and Rudolph work together to implement a server-sided method of paginating through rules and ensuring that they stay in sync while passing a minimal amount of data over each request/response.

## How Pagination Works (From Santa’s Perspective)
Pagination only occurs on the /ruledownload endpoint.

In the first /ruledownload request of every sync process, is POST’d with no postbody. In the server response, the server can instruct the client to paginate by returning a cursor in the response body.

Upon receiving a cursor in the response body, the client sends a follow-up POST /ruledownload request with a postbody containing the cursor, verbatim. The server can continue to instruct the client to keep paginating until no pages remain. To instruct the client to stop, the server simply returns no cursor in the response body.

## How Rudolph implements Pagination

The initial request will contain a cursor which is how Santa agents keep track of what to download.

```json
{
    "cursor": ""
}
```

A full clean sync will occur, in which all GlobalRules are returned and a clean-sync is set to occur.

Additional calls will use the Pagination Strategy Method which is set in the cursor to instruct what sync needs to happen.

### Strategies Definitions:
- 1 = Downloads all GlobalRules (pages of the configured batch size)
- 2 = Downloads all FeedRules which is determined via the last sync state time (pages of up to 5000 rules)
- 3 = Downloads all MachineRules. Only cursors issued by earlier versions of Rudolph use it: the machine-specific
  rules are now sent at the end of the last GlobalRules or FeedRules page.

## Machine-specific rules
Machine-specific rules are sent at the end of the last page of every sync, after the global rules or the feed, so that
they take precedence. Only rules that apply are sent: a rule that expired (`ExpiresAfter`, which is also the table's
TTL attribute; DynamoDB can take days to delete an expired row) or that was removed (`rudolph rule remove --machine`)
is not.

A rule the Mac holds must also leave the Mac when it stops applying, even if its row is gone by then (deleted by the
TTL, by `rudolph`, or by anyone else). Rudolph therefore records on the machine's sync state row which
machine-specific rules the Mac holds, and sends each held rule that no longer applies as the global rule for the same
identifier and type (so the Mac falls back to the fleet rule), or as a `REMOVE` when there is no global rule Santa can
apply. This is repeated on every sync until a sync that sent it completes (reaches postflight), so a rule removed while
a sync is in flight, or a sync that fails half way, does not lose the removal.

Rows marked for deletion by `rudolph rule remove --machine` are deleted by the next ruledownload that reads them.

## Feed cursor
An incremental sync reads the rules feed from the machine's `FeedSyncCursor`. When a sync completes, the next sync
reads from 10 minutes before the completed sync's preflight (`PreflightAt`), so rules added while a slow sync was
running, rules stamped by a writer whose clock is a little behind, and rules not yet visible to eventually consistent
reads are never skipped. A sync that does not complete does not move the cursor. Rules in the overlap are sent again,
which Santa applies idempotently.

## Sync state row
`PK = Machine#<machine id>`, `SK = SyncState`. Every preflight replaces it, postflight updates it once, and ruledownload
only writes to it when the machine-specific rules it sends differ from those the Mac holds. A steady-state sync costs
three writes (sensor data and sync state at preflight, sync state at postflight).

| Attribute | Written by | Meaning |
|---|---|---|
| `FeedSyncCursor` | preflight | Where this sync reads the feed from. Postflight no longer moves it: the next preflight derives the next cursor from `PreflightAt` if `PostflightAt` is set. |
| `PreflightAt`, `PostflightAt` | preflight, postflight | When this sync started and completed. `PostflightAt` is empty until the sync completes. |
| `MachineRulesDelivered` | preflight | List of rule sort keys (`Binary#<sha256>`, `TeamID#<team id>`, ...) of the machine-specific rules the Mac may hold as this sync starts. |
| `MachineRulesServed` | preflight, ruledownload | The same for the rules this sync sent. If the sync completes, the next preflight takes it as `MachineRulesDelivered`; if not, the union of both, since the Mac may have applied them. |
| `RuledownloadStartedAt`, `RuledownloadFinishedAt` | no longer written | Written by earlier versions (two writes per sync that nothing read). |

### Upgrading
Rows written by earlier versions keep working:
- A sync state without `MachineRulesDelivered` (or with `NULL`) means Rudolph does not know what the Mac holds. Earlier
  versions sent every machine rule row on every sync, so the first sync after the upgrade assumes the Mac holds every
  row of the machine, sends a removal (or the global rule) for each row that no longer applies, including expired rows
  and rows marked for deletion, and starts tracking. Rows whose removal the Mac never received and that DynamoDB
  already deleted before the upgrade cannot be recovered: run a clean sync (`santactl sync --clean`) on affected Macs.
- A sync state written by an earlier preflight and completed by an earlier postflight gets its next cursor from its
  `PreflightAt`, which is earlier than the cursor that postflight wrote, so nothing is skipped.
- A cursor with strategy 3 issued by the previous version during the deployment is still served.

