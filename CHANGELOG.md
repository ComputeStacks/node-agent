# Changelog

## v3.1.0

Correctness release on top of v3.0.0 — **no migrations (`control.db` stays at schema `v4`), no
config changes, no API changes.** Five restore paths that could destroy a customer volume are
fixed, a failed `borg` is no longer reported as a success, and a published port is now reachable
across projects on the same node.

Two of the restore fixes change what an operator sees on a normal, successful restore — a longer
outage, and dotfiles now tracking the archive exactly. Both are called out inline below.

- [FIX] **Restore rollback no longer destroys the snapshot it exists to restore.**
  `rollbackRestore` ran the volume's `PostRestore` command *before* putting `/root/.snapshot` back,
  and gave up if that command errored — and on the rollback path it could only error, because the
  hook resolves its target to *running* containers and every caller is downstream of the restore's
  stop loop. So for any volume with a `PostRestore` command configured, rollback returned early,
  the deferred teardown removed the AutoRemove backup container, and the snapshot — which lives in
  that container's own filesystem, not in the volume — went with it. The put-back now runs first,
  unconditionally, for every strategy.
- [FIX] **One owner for the restore snapshot and its rollback.** The strategy hooks and the borg
  layer each took the snapshot and each rolled it back; once a non-zero `borg` exit counts as a
  failure, both rollbacks running would move the snapshot back into `/mnt/data` and then delete it.
  `preRestore` now takes the snapshot once, for every strategy, and halts the restore if it cannot;
  `rollbackRestore` puts it back once, before the strategy hooks. This also gives the `default` and
  `postgres` strategies a real rollback, which they never had — the internal one only ran on the
  docker-fault path, so a `borg` failure rolled back nothing and left the volume empty.
- [FIX] **The service stops before the volume is snapshotted, for every strategy.** The snapshot is
  a cross-device copy-and-delete rather than a rename, so taking it under anything still writing
  left a torn copy in the snapshot and an emptied volume — and `preRestore` stopped containers only
  for `mysql`, `mariadb` and `postgres`, with no default case. An ordinary application volume, which
  is the common case, was copied while its containers ran. (`postgres` also stopped nothing, and
  `mariadb` was absent from the switch altogether despite being an accepted `borg_strategy`.) The
  stop is now unconditional and owned by one place. It still runs *after* the volume's `PreRestore`
  command, which needs a running container to exec into. **Operator-visible:** an ordinary
  application restore now takes its downtime before the snapshot copy rather than during it, so the
  outage is longer by roughly the time it takes to copy the volume. That is not a loss of real
  availability — the application was previously "up" against a directory being emptied underneath
  it — but the wall-clock window does grow.
- [FIX] **Dotfiles are snapshotted, cleared and put back.** The snapshot moved `src/*` and the
  rollback cleared `dst/*`; neither glob matches a leading dot. On a failed restore the snapshot
  never held the volume's dotfiles, `borg extract` overwrote whichever ones the archive carried, the
  rollback did not clear them, and the put-back had nothing hidden to return — so the pre-restore
  content of every dotfile a partial extract touched was gone, with the rollback reporting success.
  On a WordPress volume that is `.htaccess`, `.user.ini`, `.env`, `.git/` and `.ssh/`. Archives were
  never affected: `borg create` recurses from `.` and always captured them. **Operator-visible:** a
  successful restore is now faithful to the archive, so a dotfile present in the volume but absent
  from the archive is removed rather than surviving. Note `borg create` runs with `--exclude-caches`,
  so the contents of a `CACHEDIR.TAG`-marked directory were never archived — hidden cache trees
  therefore go from quietly surviving a restore to being deleted by one.
- [FIX] **A mysql restore refuses to promote something that is not a prepared dump.** The mysql
  strategy rearranges the extracted archive by promoting its `backups/` directory into the datadir.
  That step now verifies up front that `backups/` is a real directory (not a symlink out of the
  volume), is non-empty, and carries the `xtrabackup_checkpoints` that `xtrabackup`/`mariabackup`
  write — and it fails before moving anything, with a diagnosis, rather than discovering the problem
  after it has already emptied the volume. Previously a volume whose `backups/` was missing, empty,
  or not a dump at all could leave the datadir wrong, and the failure arrived too late to be cheap.
- [FIX] **Restore-rollback outcomes are reported the right way round** — a successful rollback no
  longer reports as a failure, or the reverse.
- [FIX] **A failed `borg` fails the task.** `containermgr.Container.Exec` returns a nil error for a
  non-zero exit, so the borg layer only ever reported docker-level faults: a failed `init`,
  `create`, `prune`, `compact`, `info`, `contents` or `delete` could complete as a success —
  including `create`, which is how a backup could be recorded green with nothing in the archive.
  Every invocation now goes through one exec funnel that owns the verdict and reports `borg`'s own
  diagnosis, extracted from its `--log-json` record.
- [FIX] **Quote `borg`'s diagnosis, not its usage banner.** When `borg` rejects an argument it emits
  no JSON record at all (argparse writes plain text before JSON logging is in effect, banner first
  and reason last), so the fallback quoted the least informative line available. It now prefers the
  line carrying `error:`, falling back to first-line for output where nothing does.
- [FEATURE] **Published ports are reachable across projects on the same node.** The blanket
  cross-project isolation rule in `DOCKER-USER` dropped bridge-to-bridge traffic to a published
  (direct-NAT) port, so whether an exposed port answered depended on the luck of container
  co-placement. A published port is a node-level endpoint, not external-only, so a connection that
  arrived via a DNAT'd endpoint (`-m conntrack --ctstate DNAT`, connection-scoped, so replies are
  covered) is now allowed. Direct private-bridge-IP access across projects still drops.
- [FIX] **Gate the `cs_agent` DNAT chain on `fib daddr type local`.** The DNAT rule matched only
  l4proto plus dport-in-published-set, with no destination-address gate, so a container dialing a
  sibling project's private bridge IP on a colliding port was silently redirected to the publisher.
  With the gate (the nft analog of Docker's `-m addrtype --dst-type LOCAL`), "was DNAT'd" means
  "dialed a node-local published endpoint" by construction. External ingress and the host-origin
  `OUTPUT` mirror DNAT as before.
- [FIX] **`postBackup` runs after a failed `create`, per `backup_error_cont`.** The failed-create
  branch consulted `restore_error_cont` — the restore hooks' flag — so a mysql-strategy volume
  whose backup failed skipped `postBackup` unless the restore flag happened to be set, leaving the
  xtrabackup/mariabackup dump in `/mnt/data/backups` on the customer's volume until the next
  successful backup. (Unreachable before a failing `borg` could fail the task.)
- [FIX] **Failures no longer reach the controller with a bare `()` prefix.** The msgid prefix is
  only populated for messages that came from `borg`'s JSON output, so every message the agent
  synthesizes itself was prefixed with `"() "` in production task errors. The two `DeleteBackup`
  failure paths also put a rendered, mostly-empty struct in `result_json` instead of the reason.
- [CHANGE] **A completed task logs a terminal line** with its task id, kind and elapsed duration.
  Success previously logged nothing at the task layer, so a task UUID could not be traced
  start→finish in the node log and confirming one meant querying `control.db` or the controller's
  projection.
- [CHANGE] **The `borg delete --stats` table is recorded, not logged at INFO.** It was the last
  success-path `PostEventUpdate` in the package, so the final thing a successful delete wrote to the
  node log was a 9-line stats table that reads like a truncated failure. Verbose payloads stay
  reachable on the node: `Record` now also logs at DEBUG.

Upgrading is a plain `apt-get install cs-agent` per node — no `agent.yml` changes, no controller
coordination, and no maintenance window. Because there is no migration, downgrading to v3.0.0 is a
normal `apt-get install --allow-downgrades cs-agent=3.0.0`.

## v3.0.0

Major release — **Consul is fully removed from the agent.** The embedded SQLite `control.db`
becomes the sole coordination plane for tasks, volumes, firewall rules, borg repositories, and
backup schedules. This completes the Consul-retirement / node-autonomy re-architecture. One
big-bang cutover in a maintenance window (controller-off-first); coordinated with a controller
release (see the controller repo's changelog for its side).

- [CHANGE] **Consul removed.** The binary no longer links `hashicorp/consul/api` and never
  contacts Consul. `control.db` is the source of truth for all node coordination state.
- [CHANGE] **In-process task dispatch.** A single dispatcher goroutine drains pending tasks
  from `control.db` with an at-most-once claim (status CAS), replacing the Consul long-poll job
  watcher. Task kinds: `volume.backup`, `volume.restore`, `backup.delete`, `backup.export`,
  `volume.trash`. A boot reconcile fails orphaned `running` tasks; restore/delete/export/trash
  never auto-replay.
- [CHANGE] **Durable backup scheduler.** A `schedules` table + tick loop fires backups
  exactly once (with a single catch-up and no backfill storm), replacing the in-memory cron
  runner and the Consul schedule mirror. `robfig/cron` is kept only as the cron-string parser.
- [FEATURE] **Controller DOWN endpoints** (per-node admin Bearer): `POST /v1/admin/tasks`
  (controller-supplied idempotent id), `PUT`/`DELETE /v1/admin/projects/{pid}/volumes/{name}`,
  `PUT`/`DELETE /v1/admin/nodes/{host}/firewall_rules` (firewall reconciles on the PUT — there
  is no firewall task), and `POST /v1/admin/changelog/ack`. UP state (task status + results,
  observed repositories) rides the existing `GET /v1/admin/changelog` pull.
- [CHANGE] **csevent retired.** The agent no longer POSTs to `/api/system/events`;
  backup/restore/export/delete outcomes ride the task's status + `result_json` (terminal
  outcome plus captured failure output — no live per-step stream).
- [CHANGE] **Populate-before-enforce (no cutover outage).** On a fresh/empty node the firewall
  and volume domains skip reconcile — leaving the live `cs_agent` nftables table and running
  workloads untouched — until the controller backfills desired state; cross-project isolation
  still applies. Published ports stay up across the upgrade gap.
- [FEATURE] **Bounded control.db.** Changelog prune (ack-gated, plus an ack-independent age
  fallback) and terminal task-row retention keep the DB bounded even before the controller
  starts acking.
- [REMOVED] `consul.*` config, the Consul auth proxy (`proxy_to_consul`), the `schedule_source`
  and `cutover.*` options, and the `consul.service` dependency in the systemd unit.

Migrations run additively (`control.db` schema `v1→v4`); an older binary refuses a `v4` DB via
the schema-version guard, so downgrades require restoring a pre-upgrade snapshot.

### Upgrading a node to v3.0.0

Maintenance window, **controller-off-first**. The rollback anchors are a per-node `control.db`
snapshot **and** Consul left running until the very end. No `agent.yml` changes are required —
`metadata.admin_token_hash` (already set) authenticates the controller's DOWN writes, and any
leftover `consul:` keys are ignored.

1. **Pre-window (per node):** snapshot the DB and drain in-flight jobs. Leave Consul running.
   ```
   cp -a /var/lib/cs-agent/control.db /var/lib/cs-agent/control.db.pre-v3
   ```
2. **Stop the old controller** (once). No more Consul writes; customer containers keep running.
3. **Install v3.0.0 on all nodes** and start. Migrations run `v1→v4`; the agent boots with
   empty coordination tables and, sentinels unlatched, leaves the live firewall table +
   workloads untouched.
   ```
   sudo apt-get update && sudo apt-get install -y cs-agent
   ```
4. **Boot the new controller** (once). It backfills each node (firewall rules + every volume)
   via the DOWN endpoints. The sentinels latch and the agent reconciles: the firewall
   re-renders the same rules, and schedules rebuild with a future `next_fire_at` (no backup
   storm).
5. **Verify per node:** `cs-agent -version` (v3.0.0), `control.db` at schema `v4`, firewall
   renders (`nft list table inet cs_agent`), tasks dispatch, and task status/results flow up
   the changelog with ack + prune working.
6. **Tear down Consul** (per node, once confirmed) and remove it from the provisioner. The v3
   agent has no Consul client and its unit no longer orders after `consul.service`, so this is
   safe; the old `:8502` HTTP relocation is obsolete.
   ```
   sudo systemctl disable --now consul
   ```

**Rollback (until step 6, all nodes):** restore the `control.db.pre-v3` snapshot **and**
downgrade the binary — **order matters**: an old binary against a migrated (`v4`) DB refuses to
boot, so restore the snapshot first (or do both atomically). Consul is still live as the other
anchor. Restart the old controller last.

## v2.1.0

Adds a generic **container-action channel** and the append-only **changelog** primitive the controller
consumes — the first step of inverting task/coordination state onto the agent (continuing the
Consul-retirement / node-autonomy re-architecture).

- [FEATURE] **Container-initiated actions.** A container can `POST /v1/actions` (tenant Bearer) to request
  a named action on its environment (`{action_type, params}`). The agent stamps the project from the token
  — never the request body — and records the request to a durable outbox. It is deliberately **generic**:
  it does not interpret `action_type` (the controller dispatches it), so new actions need no agent change.
  Fire-and-forget (`202 Accepted`), guarded by a per-tenant rate limit and a request-size cap. First use
  case: CDN cache purge.
- [FEATURE] **Node changelog + controller pull API.** A new append-only `changelog` (global monotonic
  `seq`) in the embedded SQLite control DB records node-owned state changes as full snapshots; the
  controller pulls incrementally via `GET /v1/admin/changelog?since=&limit=&entity_type=` (per-node admin
  Bearer). This is the replication spine for moving the remaining Consul-backed coordination state onto the
  agent. Migrations stay rollback-tolerant (additive + schema-version guard).

## v2.0.0

Major release — the agent becomes the node's **data plane** (part of the Consul-retirement /
node-autonomy re-architecture). Three independent changes ship together; production rolls out staged
(native deploy first, then the firewall and metadata cutovers, validated on a canary).

- [CHANGE] **Native deployment.** The agent now runs as a **native systemd binary installed from a
  self-hosted, GPG-signed apt repo**, replacing the `docker run` container unit. The container image
  is kept for local dev only. `cs-agent -version` reports the build version/commit/date.
- [CHANGE] **nftables firewall.** Published-port DNAT/forwarding is rendered into a native `cs_agent`
  nftables table via netlink, replacing the iptables shell-out + string-diff. Cross-project isolation
  stays in `DOCKER-USER`. **Fail-closed:** published ports are closed until the first reconcile. Reads
  the same `ingress_rules` desired state. (Relies on the project bridges' `nat-unprotected` mode, under
  which Docker already accepts the forwarded ingress.)
- [FEATURE] **Customer metadata served by the agent.** A new HTTP API on `node.primary_ip:8500` serves
  per-project customer metadata from **embedded SQLite** — no more Consul KV for the `/db/` space, and
  **no value size cap** (kills the 512 KB ceiling). Bearer→tenant auth + a per-node admin Bearer; a
  compatibility shim serves the legacy `…/metadata?raw=true` read. Migrations are rollback-tolerant
  (additive + schema-version guard).

### Upgrading a node to v2.0.0

Take a maintenance window — the firewall cutover (+ optional reboot) briefly closes published ports.
All nodes must be **Debian 12/13** (`iptables` = the nft backend). Snapshot the firewall first:
`iptables-save > /root/iptables.pre-upgrade`.

1. **Native binary** — add the apt source + keyring, stop the old container unit, install:
   ```
   curl -fsSL https://repo.computestacks.com/public/computestacks.gpg.asc \
     | gpg --dearmor | sudo tee /etc/apt/keyrings/computestacks.gpg >/dev/null
   echo "deb [signed-by=/etc/apt/keyrings/computestacks.gpg] https://repo.computestacks.com/public stable main" \
     | sudo tee /etc/apt/sources.list.d/computestacks.list
   sudo systemctl disable --now cs-agent; docker rm -f cs-agent 2>/dev/null || true
   sudo rm -f /etc/systemd/system/cs-agent.service   # the package unit lives in /lib/systemd/system
   sudo apt-get update && sudo apt-get install -y cs-agent
   ```
2. **Metadata / Consul port** — the agent binds `:8500`, so Consul's HTTP listener moves to `:8502`
   (provisioner); confirm the agent's `consul.host` + the admin-token hash are configured. New
   containers receive `CS_NODE_ID`; existing ones use the compatibility shim — no recreation needed.
3. **Firewall** — the agent renders the `cs_agent` nft table on start (`nft list table ip cs_agent`).
   The host firewall itself is applied at boot by `cs-iptables.service` (a oneshot that runs
   `/usr/local/bin/cs-recover_iptables`); the agent does not manage that file. **Edit that file
   directly** to delete the lines the agent has now taken over — the `expose-ports`/`container-inbound` chain setup — then **reboot**
   so the oneshot re-applies the trimmed ruleset from a clean slate (or, to avoid a reboot, delete
   those rules from the live ruleset by hand). Verify published ports still reach containers and
   `iptables -S` shows none of the old `expose-ports`/`container-inbound` artifacts.
   - **Rollback** — v2.0.0 is the *first* native release, so there is **no previous `.deb`**; the
     prior version ran as a Docker container, so rolling back means undoing the deployment-model
     change, not just downgrading a package:
       1. `sudo apt-get purge cs-agent` (removes the native binary + the `/lib/systemd/system` unit).
       2. Restore the old containerized `cs-agent.service` (the `docker run` unit) and pull the agent
          image — i.e. re-apply the previous provisioner config.
       3. Restore the host firewall: `sudo iptables-restore < /root/iptables.pre-upgrade`, **and**
          revert `/usr/local/bin/cs-recover_iptables` to the version that re-creates the
          `expose-ports`/`container-inbound` chains — the old containerized
          agent *appends* to those chains and silently loses published ports without them.
       4. Re-bind Consul's HTTP listener to `:8500` (the old agent and customer containers reach
          metadata via Consul there).
     From v2.0.1 onward rollback is a normal `apt-get install --allow-downgrades cs-agent=<prev>`;
     never roll back across a non-additive DB migration.

The controller/provisioner changes (`CS_NODE_ID` injection, the Consul port move, the host-firewall
trim, the apt source) ship alongside — coordinate per the rollout runbook.

## v1.10.0

- [FEATURE] Backup export ("download backup"): a new `backup.export` job streams a chosen archive to S3 (or S3-compatible storage) via `borg export-tar --bypass-lock` and publishes a presigned download URL to Consul KV (`borg/exports/<volume>/<jid>`) for ComputeStacks to read. Streams with no scratch disk, runs on a dedicated worker so it never blocks scheduled backups, and serializes against compaction per-repo. Configure under `backups.export.*`; inert until `backups.export.s3.bucket` is set. NOTE: the exported tar is plaintext (unlike the encrypted repo) — keep the bucket private, enable SSE, and use a short URL TTL + object-expiry lifecycle rule.

## v1.9.0

- [CHANGE] Move borg repository compaction from the backup server's host cron into the agent. It is scheduled per node (`backups.compact_freq`, with `backups.compact_jitter_sec` to spread load across nodes) and serialized against exports/prune via a per-volume lock. NFS-backed repositories are compacted locally on the server over SSH (`backups.borg.nfs_borg_path`). **Operators: remove the `cs-borg_compact` host cron once the fleet is upgraded; stagger the two during the overlap.**
- [FIX] Only advance a volume's `last_backup` timestamp when the backup actually succeeds. Previously it advanced even on failure, masking missed backups.
- [CHANGE] `borg create` now waits out an in-progress compact/prune (`backups.borg.lock_wait_create`, default 600s) rather than failing after 1 second and missing the backup.
- [CHANGE] Surface remote stderr from SSH commands (e.g. failed NFS compaction/chown) in agent logs.

## v1.8.0

- [CHANGE] Move docker network isolation under the responsibility of the backup agent.
- [FIX] Resolve crash during firewall reconciliation on nodes with no ingress rules defined.
- [FIX] Resolve crash when backing up a MySQL/MariaDB container that is offline or whose project event could not be created.
- [FIX] Resolve crash while stopping a backup container that failed to initialize.

## 1.7.0

* [CHANGE] Support for docker api v1.44
* [CHANGE] Refactor and update dependencies.

## 1.6.0

* [CHANGE] Bump dependencies to new major versions.
* [CHANGE] Support for Mariadb 11.

## 1.5.2

* [CHANGE] Include ssl in the docker image.

***

## 1.5.1

* [CHANGE] Our agent will now run inside of a container by default.

***

## 1.5.0

* [FEATURE] Support creating iptable rules for linux bridges.

***

## 1.4.2

* [FIX] Resolve issue that prevented cloning from an ssh target.
* [FIX] Fix cleaning up the backup folder when backing up MariaDB and MySQL containers.

***

## 1.4.1

* [CHANGE] make --lock-wait configurable in the yaml file.

***

## 1.4.0

* [FEATURE] Support backing up over SSH as an alternative to NFS.

***

## 1.3.6

* [FIX] Resolved error handling response from borg during volume deletion.

***

## 1.3.5

* [FIX] Resolve an issue that prevented volumes with backups disabled from being cloned.

***

## 1.3.4

* [CHANGE] Update system container images to use GitHub registry to avoid rate limits with Docker Hub.
* [CHANGE] Update borg to use `--numeric-ids` instead of the deprecated `--numeric-owner`.

***

## 1.3.3

* [CHANGE] MariaDB will use built-in MariaBackup, rather than a separate container.
* [FIX] Resolve MariaDB backup issues with v10.6+.

***

## 1.3.2

* [CHANGE] Additional tuning parameters available for MariaBackup.

***

## 1.3.1

* [CHANGE] Add in placeholder for MariaDB 10.10 that's in development.
* [CHANGE] Beginning with MariaDB 10.9, the container no longer includes the `MARIADB_MAJOR` environmental parameter, which we used to determine which version of maria backup to use. There is a request in with the MariaDB docker developer to add that back in, but for now we're defaulting to v10.9 if the `MAJOR` param is missing, but `VERSION` exists.

***

## 1.3.0

* [FEATURE] Restore backup from different volume.
* [CHANGE] Build arm64 binaries.
* [CHANGE] Configurable option to create nfs directory.
* [CHANGE] Issue `CHECKPOINT` command to postgres before taking snapshot.
* [CHANGE] Improvements to how container restores happen.
* [FIX] Resolve issue that left mysql/mariadb backup containers running after a restore.

***

## 1.2.5

* [FIX] Incorrectly flagged MySQL 5.6 as v8+.

***

## 1.2.4

* [CHANGE] Add in support for parsing the mariadb version used in a bitnami image.
* [FIX] Add additional checks to avoid nil pointer dereference when loading volume data from consul.

***

## 1.2.3

* [FEATURE] Support for consul token auth.

***

## 1.2.1

* [CHANGE] Hooks will now require at least 3 characters before executing.
* [FIX] Duplicate volume IDs in event log.

***

## 1.2.0

* [FEATURE] Support for excluding cache directories. `echo "Signature: 8a477f597d28d172789f06886806bc55" > CACHEDIR.TAG`

***

## 1.1.0

* [FEATURE] IPTable rules are now stored in consul, instead of having to poll the controller.

***

## 1.0.0

* [FEATURE] Manage iptables for udp container rules, and sftp containers.

***

## 0.4.0

* [FEATURE] Support for Bitnami's MariaDB using our MySQL backup tool.
* [CHANGE] A backup volume is created per-repository, and will auto-mount via NFS if applicable.
* [CHANGE] MySQL Backup jobs for offline containers will now show as "cancelled", and not "failed", in ComputeStacks.

***

## v0.3.1

* [FIX] Prune events will correctly stop their container after running.
* [FIX] Prune will correctly find the repo, and halt if it does not exist.

***

## v0.3.0

* [CHANGE] Restore will now completely clear the volume before restoring.

***

## v0.2.0

* [CHANGE] Uses docker container for backing up, instead of host system.
* [FIX] Various bug fixes

***

### Oct 19, 2020

* [CHANGE] Package Updates.
* [FIX] Resolve nil pointer error on posting events to ComputeStacks.

***

## v0.1.4

### June 27th, 2019

* [FIX] Incorrect parameters being passed to `borg prune`.
