# ComputeStacks Node Agent

`cs-agent` runs on every ComputeStacks node as the node's local **data plane** — a
single native binary, managed by systemd, that:

* **Backs up volumes** — scheduled [borg](https://www.borgbackup.org/) backups, restores,
  and on-demand backup export to S3 (streamed as a presigned download).
* **Programs the node firewall** — renders published-port DNAT/forwarding into a native
  `cs_agent` nftables table (replacing the old iptables shell-out).
* **Serves customer metadata** — an HTTP API on `:8500` that returns per-project metadata
  from an **embedded SQLite** store (replacing Consul KV for the `/db/` space; no
  value-size cap).

> **As of v2.0.0 the agent ships as a native systemd service installed from a signed apt
> repo — it is no longer run as a Docker container.** The container image is retained for
> local development only. See [`CHANGELOG.md`](CHANGELOG.md) for the v2.0.0 upgrade/rollback
> runbook and [`packaging/README.md`](packaging/README.md) for the release + apt-distribution
> details.

## Requirements

* **Debian 12 or 13** (the agent uses the `iptables`/nftables backend present there).
* **Docker** — the agent talks to the Docker socket and manages container firewall rules.
* **Consul** — still required for coordination until that workload is moved off it.

## Installation

The agent is distributed as a `.deb` from the ComputeStacks signed apt repo. Add the
keyring and source **once** per node, then install:

```bash
# Trust the ComputeStacks signing key (one shared key for all CS packages)
curl -fsSL https://repo.computestacks.com/public/computestacks.gpg.asc \
  | gpg --dearmor | sudo tee /etc/apt/keyrings/computestacks.gpg >/dev/null

# Add the repo (one source line for the whole ComputeStacks repo)
echo "deb [signed-by=/etc/apt/keyrings/computestacks.gpg] https://repo.computestacks.com/public stable main" \
  | sudo tee /etc/apt/sources.list.d/computestacks.list

sudo apt-get update && sudo apt-get install -y cs-agent
```

Installing places the binary at `/usr/bin/cs-agent` and a systemd unit at
`/lib/systemd/system/cs-agent.service`, and **enables the service for boot**. On a fresh
node the service won't actually start until it's configured (see below) — the unit is
gated by `ConditionPathExists=/etc/computestacks/agent.yml`.

## Configuration

The agent reads `/etc/computestacks/agent.yml`. A fully-commented sample is installed at
`/usr/share/doc/cs-agent/agent.sample.yml` (also [`agent.sample.yml`](agent.sample.yml) in
this repo):

```bash
sudo install -Dm600 /usr/share/doc/cs-agent/agent.sample.yml /etc/computestacks/agent.yml
sudoedit /etc/computestacks/agent.yml
sudo systemctl restart cs-agent
```

Notable settings:

* `store.data_dir` — where the embedded SQLite data plane lives (`control.db` + a metadata
  DB per project).
* `metadata.listen_addr` — **must stay `:8500`**; it's baked into customer containers as
  `metadata.internal:8500`. Because the agent binds `:8500`, Consul's HTTP listener moves
  to `:8502` on upgraded nodes (see the CHANGELOG upgrade steps).
* `metadata.admin_token_hash` — sha256 of the per-node admin bearer the controller uses.
* `backups.*` — borg schedule, encryption key, and the SSH/NFS backup repo.
* `backups.export.s3` — S3 target for backup export (inert until `bucket` is set).

## Service management

```bash
systemctl status cs-agent
sudo systemctl restart cs-agent
journalctl -u cs-agent -f       # follow logs (SyslogIdentifier=cs-agent)
cs-agent -version               # print version / commit / build date
```

The agent runs as **root** — it needs the Docker socket and `NET_ADMIN` for firewall
management.

## Maintenance mode

A node can be put into maintenance from the controller (admin API) or locally on the node.
Each side places its own **hold**; the node is paused while any hold exists, and each side
clears only its own. While paused the agent starts no new tasks, skips backup slots and
prune/compact runs, and cancels restores that had not started yet. Work already running
finishes. The firewall, the metadata API and desired-state updates keep working.

```bash
sudo cs-agent maintenance on --reason "docker upgrade" --wait --timeout 2h   # pause and wait until idle
sudo cs-agent maintenance status --json                                     # read-only status
sudo cs-agent maintenance off                                               # clear the local hold
```

`--wait` returns 0 once the local hold is set, nothing is in flight, and the controller has
acknowledged the hold. Exit codes: `1` usage or the hold was cleared while waiting, `2`
timed out with work still running, `3` the agent database could not be opened, `4` the
controller has not acknowledged the hold (`--no-controller` skips that check; avoid it
while docker is being restarted). Holds persist across restarts and reboots, so clear the
local hold explicitly when the work is done.

## Upgrades & rollback

```bash
sudo apt-get update && sudo apt-get upgrade                 # move to the latest published version
apt list -a cs-agent                                        # list all available versions
sudo apt-get install --allow-downgrades cs-agent=<version>  # roll back to a specific version
```

Every published version stays in the apt pool, so rollback is a normal downgrade —
**except** across a non-additive SQLite migration. See the [rollback
notes](packaging/README.md#rollback) and the [`CHANGELOG.md`](CHANGELOG.md) upgrade runbook.

## Development

The Docker image is kept for local development only:

```bash
make build-container      # build ghcr.io/computestacks/node-agent:latest
```

Build and run the native binary directly (it reads `./agent.yml` or
`/etc/computestacks/agent.yml`):

```bash
go build -o cs-agent .
```

Install Go dependencies with `go mod download`.

> **NOTE:** Do not run `go get -u` to update Go modules. Docker has not used semantic
> versioning in quite some time, so the update-all command will replace the current Docker
> API with an older version and break the build. Update a single module with
> `go get <mod>@<version>` instead.

Releases are automated: pushing a `vX.Y.Z` tag runs
[`.github/workflows/release.yml`](.github/workflows/release.yml), which builds the binaries,
packages the `.deb` with GoReleaser, and publishes them to the signed apt repo. See
[`packaging/README.md`](packaging/README.md) for the full release + distribution design.
