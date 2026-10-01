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
is not. Updating a rule that is being removed (`rudolph rule update --machine`) brings it back with the new policy.

A rule the Mac holds must also leave the Mac when it stops applying, even if its row is gone by then (deleted by the
TTL, by `rudolph`, or by anyone else). Rudolph therefore tracks which machine-specific rules the Mac may hold, and
sends each tracked rule that no longer applies as the global rule for the same identifier and type (so the Mac falls
back to the fleet rule), or as a `REMOVE` when there is no global rule Santa can apply. It keeps sending it until two
syncs that sent it have completed (reached postflight), then stops tracking the rule. So a rule removed while a sync is
in flight, a sync that fails half way, or a single stray postflight (see below) does not lose the removal.

Rows marked for deletion by `rudolph rule remove --machine` are deleted by the ruledownload that reads them, once the
removal is tracked; that is always the case from the first sync after the upgrade that starts with the new preflight.
A sync that was already running during the deployment (numbered by the old preflight) sends the removals but leaves the
rows to the next sync.

### Tracking storage and costs
Tracking is stored as one item per tracked rule, in its own partition (`PK = MachineRuleDelivery#<machine id>`,
`SK` = the rule's sort key, e.g. `Binary#<sha256>`; about 130 bytes), with `RemovalFirstSentInSync` set once the
rule's removal or replacement has been sent. The sync state row only holds a fixed-size summary of these items
(`MachineRulesTracked`). A sync compares it with the digest of the rules that apply now: when they are equal (nothing
added, nothing stopped applying, no removal pending) it reads and writes nothing more, so a steady-state sync costs 3
writes of about 0.5 KB each (sync state and sensor data at preflight, sync state at postflight) and 5 read operations,
whatever the number of machine rules. These count requests, not capacity: the machine rules query of every sync still
reads every machine rule row of the Mac (at 1,000 rules about 150 KB, i.e. about 20 read capacity units with eventually
consistent reads). When the summary differs, the sync reads the machine's tracking items (one strongly consistent query
per 1 MB, about 8,000 rules), looks the global rules of the rules that stopped applying up with strongly consistent
`BatchGetItem` requests (100 per request), and writes only the items that changed (`BatchWriteItem`, 25 per request).

The items and the summary cannot be written atomically, and a ruledownload can die (timeout, crash, a failing batch)
or be overtaken by a newer sync of the same Mac (Santa giving up on a slow response and starting again):
- Before changing any item, ruledownload sets the summary to `dirty`, a value that never matches a set of rules, and
  only writes the new summary after all items are written. A ruledownload that dies in between leaves `dirty`, so the
  next sync reads the items instead of trusting a stale summary. Items written before it died are kept; the next sync
  only writes what is still missing, so a large first sync that times out completes over several syncs.
- Both summary writes are conditional on the row still belonging to the ruledownload's sync (`SyncSeq`) and holding
  the value it last saw or wrote. A ruledownload overtaken by a newer sync's preflight fails instead of overwriting the
  newer sync's summary.
- Preflight copies the previous summary only if the previous sync completed. If it did not, its ruledownload may still
  be running or may have died after changing items, and preflight writes `dirty`. A preflight that read the row before
  an overlapping ruledownload changed the items therefore never writes back a summary that hides the change: that
  ruledownload's sync has not completed (Santa sends postflight only after the last rule page), so the summary written
  is `dirty`.
- Retries of throttled batch requests wait at most 0.75 s per request in total, well within the API Lambda's 10 s
  timeout.

Remaining window: two ruledownloads of the same Mac running at the same time (only after Santa abandoned a slow sync and
started another) can still interleave their item writes. That needs an admin to remove and re-add the same rule within
those seconds; the result is at worst one removal sent again later, or, in the worst interleaving, a removal that only a
clean sync delivers.

Tracking items expire (`ExpiresAfter`, the table's TTL attribute) 120 days after they were last written, and
ruledownload rewrites all of a Mac's items every 30 days (`MachineRulesTrackingRefreshedAt`), so they outlive the Mac's
sync state (which expires 90 days after its last sync) while the Mac keeps syncing, and disappear after it stops. A Mac
that comes back after its sync state expired is told to clean sync, after which it holds no machine rule from Rudolph,
so an expired item never hides a rule the Mac still has. The refresh costs one write per tracked rule per month.

Scale: a Mac's machine rules are all sent on the last page of every sync, so their number is bounded by the Lambda
response size (6 MB, roughly 30,000 binary rules) and by the ruledownload Lambda's run time when many change at once
(the first sync of a Mac with N machine rules writes N tracking items, N/25 requests; see above for timeouts). Each
machine rule costs about 130 bytes of storage for tracking while the Mac may hold it.

### Known limitation: cloned machine IDs and stray postflights
Santa sends no sync ID, so Rudolph cannot tell which sync a postflight closes. A stray postflight (a delayed duplicate,
a retry, or a second Mac with the same machine ID, e.g. a cloned image) can mark a sync as completed although the rules
it served never reached the Mac. Rudolph therefore only relies on two completed syncs: a removal is tracked until two
completed syncs sent it, and the feed cursor advances to the start of the older of the last two completed syncs. One
stray postflight between two real syncs cannot lose a rule. Two Macs sharing a machine ID are not supported: they share
one sync state, their syncs interleave, and either can miss removals and feed rules until its next clean sync. Give
every Mac its own machine ID (Santa's `MachineID` setting or the default, the hardware UUID).

## Feed cursor
An incremental sync reads the rules feed from the machine's `FeedSyncCursor`. The next sync reads from 10 minutes
before the start (`PreflightAt`) of the older of the last two completed syncs, so rules added while a slow sync was
running, rules stamped by a writer whose clock is a little behind, rules not yet visible to eventually consistent reads,
and rules served by a sync that a stray postflight closed are never skipped. With a single completed sync (a new Mac),
the cursor moves to 10 minutes before its start if that is earlier; a sync that does not complete does not move it. Rules
in the overlap are sent again, which Santa applies idempotently; feed pages hold up to 5000 rules, so the overlap
normally costs one query.

Feed rows are sorted by the clock of whoever wrote them, including the CLI on an admin's machine. A rule written by a
CLI whose clock is more than the overlap behind could land before some Macs' cursors and never reach them until their
next clean sync. The CLI therefore compares the local clock with the `Date` header of a DynamoDB response before it
writes rules (`rudolph rule ...`, `rudolph rules import`) and refuses when they differ by more than 2 minutes; fix the
clock (e.g. `sudo sntp -sS time.apple.com`) or pass `--force`. When the clock is so far off that AWS rejects the
request itself (about 5 minutes), `--force` cannot help: fix the clock.

## Sync state row
`PK = Machine#<machine id>`, `SK = SyncState`. Every preflight replaces it, postflight updates it once, and ruledownload
only writes to it when the tracked machine-specific rules change. It stays about 0.5 KB whatever the number of rules.

| Attribute | Written by | Meaning |
|---|---|---|
| `SyncSeq` | preflight | Identifies this sync: increasing, and at least the preflight time in milliseconds, so it keeps increasing when the row is lost (90-day TTL) and recreated. |
| `CompletedSyncs` | preflight | The last two syncs (`Seq`, `PreflightAt`) that completed before this one started, newest first. |
| `FeedSyncCursor` | preflight | Where this sync reads the feed from. Postflight does not move it. |
| `PreflightAt`, `PostflightAt` | preflight, postflight | When this sync started and completed. `PostflightAt` is empty until the sync completes. |
| `MachineRulesTracked` | preflight, ruledownload | Summary of the tracking items: `<count>:<hash>`, or `dirty` while they may not match it. Absent when delivery is not tracked yet. |
| `MachineRulesTrackingRefreshedAt` | preflight, ruledownload | When the tracking items' expiry was last extended. |
| `RuledownloadStartedAt`, `RuledownloadFinishedAt` | no longer written | Written by earlier versions (two writes per sync that nothing read). |

### Upgrading
**Apply the Terraform change before (or together with) deploying the Lambdas.** The store policy now allows
`dynamodb:BatchWriteItem` and writes to `MachineRuleDelivery#*` partitions. Without it, every ruledownload that records
tracking fails with AccessDenied (HTTP 500), which is every sync of a Mac with a machine rule.

Rows written by earlier versions keep working:
- A sync state without `MachineRulesTracked` means Rudolph does not know what the Mac holds. Earlier versions sent every
  machine rule row on every sync, so the first sync after the upgrade assumes the Mac holds every row of the machine,
  sends a removal (or the global rule) for each row that no longer applies, including expired rows and rows marked for
  deletion, and starts tracking. Rows whose removal the Mac never received and that DynamoDB already deleted before the
  upgrade cannot be recovered: run a clean sync (`santactl sync --clean`) on affected Macs.
- A sync state without `SyncSeq` or `CompletedSyncs` counts its own completion, if any, as the one completed sync: the
  next cursor is 10 minutes before its `PreflightAt`, which is earlier than the cursor the earlier postflight wrote. A
  sync started by an old preflight during the deployment (no `SyncSeq`) sends removals but records nothing; the next
  sync does.
- A cursor with strategy 3 issued by the previous version during the deployment is still served.
