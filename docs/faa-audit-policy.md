# An audit-only file access policy to start with

[`examples/faa-audit-policy.plist`](examples/faa-audit-policy.plist) is a first File Access Authorization (FAA) policy for a fleet managed with Jamf. Every rule is `AuditOnly`: Santa blocks nothing and shows no notification, but each access a rule would have denied becomes an `AUDIT_ONLY` file access event. Santa uploads those to Rudolph with the next sync, Rudolph stores them (see [File access events](events-table.md#file-access-events)), and rudolph-web lists them at `/events/file-access`. A few weeks of these events show which processes legitimately touch each path, which is what you need before any rule is enforced.

Rudolph cannot send this policy itself: FAA rules over the sync protocol need sync v2 (Workshop). The policy goes to the Macs in a configuration profile, as `FileAccessPolicy` in the Santa profile or as a separate plist named by `FileAccessPolicyPlist` (see [configuring-santa.md](configuring-santa.md)). Use one of the two: Santa ignores `FileAccessPolicyPlist` when `FileAccessPolicy` is set.

## The rules

| Rule | Paths | Logged |
|---|---|---|
| `ProtectJamf` | `/usr/local/jamf/`, `/Library/Application Support/JAMF/` | writes by anything but Jamf's own processes (its Team ID) and Apple's installer |
| `SystemLaunchItems` | `/Library/LaunchDaemons/`, `/Library/LaunchAgents/` | writes by anything but Apple's installer and Jamf: new system-wide persistence |
| `UserLaunchAgents` | `~/Library/LaunchAgents/` of every user | every write: per-user persistence |
| `ChromeCookies` | each Chrome profile's `Cookies` | reads and writes by anything but Chrome and Spotlight (cookie theft) |
| `Sudoers` | `/etc/sudoers`, `/etc/sudoers.d/`, sudo's timestamps | reads and writes by anything but `sudo` |
| `PAMConfig` | `/etc/pam.d/` | every write |
| `SSHKeys` | `~/.ssh/` of every user | reads and writes by anything but Apple's SSH tools |
| `InMemoryCodeLoading` | the temporary files dyld writes for `NSCreateObjectFileImageFromMemory` | every access: code loaded from memory through `NSCreateObjectFileImageFromMemory`, which dyld backs with this temporary file. It does not catch every in-memory technique: an interpreter calling native code directly (Python's `ctypes`, for example) or a custom loader writes no such file |

`ChromeCookies` and `Sudoers` follow North Pole's published examples ([FAA cookbook](https://northpole.dev/cookbook/faa/)), switched to audit only; `InMemoryCodeLoading` follows North Pole's in-memory execution recipe. The others are this repository's.

## Before deploying

- **Check Jamf's Team ID.** The policy names `483DWKW443` for Jamf in two rules. Confirm it on a Mac with `codesign -dv /usr/local/jamf/bin/jamf` (the `TeamIdentifier` line) and correct the plist if it differs. With a wrong Team ID nothing breaks, as the policy only audits, but every Jamf write shows up as an event.
- **Check the other identifiers on your macOS version.** The Apple signing IDs (`com.apple.installer`, `com.apple.installd`, `com.apple.package-script-service`, the SSH tools) were read with `codesign -dv` on macOS 26. A rule whose processes do not match logs more, not less.
- **Validate the file.** `plutil -lint faa-audit-policy.plist`. After it reaches a test Mac, Santa logs `Ignoring file access rule '<name>': …` for a rule it refuses and `Failed to parse watch item config` for a file it cannot read: `log show --predicate 'subsystem == "com.northpolesec.santa"' --last 15m | grep -i 'watch item\|file access'`. `santactl status` shows the number of file access rules loaded.
- **Test Mac first.** Scope the profile to one test Mac, then a handful of pilot Macs, before the fleet.

Santa 2026.9 also normalises paths with `//` or a trailing slash, and from 2026.9 file access events carry the `platform:` or team ID prefix on signing IDs again (2026.4 to 2026.8 left it off), so events from before and after the upgrade name the same process differently.

Paths with `*` are globs that Santa expands when it loads the policy and again every `FileAccessPolicyUpdateIntervalSec` (600 seconds by default), so a user account or Chrome profile created after that is covered from the next evaluation.

## Reading the results

On `/events/file-access`, filter by decision `AUDIT_ONLY` or by Mac. For each rule, group the events by the process path and signing ID:

- processes that belong (an updater, a backup tool, MDM) go into that rule's `Processes` list;
- anything you cannot explain is worth a look: on a student Mac, a write to a launch agents folder or a read of `Cookies` by an unknown process is what this policy is for.

Expect the first days to be noisy: software updates, Spotlight and app updaters touch several of these paths. `UserLaunchAgents` and `PAMConfig` log every write on purpose, as there is no list of legitimate writers yet; give them a `Processes` list from what you see.

## Moving a rule to enforcement

Only once a rule has run in audit for long enough that its events are all explained, and one rule at a time:

1. Set that rule's `AuditOnly` to `false`, and give it an `EventDetailText` (and the policy an `EventDetailURL`, if you have a page to send people to) so the block notification says what to do.
2. Raise the policy's `Version` (for example `pilot-2`), so events show which version decided them.
3. Deploy to the test Mac, then the pilot Macs, then the fleet, watching for `DENIED` events from processes that should have been allowed.

If an enforced rule breaks something, back out with Rudolph rather than waiting for a new profile to reach the Macs: `rudolph config update --group <group> --override-file-access-action audit_only` makes every Mac in that device group audit instead of deny from its next sync (`disable` turns FAA off for them), and `--ungrouped` does the same for the Macs in no group. Between a Mac's groups the most relaxed override wins, so a Mac also in a group that still enforces is backed out too; a Mac's own override still beats its groups'. Once the rule is fixed, remove the switch with `rudolph config update --group <group> --inherit override-file-access-action`. See [file access override per group](groups.md#file-access-override-per-group).

From Santa 2026.9, `ParentDirectoryProtection` also covers renaming or replacing a protected path's parent directories to get round a rule. It defaults to `audit`, so expect some extra events from it while auditing; set it to `enforce` on a rule when you enforce that rule.

`ProtectJamf` and `SystemLaunchItems` are the rules most worth enforcing on student Macs. `ChromeCookies` and `SSHKeys` matter most on staff Macs. Keep `InMemoryCodeLoading` in audit at first: it catches a technique rather than a known path, and what it catches is worth reading before deciding.
