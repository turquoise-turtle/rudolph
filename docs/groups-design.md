# Device groups: design

Status: built 2026-11-06 (user documentation: [groups.md](groups.md)), except `rudolph group members`, the `GroupMembers#` rows and `rudolph rules import|export --group`; see *As built* below. Owner decisions: a Mac's groups are the union of the groups Santa reports and groups assigned in Rudolph; Macs in no group can be targeted ("ungrouped"); for the same rule type and identifier on one Mac, a machine rule beats a group rule, which beats a global rule, and between two groups of one Mac a block beats an allow.

## Where groups come from

Santa 2026.8 sends `primary_user_groups` in every preflight (`sync_v1.proto`, field 21). Santa takes them from the `MachineOwnerGroups` key of its configuration profile (an array of strings) when that is set and not empty, and otherwise from a plist on the Mac: `MachineOwnerPlist` names the file and `MachineOwnerGroupsKey` the array in it (`SNTConfigurator.mm`, `machineOwnerGroups`). The primary user works the same way (`MachineOwner`, or `MachineOwnerPlist` + `MachineOwnerKey`).

- A small custom group: put its name in `MachineOwnerGroups` in the Santa profile scoped to those Macs, or have the MDM write the plist.
- Groups can also be assigned in Rudolph: `rudolph group add --machine <id> <group>`.
- A reported group only counts once it is registered in Rudolph (`rudolph group create <group>`), so a Mac reporting directory groups Rudolph does not know gains nothing.
- Trust: a profile key cannot be changed on the Mac. A plist can be edited by a local administrator, who could add a group (gaining its allows) or remove one (escaping its blocks, when the profile key is not set). Reported membership is therefore advisory; `group create --no-reported` makes only Rudolph-assigned membership count for a group that grants privilege or must stick. Assigned groups cannot be removed from the Mac.

## Recommended approach: group rules sent with the machine rules

Today every sync ends with the Mac's machine rules, after the global pages of a clean sync or the feed pages of an incremental one (`internal/handlers/ruledownload/machine_sync.go`), and delivery tracking (`pkg/model/machinerules/delivery.go`) keys on the rule's sort key: when a key stops applying, the global rule (with its fields) or a `REMOVE` is sent until two completed syncs confirm it. Group rules join that tail: for every key covered by a machine rule or by a rule of one of the Mac's groups, one winner is sent each sync. Joining a group adds keys, leaving one removes them (the global rule comes back, or a `REMOVE`), a key still provided by another level stays, and an edit is simply re-sent. No group feed, cursor or new completion logic is needed.

Rejected alternative: a feed per group with a cursor per group per Mac. It only downloads changes, but joining needs a mini clean sync, leaving still needs the tracking above, every global change must be checked against each Mac's group rules, and the sync state would carry N cursors through the most delicate code (`pkg/model/syncstate/handover.go`). Worth revisiting only if one Mac's group rules exceed about 5,000.

## Data

| Row | Keys | Attributes | Written by |
|---|---|---|---|
| Group registry | `GroupRegistry` / `Current` | `Groups`: name → {`Description`, `AcceptReported`, `RulesVersion`, `CreatedAt`}; `MembershipVersion`; `DataType=GroupRegistry` | CLI |
| Group rule | `GroupRules#<group>` / `<RuleType>#<identifier>` | rule fields as on global rules, `Description`, `Group`, `DataType=GroupRule`; no feed row, no `GlobalRulePolicy` | CLI, in a transaction that bumps the group's `RulesVersion` |
| Machine groups | `Machine#<id>` / `Groups` | `Assigned` (string set, CLI), `Reported` (normalised, registered groups that accept reported membership), `UpdatedAt` | CLI and preflight, separate attributes |
| Reported groups | `Machine#<id>` / `ReportedGroups` | `PrimaryUserGroups` as reported, `Time`, `ExpiresAfter` (90 days) | preflight, only when the list changes |
| Group members | `GroupMembers#<group>` / `<machine id>` | `Source` (assigned or reported), `ExpiresAfter` for reported members | CLI and preflight (for listing members) |
| Sync state | existing | `GroupsBasis`: hash of the reported list and `MembershipVersion` | preflight |

`PrimaryUserGroups` leaves the sensor data row, which today rewrites it on every preflight (the COST2 test with 256 groups costs 18 write units per sync).

Preflight normalises the reported groups against a cached registry (60 s, like `machineconfiguration/cache.go`) and writes the Groups item only when the basis changed. Ruledownload reads the Mac's Groups item (one GetItem), takes `Assigned ∪ Reported` restricted to registered groups (or the reserved `rudolph:ungrouped` when empty), reads group rules from a cache keyed by group and `RulesVersion`, and resolves one winner per key.

## Precedence

Per rule type and identifier: an applying machine rule wins; otherwise the strongest group rule; otherwise the global rule, which the Mac already has. Between groups, strongest first: `BLOCKLIST`, `SILENT_TTY_BLOCKLIST`, `SILENT_GUI_BLOCKLIST`, `SILENT_BLOCKLIST`, `CEL` (a conditional block beats an unconditional allow), `ALLOWLIST`, `ALLOWLIST_COMPILER`; for equal policies the group whose name sorts first. `REMOVE` and `ALLOWLIST_TRANSITIVE` cannot be group rules. Rules of different types are all sent and Santa's own order decides (CDHash, binary, signing ID, certificate, team ID): a group's signing ID block on Gemini beats the global Google team ID allow.

## Names and limits

Names are NFC-normalised, trimmed and lower-cased; empty names, the names `.` and `..` (path segments that browsers and routers resolve away in rudolph-web's `/groups/<name>`), control characters, names over 128 bytes and the `rudolph:` prefix are refused at the CLI and ignored (logged) when reported. At most 64 registered groups and 5,000 rules per group. Santa may still report up to 256 groups; only registered ones count.

## Commands

- `rudolph group create|delete [--force]|list|get|update <group> [--description …] [--no-reported]`
- `rudolph group add|remove --machine <id> <group>`, `rudolph group members <group>`, `rudolph group show --machine <id>` (assigned, reported, effective, ungrouped or not)
- `rudolph rule … --group <group>` and `--ungrouped`, a third target besides `--global` and `--machine` (exactly one is required); `--expires`/`--permanent` are refused for groups
- `rudolph rules import|export --group <group>`

## Costs

A steady-state sync stays at 3 write units, plus half a read unit for the Groups item. A membership change costs 1–2 write units at preflight plus about one per newly tracked rule; a group rule added or removed about 3 write units once per member Mac; an edit nothing extra per Mac. Joining a 1,000-rule group costs about 1,000 write units once, spread over several syncs if needed.

## Rollout

Apply Terraform (IAM for the new partitions) before the Lambda. Each Mac writes its Groups item once at its first preflight after the deploy. Without group rows behaviour is unchanged; rolling back sends the global rule or a `REMOVE` for tracked group keys.

## Tests to write first

End to end through fakesanta (`internal/handlers/groups_integration_test.go`): reported and assigned membership and their union; ungrouped rules and what happens when a Mac joins a group; leaving a group restores the global rule or removes; a key provided by two groups or a machine rule survives leaving one; the precedence table; group rule add, edit and remove reach Macs on incremental syncs; a global change does not clobber a group winner, nor does a clean sync; removals delivered without postflights; name normalisation and limits; `--no-reported`; COST2 with 3 groups and 500 group rules (steady state at most 4 write units and 8 read units); web contract rows; a group rule change visible within the cache lifetime.

## As built

Differences from the design above, and choices it left open:

- **Reported membership.** Preflight normalises the reported list against the registry it caches (60 s per instance), keeps the registered groups that accept reported membership, and stores them as `Reported` on `Machine#<id>`/`Groups` (a string set, removed when empty). The sync state's `GroupsBasis` is a digest of that stored list only, not of `MembershipVersion`: assigned membership is read at every ruledownload anyway, so a change elsewhere in the fleet makes no Mac rewrite its item. A group created, deleted or switched to `--no-reported` changes a Mac's `Reported` at its first preflight after the cache expires; ruledownload also re-checks `AcceptReported` against its own registry. The raw list stays on `Machine#<id>`/`ReportedGroups`.
- **Ruledownload** skips all group work while the registry is empty or missing. Otherwise it reads the Groups item strongly consistently (preflight may just have written it), and group rule partitions are read strongly consistently and cached per server instance by group and `RulesVersion`. Winners are appended after the machine rules, sorted by rule sort key, and added to the `applying` set, so delivery tracking is unchanged. Stored group rules with a policy a group rule cannot have, or whose sort key does not match their type, are skipped.
- **Ungrouped** is registered by the CLI (with `AcceptReported` false) the first time an `--ungrouped` rule is written; it does not count towards the 64 groups.
- **CLI writes.** `rule … --group` puts the rule row and increments the group's `RulesVersion` in one transaction (conditional on the group being registered); `rule remove … --group` deletes it the same way. `rule update` refuses group targets: writing the rule again replaces it. `group delete --force` deletes the group's rules first, then the registry entry; assigned membership is left on the Macs, inert while no group of that name exists. Registry and membership changes increment `MembershipVersion` for readers that cache them.
- **IAM.** The Lambdas only read `GroupRegistry` and `GroupRules#…`, which the read statement already allows; they write only `Machine#<id>`/`Groups`, already covered by `Machine#*`. The group partitions are therefore not added to the write statement's `dynamodb:LeadingKeys`, as for global rules. No Terraform change is needed.
- **Versions never repeat.** Every `RulesVersion` (at `group create` and at each rule change) is drawn from the registry's `RulesVersionCounter`, conditional on the counter not having moved, so a group deleted and created again never reuses a version a warm server instance may have cached for the old group.
- **No reported membership** has the basis `none`, not the empty basis of a sync state that never recorded one. A sync state that expired, or was written by a Lambda that predates groups, therefore makes the next preflight clear a stale `Reported` (a conditional update that creates no item for Macs that never had one).

Known limits, not fixed:

- The machine-rule tail, group winners included, has no response-size budget, like machine rules alone (deferred with the SCALE work): a Mac with very many group rules gets them all on the last page.
- A group rule added or removed changes the set of keys that apply, so each member Mac reads its whole delivery partition once at its next sync (an edit that keeps the key changes nothing tracked).
- Server instances cache the registry independently for up to 60 s, so during that window one instance can send a new group rule and another its replacement (the global rule or a `REMOVE`); the Mac settles within a sync or two once all instances see the same version.
- The 5,000-rule limit is checked before the write, not atomically with it: concurrent writers can take a group slightly past it.
