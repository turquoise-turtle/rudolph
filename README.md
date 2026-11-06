# Rudolph
Rudolph is the control server counterpart of [Santa](https://github.com/northpolesec/santa), and is used to rapidly deploy configurations to Santa agents.

Rudolph is built in Amazon Web Services, and utilizes exclusively serverless components to reduce operational burden. It is designed to be fast, easy-to-use, low-maintenance, and cost-conscious.

## Who is Rudolph For?
Rudolph is built for teams interested in deploying [Santa](https://github.com/northpolesec/santa) to implement Binary Authorization
on MacOS environments. In particular, it is designed around supporting:

* Santa in `LOCKDOWN` Mode
* Realtime unblocking
* Machine-specific configurations

Addtionally, Rudolph uses Amazon Web Services and is ideal for teams that are too small to stand up or maintain more
sophisticated environments.

* Easy deployment: Set up the entire stack in 20 minutes. Tear it down in 1 minute
* (Almost) Zero maintaintence
* Proven scalability & cost-efficiency
* Scales up and down automatically
* High performance; Rudolph is _designed_ to support 60-second sync intervals on Santa sensors, for real-time unblocking

More information can be found in our [primer on Lockdown](/docs/lockdown.md).

Rudolph requires Santa 2026.8 or newer and is tested with 2026.8 and 2026.9; Santa 2025.12 is supported for macOS 13 Macs only, with limits (see [configuring Santa](docs/configuring-santa.md)).


# Deployment

## Step 1) Deploy Rudolph
Start by deploying rudolph ([docs/deploy.md](docs/deploy.md)).


## Step 2) Deploying Santa Agents
Next, deploy and configure your Santa sensors ([docs/configuring-santa.md](docs/configuring-santa.md)).


## Step 3) Deploy Rules
Use the cli to sync rules ([docs/rules.md](docs/rules.md)). Every command and flag is in the [CLI reference](docs/cli.md); commands that write accept `--dry-run` to show what they would change.


## Further reading
* [Architecture](docs/architecture.md)
* [The sync process](docs/sync.md)
* [Device groups](docs/groups.md)
* [Execution events table](docs/events-table.md), which the read-only web view, rudolph-web, lists events and top binaries from
* [Costs](docs/costs.md) and [security risks](docs/security-risks.md)

