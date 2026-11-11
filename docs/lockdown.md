# A Primer on Santa Lockdown
Santa `LOCKDOWN` is the holy grail of protection, but is extremely dangerous to implement if you're not properly prepared.
Below we've listed some lessons we've learned along the _Journey to Lockdown_.

## Realtime Unblocking
One of the most important elements of a graceful migration toward lockdown is being able to facilitate **realtime unblocking**. We define as the ability to unblock an `UNKNOWN` application while on `LOCKDOWN` mode as quickly and
with as little friction as possible.

Rudolph supports this through machine-specific configurations and fast syncing.

### Fast Syncing
Santa's official documentation suggests a default sync period of 10 minutes (600 seconds). This means, new rules can take up to 10 minutes before being pushed down to Santa agents. This can result in very poor user experiences.

Rudolph supports any sync interval, down to Santa's minimum of 60 seconds.

### Machine-specific Configurations
On Rudolph both rules and machine configurations can be specified both _globally_ and at a _machine-specific_ level.

For Santa agent configurations, there is a default configuration that is hardcoded into the app, but we recommend setting a
global one in DynamoDB. Each machine then also can have a machine-specific configuration that overrides the settings
stored on it; every other setting follows the global one (see [sync](./sync.md#reconfiguring-the-sensor)).

Setting the _global_ client mode to `LOCKDOWN` (or any other mode except `MONITOR`, such as `STANDALONE`, which also blocks unknown executions) changes every machine whose machine-specific configuration does not set a client mode at its next sync, so Rudolph requires an explicit confirmation for it:

```
rudolph config update --global --client-mode lockdown --confirm-global-lockdown
```

Without `--confirm-global-lockdown` (or `ConfirmGlobalLockdown()` when using the `machineconfiguration` package), the change is rejected. Switching the global mode back to `MONITOR`, and putting individual machines into lockdown, need no confirmation:

```
rudolph config update --machine <machine-uuid> --client-mode lockdown
```

A machine can also be put into lockdown, standalone or monitor mode for a limited time, e.g. to unblock a developer for an afternoon without changing the machine's own mode: `rudolph config update --machine <machine-uuid> -c monitor --for 2h`. The Mac switches at its next sync and returns to its usual mode at its first sync after the end; see [temporary client modes](./sync.md#temporary-client-modes).

A device group can have a client mode too, e.g. to lock down a pilot group before the whole fleet: `rudolph config update --group pilot -c lockdown --confirm-group-lockdown`. A Mac's own or temporary mode beats its groups', and between its groups the strictest mode wins; see [client mode per group](./groups.md#client-mode-per-group).

Warm Lambda instances cache the global configuration for at most one minute, so global changes reach hosts on their first sync after that.

For rules, there is a set of global rules which is deployed to all machines. Each machine can also have their own machine-specific rules, which are appended onto the global rules (and override them, when applicable). This allows you to deploy rules to specific machines without influencing other machines.

## Eventupload
To improve adoption of Santa, it is extremely important to be able to introspect on what your fleet is running. To collect information on this, the `/eventupload` endpoint in Rudolph can be configured to plug into other AWS services, such as Lambda, Firehose, or Kinesis Data Streams.


## Lockdown Gotchas
Here are some gotchas to think about prior to changing sensors to lockdown.

### Libcurl and SSL and other Homebrew Darkness
Some system-critical libraries in Homebrew have post-installation scripts that initialize certain parts of the application. One example os `libcurl` which runs a binary to set up SSL certificates.

If Santa blocks this post-install binary, it can result in `libcurl` being partially broken inside of Homebrew, and a lot of headaches and debugging will follow.


### VPNs
If your Rudolph server is accessible only via a VPN or has some other IP-based inbound allowlist, it's vital that Santa has rules that prevent this pipeline from breaking down.

As an example, if an erroneous rule is deployed to (or is _missing from_) an endpoint that blocks your VPN provider, it is
possible that the Santa agent can get "orphaned" if it is unable to connect to Rudolph (if it needs VPN in order to do so).


### Browsers
If you have a web GUI to facilitate the Santa binary unblocking process, it's important that the web browser is not locally
blocked, or it can result in a user being "stuck" trying to unblock something.

This can be fixed through the backend but usually results in bad user experience.

