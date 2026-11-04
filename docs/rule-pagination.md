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
- Preflight copies the previous summary only if the previous sync's ruledownload finished: its postflight arrived, or ruledownload recorded serving its last page (`LastPageServedAt`, see "Macs whose postflight never arrives"), which it does only after its tracking writes, whether or not that sync later counts as completed. Otherwise its ruledownload may still be running or may have died after changing items, and preflight writes `dirty`. A preflight that read the row before an overlapping ruledownload changed the items therefore never writes back a summary that hides the change: Santa sends postflight only after the last rule page, and that ruledownload had not yet recorded serving it, so the summary written is `dirty`.
- Retries of throttled batch requests wait at most 0.75 s per request in total, well within the API Lambda's 10 s
  timeout.

- Item writes (`BatchWriteItem`) cannot be conditional, so the item writes of a ruledownload overtaken by a newer sync
  (Santa gave up on a slow sync and started another) can land after the newer sync recorded its own items and
  summary, e.g. deleting the item of a rule the admin re-added in between. A ruledownload whose summary write fails
  after it sent item writes (or whose item writes fail) therefore replaces the summary, on whichever sync's row, with
  `dirty:overtaken`. Like `dirty` it makes the next sync read the items and bring them back in line with the rules that
  apply (a rule that applies but is not tracked is tracked again); unlike `dirty`, a newer ruledownload still writing
  its items cannot overwrite it with its own summary (its conditional write fails and that sync is retried).

Remaining window: a ruledownload overtaken while its item writes are in flight that then dies before it marks the
summary (e.g. a Lambda timeout at that moment), or whose marking lands between the next preflight's read and write of
the sync state. That needs an admin to remove and re-add the same rule within those seconds; the result is at worst
one removal sent again later, or, in the worst interleaving, a removal that only a clean sync delivers.

Tracking items expire (`ExpiresAfter`, the table's TTL attribute) 120 days after they were last written, and
ruledownload rewrites all of a Mac's items every 30 days (`MachineRulesTrackingRefreshedAt`), so they outlive the Mac's
sync state (which expires 90 days after its last sync) while the Mac keeps syncing, and disappear after it stops. A Mac
that comes back after its sync state expired is told to clean sync, after which it holds no machine rule from Rudolph
other than those the clean sync sent. Its items written before the gap may still exist, and when they were last
extended was lost with the sync state, so the recreated sync state has no `MachineRulesTrackingRefreshedAt` and its
first ruledownload extends every item kept: an item never expires while the Mac may still hold its rule. The refresh
costs one write per tracked rule per month.

A clean sync that has no rule to send (no global rule, and no machine rule applies) sends a single `REMOVE` of a
placeholder binary identifier (`000…0`, 64 zeros). Santa clears its rules at a clean sync only when the download holds
at least one rule it can parse; without it, a Mac whose machine rules, sync state and tracking all expired while it
was offline would keep those rules forever.

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

### Macs whose postflight never arrives
Santa ignores a failed postflight. A Mac whose postflights never reach Rudolph (a broken route or proxy, a postflight Lambda failing) would otherwise never have a completed sync: its feed cursor would never advance, so every sync would re-send every feed change since its first sync, and no removal would ever count as delivered. So after syncs without postflight (`SyncsWithoutPostflight` > 0), and on every clean sync, ruledownload records when it served a sync's last page (`LastPageServedAt`, one extra conditional write per such sync; clean syncs are rare, about one per Mac per periodic clean sync), and the next preflight decides whether that sync counts as completed. The ruledownload cursor carries the sync's `SyncSeq`: a later page requested by an older sync after a newer sync of the Mac started (Santa gave up on it) is refused (HTTP 500) before it writes anything, so it can neither change tracking items nor be recorded as the newer sync's.

A served last page is not proof that the Mac applied it: the response may have been lost, or santad may have failed to apply it. Counting such syncs as completed would let the feed cursor skip, and removals count as delivered, for rules the Mac never got. A feed rule or removal is lost (until the next clean sync) when two syncs that carried it count as completed without the Mac applying either: the cursor moves to the older of the last two completed syncs, and a removal counts as delivered once two completed syncs sent it. Every preflight stores the `rules_hash` the Mac reports (`PreflightRulesHash`), and the next preflight counts the sync as completed when:
- **The hash changed.** Santa applies all the rules of a sync in one transaction after downloading the last page, and only that apply changes the Mac's rules between two of its preflights, so the Mac applied the sync, whether or not its last page was recorded (it is not for the first sync after a postflight that arrived). Such a sync is marked `Applied` in `CompletedSyncs`. Known exotic case: Santa stops waiting for santad's apply after 300 seconds; an apply that only finishes after the next sync's preflight is credited to that next sync instead.
- **Its last page was served and it served nothing new** (`LastPageServedNothingNew`, recorded in the same write). Counting it cannot lose anything, whether or not the Mac applied it. An incremental sync serves nothing new when every feed row it served is stamped at or before the start of the covering sync, and every removal it served was first sent by the covering sync or earlier. The covering sync is the newest completed sync proven applied among the last two; otherwise the older of the last two completed syncs (either of which, if it really reached the Mac, read the feed up to at least that time), or the only one (a machine's first completed sync is a clean sync a postflight or its hash confirmed). Machine rules that apply do not matter: they are sent on every sync. Clean syncs never count this way.
- **Its last page was served, it is not a clean sync, and the fallback allows it.** After a completed sync that did not count through the fallback, the next such sync counts at once: whatever it carried, it is the first count to carry it. After a sync that counted through the fallback, the next one counts only once the newest content that sync carried has been served by 8 syncs (`FallbackContentServings`, counted from its first serving with `LastPageServedRun`, which starts again at 1 whenever a sync serves content the previous sync did not: a feed row stamped after the previous sync started, recorded on the sync state as `PreviousPreflightAt`, a removal sent for the first time, or any clean sync). So no content is carried by a second count before 8 syncs served it, and whatever arrives, a sync counts at least every 8 syncs. The fallback covers what nothing else shows: a Mac that reports no hash, and content that leaves the hash unchanged even when applied: an edit of a custom message or URL (Santa leaves them out of the hash), a rule written again as it was, or the REMOVE of a rule the Mac does not hold (Rudolph cannot tell such a REMOVE from one that changes the Mac: the Mac may hold an identical rule from elsewhere, e.g. a global rule the machine rule shadowed).

A clean sync never counts as completed through the fallback: until one completes, the Mac may lack every rule written before it (e.g. after its sync state expired while it was away), and the incremental syncs after it would never send them. A clean sync that sends the rules the Mac already holds leaves the hash unchanged, so to keep a Mac whose postflights never arrive from repeating its periodic clean sync (every rule) on every sync, a clean sync whose last page was served counts as performed for scheduling the next periodic clean sync, when the Mac was already missing postflights (`SyncsWithoutPostflight` > 0) and an earlier clean sync counted (`PreviousCleanSync`). This cannot lose a rule even if the Mac never applied it: the feed cursor and removals still rely only on completed syncs, and the Mac received every rule through the earlier clean sync and the syncs since; at worst the periodic refresh comes one period late. A Mac whose postflights arrive gets a postflight for a clean sync it performed, so when one does not complete (e.g. its last response was lost) the next sync is a clean sync again.

Worst cases, all for Macs whose postflights do not arrive (a postflight that arrives completes its sync as before):
- **Loss.** A feed rule or removal is lost only after **8 syncs in a row that served it failed after their last page was served**, or 9 when the first of them is also the first sync whose postflight did not arrive (its last page is not recorded, so its serving is not counted). This holds however the content arrives (with recurring writes, clean syncs or expiring machine rules in between), with or without a rules hash: the first count that carries it may come at its first serving, the second only at its eighth. 8 is the smallest value with which 8 such syncs after a postflight lose nothing.
- **Late feed rows.** The "nothing new" rule cannot see a feed row written late with a stamp at or before the covering sync's start, which that sync did not read: written by a CLI whose clock is behind (by at most the 2 minutes the CLI's clock check allows), not yet visible to that sync's eventually consistent read (normally under a second), or written in the same second as that sync started (stamps have one-second resolution). The next sync re-reads the 10 minutes before the covering sync's start (the feed cursor's overlap) and serves the row, but on a Mac whose postflights do not arrive it counts as serving nothing new, so **a single failed sync after its last page was served loses such a row** until the next clean sync. This is as if a stray postflight arrived for every failed sync, which before this mechanism lost late rows only when a stray postflight did; the window is that narrow (minutes for a CLI with a skewed clock, about a second otherwise).
- **Cost.** After the Mac's rules stop changing, it re-reads the rows of the last overlap for a sync or two, after which every sync serves nothing new and reads only new rows. Content that leaves the hash unchanged even when applied is sent again by up to 8 syncs before the cursor moves past it, or before a REMOVE that changes nothing is confirmed. Whatever keeps arriving, a sync counts at least every 8 syncs, so the feed cursor falls at most about 16 syncs behind. A Mac that reports no hash and has never completed a clean sync (new, or its sync state expired) clean syncs on every sync until a postflight arrives. Santa 2026.8 always reports a hash.

`rudolph rules import` does not write rules whose stored policy, custom message, custom URL, CEL expression, button label, notification app name and description are already those of the file, so a scheduled import of an unchanged file writes no feed rows at all.

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
| `CompletedSyncs` | preflight | The last two syncs (`Seq`, `PreflightAt`, and `Applied` when the Mac's rules hash proved it applied the sync) that completed before this one started, newest first. |
| `FeedSyncCursor` | preflight | Where this sync reads the feed from. Postflight does not move it. |
| `PreflightAt`, `PostflightAt` | preflight, postflight | When this sync started and completed. `PostflightAt` is empty until the sync completes. |
| `MachineRulesTracked` | preflight, ruledownload | Summary of the tracking items: `<count>:<hash>`, or `dirty` / `dirty:overtaken` while they may not match it. Absent when delivery is not tracked yet. |
| `MachineRulesTrackingRefreshedAt` | preflight, ruledownload | When the tracking items' expiry was last extended; empty on a recreated sync state (the next ruledownload extends them). |
| `LastCleanSync` | preflight | When the last clean sync started; for a clean sync, its own start. |
| `PreviousCleanSync` | preflight | On a clean sync: the last clean sync before it that counts as performed. If this clean sync does not complete (e.g. a replayed preflight), the next preflight uses this value for the periodic clean sync instead of `LastCleanSync`, unless this clean sync's last page was served, `SyncsWithoutPostflight` > 0 and `PreviousCleanSync` is set (see "Macs whose postflight never arrives"). |
| `SyncsWithoutPostflight` | preflight | How many syncs in a row before this one received no postflight. |
| `PreviousPreflightAt` | preflight | When the previous sync started, for this sync's ruledownload to tell which feed rows it serves first. |
| `ContentRun` | preflight | The previous sync's `LastPageServedRun`, or 0 when it completed or recorded no served last page. |
| `FallbackContentServings` | preflight | When the last completed sync counted through the fallback: how many syncs, this one included, have served the newest content it carried; 0 otherwise. |
| `LastPageServedAt` | ruledownload | When the last page was served; only written on a clean sync, or while `SyncsWithoutPostflight` > 0. The sync counts as completed when the next preflight's `rules_hash` differs from `PreflightRulesHash` (also without this), when `LastPageServedNothingNew` is set, or, except for a clean sync, when `FallbackContentServings` is 0 or at least 8. |
| `LastPageServedNothingNew` | ruledownload | Written with `LastPageServedAt`: this incremental sync served no feed row stamped after, and no removal first sent after, the covering completed sync. |
| `LastPageServedRun` | ruledownload | Written with `LastPageServedAt`: how many syncs in a row, this one included, served the newest content it served without completing. |
| `PreflightRulesHash` | preflight | The `rules_hash` the Mac reported in this sync's preflight (at most 128 bytes, a 64-character hex digest from Santa); absent when it reported none. |
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
- A clean sync started by an earlier version that has not completed (no `PreviousCleanSync`) does not count as a
  clean sync: the next sync is a clean sync again.
- A sync state without `PreflightRulesHash` (written by an earlier version) is never proven applied by the hash. One without `LastPageServedNothingNew` never counts as having served nothing new; one without `LastPageServedRun` or `ContentRun` starts its run again, and one without `FallbackContentServings` is treated as following a completed sync that did not count through the fallback.
- A ruledownload cursor without `seq` (issued by an earlier version) records the served last page for whichever sync the sync state belongs to, as before.
