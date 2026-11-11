# Device groups

A device group is a named set of Macs that rules can target, besides every Mac (`--global`) and one Mac (`--machine`). Use groups for rules that apply to a team, a department or a handful of Macs: for example, Google's team ID is allowed for everyone, but the engineering group must not run Gemini. The design and its trade-offs are in [groups-design.md](groups-design.md).

## Making groups

Register a group in Rudolph first. Only registered groups count, so a Mac that reports hundreds of directory groups gains nothing from the ones Rudolph does not know.

```
rudolph group create eng --description "Engineering"
rudolph group create admins --description "Local admin tools" --no-reported
rudolph group list
rudolph group get eng
rudolph group update eng --description "Engineering and QA"
rudolph group delete eng            # refused while the group has rules
rudolph group delete eng --force    # deletes its rules as well
```

Names are trimmed and lower-cased (and Unicode-normalised), so `Eng` and ` eng ` are the same group. A name cannot be empty, be `.` or `..` (rudolph-web shows a group at `/groups/<name>`, and browsers resolve those path segments away; names such as `.eng` or `a..b` are fine), contain control characters, be longer than 128 bytes or start with `rudolph:`, which is reserved. At most 64 groups can be registered, with at most 5,000 rules each.

A Mac is in a group in either of two ways, and its groups are the union of both.

### Groups the Mac reports

Santa 2026.8 reports the primary user's groups in every preflight. Santa takes them from its configuration profile, or from a plist on the Mac:

- **`MachineOwnerGroups` in the Santa profile** (an array of strings). Scope a profile with `MachineOwnerGroups` set to `eng` to the engineering Macs in your MDM, and they join `eng` at their next sync. This is the simplest way to make a small custom group: any MDM smart group or scope becomes a Rudolph group.
- **`MachineOwnerPlist` + `MachineOwnerGroupsKey`**: when `MachineOwnerGroups` is not set (or empty), Santa reads the array named by `MachineOwnerGroupsKey` from the plist named by `MachineOwnerPlist`. Use this when the MDM, a directory tool or a script already writes the user's groups to a plist.

Rudolph normalises the reported names, ignores unregistered ones and ones it does not accept (logging only how many), and counts the rest for groups that accept reported membership (the default; see `--no-reported` below). A change takes effect at the Mac's next sync, up to a minute later when a group was created or changed moments before (the server caches the group list for up to 60 seconds).

### Groups assigned in Rudolph

```
rudolph group add --machine <machine-uuid> eng
rudolph group remove --machine <machine-uuid> eng
rudolph group show --machine <machine-uuid>
```

`group show` lists the Mac's assigned groups, its reported groups that count (as of its last preflight) and its effective groups, or says it is ungrouped. Removing an assigned group does not affect a group the Mac reports. Assigned membership is kept when a group is deleted; it counts again if a group of that name is created later.

### Listing a group's Macs

```
rudolph group members eng
rudolph group members --rebuild            # once after upgrading; asks first
rudolph group members --rebuild --dry-run
```

`group members` lists the Macs in a registered group by machine ID, each marked `assigned`, `reported` or `assigned, reported`. Reported membership is as of each Mac's last preflight that changed it. While a group does not accept reported membership (`--no-reported`), Macs that only report it are left out, as they are not members. The Macs in no group (`rudolph:ungrouped`) and unregistered groups cannot be listed.

The list comes from a member row per Mac and group (`GroupMembers#<group>`/`<machine id>`), written with the Mac's groups by `group add|remove` and by preflight, in the same transaction. Macs whose groups have not changed since upgrading to a version with these rows have none yet: run `rudolph group members --rebuild` once after the upgrade. It scans the table once, writes the rows that are missing or wrong, deletes the rows no Mac's groups back, and writes nothing when every row is right; it shows the counts and asks first (`--yes` skips the question, `--dry-run` writes nothing), and is audited as `group.members.rebuild`. It is also the repair if the rows ever disagree with the Macs' groups, for example after a Mac changed groups while a rebuild was running.

`machine forget` deletes the Mac's member rows with the rest of its items. `group delete` keeps them, as it keeps assigned membership: the Macs are listed again if the group is created again.

### Trust: reported membership is advisory

A configuration profile cannot be changed on the Mac, but a plist can be edited by any local administrator. Through `MachineOwnerPlist`, a local administrator could add a group (and gain its allow rules) or remove one (and escape its blocks, when `MachineOwnerGroups` is not set in the profile). Assigned groups cannot be changed from the Mac.

So for a group that grants privilege, or whose blocks must stick, create it with `--no-reported` (or `rudolph group update <group> --no-reported`): only `rudolph group add` then makes a Mac a member, and Macs reporting the group are ignored. `--accept-reported` undoes it.

### Example: groups from Jamf Pro attributes

Jamf Pro substitutes payload variables (see "Payload Variables for Configuration Profiles" in Jamf Pro's documentation) such as `$DEPARTMENTNAME`, `$ROOM` and `$BUILDINGNAME` into configuration profiles, so a Mac's inventory attributes can become its reported groups. Two ways to do it:

**In the Santa profile (recommended).** Add `MachineOwnerGroups` to the `com.northpolesec.santa` payload:

```xml
<key>MachineOwnerGroups</key>
<array>
	<string>$DEPARTMENTNAME</string>
	<string>$ROOM</string>
</array>
```

A local administrator cannot change a configuration profile, so this membership is as trustworthy as your Jamf inventory.

**Through a variables payload.** If you already publish Jamf's variables to the Mac in a custom payload (Application & Custom Settings, with a preference domain of your own such as `com.example.jamfprovariables`), add the groups to it as an **array** and point Santa at the managed preferences file it becomes:

```xml
<!-- Payload for the domain com.example.jamfprovariables -->
<key>Username</key>
<string>$USERNAME</string>
<key>Groups</key>
<array>
	<string>$DEPARTMENTNAME</string>
	<string>$ROOM</string>
</array>
```

```xml
<!-- In the com.northpolesec.santa payload; MachineOwnerGroups must not be set, or Santa uses it instead -->
<key>MachineOwnerPlist</key>
<string>/Library/Managed Preferences/com.example.jamfprovariables.plist</string>
<key>MachineOwnerGroupsKey</key>
<string>Groups</string>
```

`MachineOwnerKey` can point at a key of the same file (`Username` above) if Santa should take the primary user from it too; `MachineOwner`, if set in the Santa profile, wins over it. Keep every element of the `Groups` array a string: Santa ignores the whole array if any element is not. On a test Mac, `sudo defaults read "/Library/Managed Preferences/com.example.jamfprovariables"` shows the values Jamf substituted. The file is root-owned and Jamf rewrites it, but a local administrator can still edit it between check-ins, so the trust caveat above applies.

Either way:

- **Empty attributes join nothing.** Rudolph drops empty names (and names it does not accept), so a Mac with no room is only in its department's group, and a Mac with neither is in no reported group (and gets `--ungrouped` rules unless it has an assigned group). A variable Jamf leaves unsubstituted, such as a literal `$ROOM`, is ignored too, as long as no group of that name is registered.
- **Check two things on a test Mac first**, which Jamf's documentation does not say: what Jamf substitutes for an empty inventory field, and whether it re-sends the profile when a Mac's department or room changes (if not, the Mac keeps its old groups until the profile is redistributed). Variable names are case-sensitive.
- **Only registered names count.** Register each department or room that should carry rules, using its value as the name: `rudolph group create science`. Names are lower-cased, so `Science` and `science` are one group, and a department and a room with the same value are the same group. Unregistered values cost nothing.
- **At most 64 groups can be registered**, so register the departments and rooms that need their own rules rather than all of them.
- `rudolph group show --machine <machine-uuid>` lists the reported groups that count after the Mac's next sync, and rudolph-web's machine page shows the groups as Santa sent them.

## Group rules

Every `rudolph rule` command that creates rules accepts `--group <group>`, and `--ungrouped` for the Macs in no group, as a third kind of target (exactly one of `--global`, `--machine`, `--group` and `--ungrouped`):

```
rudolph rule deny -t signingid -i EQHXZ8M8AV:com.google.GeminiMacOS --group eng --custom-msg "Gemini is not allowed for Eng"
rudolph rule allow -t binary -i <sha256> --ungrouped
rudolph rule remove -t signingid -i EQHXZ8M8AV:com.google.GeminiMacOS --group eng
rudolph rule remove SigningID#EQHXZ8M8AV:com.google.GeminiMacOS --group eng
```

- The group must be registered. `--ungrouped` needs no `group create`.
- Group rules never expire: `--expires` and `--permanent` are refused.
- To change a group rule, write it again (it replaces the old one); `rudolph rule update` does not take group targets.
- Member Macs receive group rules with their machine rules on every sync, so an added, changed or removed rule reaches them at their next sync, without a clean sync. When a Mac leaves a group, or a group rule is removed, the Mac gets the global rule for the same identifier and type back (with its own fields), or drops the rule when there is none; a rule that another of its groups, or a machine rule, still provides stays.

### Ungrouped

A Mac whose effective groups are empty is in the reserved group `rudolph:ungrouped`, and gets the `--ungrouped` rules. A Mac that reports only directory groups Rudolph does not know is ungrouped. When it joins a group, the ungrouped rules leave it at its next sync (replaced by the global rule, or removed).

### Precedence

For the same rule type and identifier on one Mac:

1. A machine rule wins.
2. Otherwise the strongest rule of the Mac's groups: `BLOCKLIST`, then `SILENT_TTY_BLOCKLIST`, `SILENT_GUI_BLOCKLIST`, `SILENT_BLOCKLIST`, `CEL` (a conditional block beats an unconditional allow), `ALLOWLIST`, `ALLOWLIST_COMPILER`. For equal policies, the rule of the group whose name sorts first.
3. Otherwise the global rule.

So a group can loosen a global rule (a group allow beats a global block), but between two groups of one Mac a block always beats an allow.

Rules of different types are all sent, and Santa's own order decides between them: CDHash, binary, signing ID, certificate, then team ID. That is what makes the Gemini example work: Google's team ID is allowed globally, the `eng` group blocks Gemini's signing ID, and a signing ID rule beats a team ID rule, so Gemini is blocked on `eng` Macs while every other Google app stays allowed:

```
rudolph rule allow -t teamid -i EQHXZ8M8AV --global
rudolph group create eng --description "Engineering"
rudolph rule deny -t signingid -i EQHXZ8M8AV:com.google.GeminiMacOS --group eng --custom-msg "Gemini is not allowed for Eng"
```

Then put `eng` in `MachineOwnerGroups` in the Santa profile scoped to the engineering Macs, or `rudolph group add --machine <uuid> eng` for each.

## Client mode per group

A registered group can hold a client mode, which its Macs get instead of the global one:

```
rudolph config update --group pilot -c lockdown --confirm-group-lockdown
rudolph config update --group lab -c monitor
rudolph config update --ungrouped -c monitor
rudolph config update --group pilot --inherit client-mode     # removes the group's mode
rudolph config get --group pilot
rudolph config get --ungrouped
```

- Only the client mode can be set for a group; other settings, `--clean-sync` and temporary modes (`--for`, `--until`, `--end-temporary-mode`) are refused with `--group` and `--ungrouped`, and `config set` takes no group.
- `lockdown` and `standalone` need `--confirm-group-lockdown`, as a group may hold many Macs.
- The group must be registered. `--ungrouped` registers the reserved group `rudolph:ungrouped` when it is not, as its first rule does.
- `group list` and `group get` show each group's client mode. `config get --machine <uuid>` marks a client mode that comes from a group with `(group <name>)`, the group whose mode wins, using the Mac's assigned groups and the reported groups recorded at its last preflight.
- The change reaches each Mac at its next sync (up to a minute later, as the server caches the group list), and is audited as `config.update` on `group:<name>` (or `ungrouped`).

### Precedence of client modes

The client mode preflight sends a Mac is the first of:

1. its temporary client mode, while it lasts (see [temporary client modes](sync.md#temporary-client-modes));
2. the client mode stored on its own configuration (`config update --machine <uuid> -c <mode>`); a machine configuration without a client mode, such as one holding only a batch size, does not stop a group's mode;
3. the strictest client mode among its effective groups that have one: `LOCKDOWN`, then `STANDALONE`, then `MONITOR`. A Mac in no group is in `rudolph:ungrouped`, which can hold a mode like any group;
4. the global configuration's (or the default `MONITOR`).

So a single Mac is exempted from its group's lockdown by giving it a mode of its own, or a temporary one, and a group's mode can loosen the global one (a `MONITOR` group on a `LOCKDOWN` fleet) as a group rule can loosen a global rule. A Mac's groups are worked out as for rules: its assigned groups, plus the groups it reports in this very preflight that accept reported membership, so joining or leaving a group by reporting it changes the mode in the same sync. A group mode that cannot be decoded (not a number, or not 1 to 3) is ignored, as if the group had none.

The mode is stored as `ClientMode` (a number, as on configuration rows: 1 `MONITOR`, 2 `LOCKDOWN`, 3 `STANDALONE`) in the group's entry of the registry's `Groups` map (`PK` `GroupRegistry`, `SK` `Current`). Tools writing it, such as rudolph-web, must bump `MembershipVersion` in the same update; the Go package `pkg/model/groups` (`SetClientMode`, `MachineClientMode`, `StrictestClientMode`) does both.

**Security.** A local administrator can edit a plist, so a Mac whose groups come from `MachineOwnerPlist` could drop out of a `LOCKDOWN` group (and get a looser mode) by removing the group from it. For lockdown groups, report membership through `MachineOwnerGroups` in a configuration profile, which cannot be changed on the Mac, or assign the Macs with `rudolph group add` and create the group with `--no-reported`.

## Costs

Without registered groups nothing changes. With groups, each sync reads the Mac's groups item (one small read); the group list and group rules are cached by each server instance. A Mac's first sync after joining a group records each of the group's rules once (about one write unit per rule), and leaving a group about two per rule (the removal is recorded, then forgotten once delivered); a steady-state sync costs nothing more. Preflight writes the Mac's groups item only when its reported membership changes, then also reads it first and writes the member row of each group joined or left (see [costs.md](costs.md#group-members)). Group client modes cost nothing while no group has one; once any group has one, preflight reads the Mac's groups item (one small read, half a read unit) unless the Mac has a temporary or its own client mode.

## Not built yet

`rudolph rules import|export --group` from the design is not available yet.
