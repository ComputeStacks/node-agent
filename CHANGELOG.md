# Changelog

## v3.2.1

Patch release for v3.2.0. **No migrations (`control.db` stays at schema `v4`), no config
changes, no API changes.** Three fixes in the path every borg operation goes through to open
a repository: a transient kernel `ENOMEM` on the repository volume's mount is now retried
rather than failing the operation, a failed image pull reports its reason instead of
panicking, and a repository that cannot be opened reports a readable reason rather than a
rendered log structure. **Upgrade if backups intermittently fail with `cannot allocate
memory`**, or if a failed backup's reported reason arrives as a block of mostly-empty fields.

One change alters behaviour rather than only reporting: a container start that fails on
`ENOMEM` is attempted three times over ten seconds before the failure is reported, so a
backup that is going to fail that way takes ten seconds longer to say so.

- [FIX] **A backup container whose volume mount fails on kernel `ENOMEM` is retried.** The
  volume mount happens inside `ContainerStart`, in the daemon, so a kernel that cannot
  allocate memory for it fails the whole operation — a backup, a prune, an export. Under
  memory pressure that refusal can come in windows of a few seconds, with mounts a second
  either side succeeding, which makes it exactly the shape a short retry converts into a
  success. Create-and-start is now attempted up to three times, five seconds apart.
  It is the **pair** that is re-attempted, not the start: the container runs with
  `AutoRemove`, and the daemon force-removes an `AutoRemove` container as soon as its start
  fails — before the start call returns — so re-starting the same container reports
  `No such container` and replaces the reason the operation actually failed. A named volume
  is exempt from that removal, so neither the daemon's nor the agent's cleanup can take the
  repository's cache volume with it. The gate is deliberately narrow, matching only the
  kernel's own `cannot allocate memory`: every other start failure is reported exactly as
  before, because a gate that matched more would spend the delay per volume across a whole
  sweep re-attempting conditions a retry cannot fix — an unreachable backup server, an image
  that is not there. Matched on the message text because there is no errno left to compare
  against by the time the daemon's error reaches the agent. **A retry logs at `WARN`** with
  the repository and the attempt, so a failure the retry absorbs still leaves a trace;
  without it a host that stopped producing the condition and a retry quietly covering for one
  cannot be told apart.
- [FIX] **A failed image pull reports its reason instead of panicking.** The pull-failure
  branch logged a variable that is guaranteed nil where it stands — the function has already
  returned on it — so a pull whose request failed took the agent into a nil dereference.
  `prune`'s `recover` caught it, which meant the whole maintenance sweep aborted and what was
  logged was a panic rather than the pull error. The branch also discarded the pull's response
  body, and that is the half that matters more often: `ImagePull`'s returned error covers the
  REQUEST only, so a pull the daemon then abandons — an unauthenticated registry, a tag that
  is not there — comes back with a nil error and reports the reason as an `error` record
  inside the stream. That stream is now read to completion, its first error record reported,
  and the body closed rather than leaked. Reading to completion is also what makes the
  container create that follows wait for a pull that has not finished.
- [FIX] **A repository that cannot be opened reports a readable reason.** The three paths
  that report a failed repository lookup or a failed `borg init` rendered the whole
  `LogMessage` structure, so a docker-level failure — which carries no msgid, levelname or
  timestamp — reached the controller as one line of text wrapped in five empty fields. The
  returned error had the opposite problem: it used the bare message, dropping the msgid. Both
  now carry the same `(msgid) reason`, matching the archive-create, restore and delete paths.
  This is what a consumer reading the task's accumulated output sees, so it is the reason an
  operator actually reads.

**Operator note — the kernel's own free-memory reserve.** The retry above is a fallback, not
a cure: the condition it covers is host-side memory pressure. `vm.min_free_kbytes` is
auto-computed at boot and **capped at 64 MB regardless of how much RAM the machine has**, so
on a large node with no swap and tens of gigabytes in page cache the global reserve is thin,
and an allocation that cannot wait fails at the watermark instead of stalling in reclaim.
Raising it — 512 MB is a reasonable starting point on a node with 64 GB or more, at the cost
of roughly that much page cache — is the host-side half of the first fix, and is worth doing
whether or not this release is installed:

```
# /etc/sysctl.d/99-computestacks-memory.conf
vm.min_free_kbytes = 524288
```

Upgrading is a plain `apt-get install cs-agent` per node — no `agent.yml` changes, no
controller coordination, and no maintenance window. Because there is no migration,
downgrading is a normal `apt-get install --allow-downgrades cs-agent=3.2.0`.

## v3.2.0

Diagnosability release for the restore path — **no migrations (`control.db` stays at schema `v4`)**,
one new configuration key (`backups.borg.lock_wait_restore`), and one additive task-result field
(`restore_warning`). A restore that fails partway is the case this release is about: until now it
reported no usable reason for it, and the reason is the only thing that says which files did not
come back.

It also carries a restore data-loss fix, one change to what a backup archives, and a group of fixes
to the `ssh` backup backend covering which repository an operation opens, which operations may
create one, and how repository names reach the backup server. **Upgrade if you run restores.** A
restore whose preparation step failed partway could destroy the volume's set-aside contents and
leave the service stopped — that is the most serious thing fixed here, and it is fixed for every
strategy.

**If you host WordPress, this release also stops a daily backup failure.** WordPress writes a
filesystem write-probe file and deletes it milliseconds later; `borg` catching it mid-flight
reported the whole backup as failed. The probe is now excluded from backups. Archives from backups
reported failed this way were always valid and restorable, so there is nothing to re-run.

Nine changes alter behaviour rather than only reporting, and each is called out inline below: a
restore that names individual file paths is refused; a restore waits up to 120 seconds for the
repository lock instead of 1; a failed restore's `result_json.error` carries the reason rather than
the literal string `task reported failure`, which matters if anything downstream matches on that
text; a restore whose preparation failed partway puts the volume's contents back, and leaves the
service stopped if it cannot; a restore no longer creates a repository; on the `ssh` backend a
restore whose source is a different volume now reads that volume's repository rather than the
destination's; only a backup creates a repository directory on the backup server; a node publishes
repository state only for its own volumes; and WordPress's write-probe file is no longer archived.

- [CHANGE] **A restore that names individual file paths is refused.** A restore replaces the whole
  volume — the volume's current contents are set aside, the archive is extracted over it, and the
  contents are put back only if the restore fails. A request naming individual paths cannot be
  honoured correctly within that model, so it now fails immediately, before anything is moved, with
  a reason saying so. Volumes using the `mysql`, `mariadb` and `postgres` strategies are unaffected:
  their restore has always used the whole archive, and a request carrying file paths continues to
  succeed with the paths ignored.
- [FIX] **`borg extract` no longer runs with `--error`.** The flag suppressed the `WARNING` record
  carrying `borg`'s explanation of its own exit, so a restore that failed partway could only report
  `borg extract exited 1: no diagnostic output`. `borg`'s diagnosis is now available to both the
  operator and the task result. This is the same reason `--error` is not passed to `borg create` or
  `borg delete`.
- [FIX] **A failed restore reports the file that failed, rather than a generic exception.** When
  `borg` cannot write a file it logs that file and the reason, and — depending on where the failure
  lands — may then also raise a generic top-level exception. The generic record explains nothing but
  outranked the useful one by severity, so a restore that ran out of disk space reported
  `Local Exception`. The reason now names the path and the error `borg` reported for it, and every
  such record is included in the task's output, so a restore that failed on several files lists
  them.
- [FIX] **A failed restore reports a readable reason as its error.** Every restore failure recorded
  its reason only in the task's accumulated output and left the task's own error as a generic
  "task reported failure"; where the reason came from `borg` it was rendered as an entire log
  structure, so what reached the controller was a mostly-empty record wrapped around one line of
  text. Restore failures now carry `(msgid) reason`, matching the backup path, and record that
  reason as the task's error.
- [FIX] **A restore whose contents could not be put back says so first.** When a restore fails, the
  volume's set-aside contents are put back; if that put-back itself fails, that is now the task's
  reported error rather than a line in the output behind the reason the restore failed. It is the
  condition that needs attention first, and it is reported for that case alone: a put-back that
  succeeded and a step after it that did not are now told apart, and the second is reported without
  claiming the volume was left unrestored.
- [FEATURE] **A completed restore that carried a warning reports it.** The task result now includes
  a `restore_warning` field with `borg`'s own diagnosis for a restore that completed with a record
  attached but no failure.
- [CHANGE] **`borg extract` waits longer for the repository lock.** It used `backups.borg.lock_wait`
  (1 second by default), so an in-agent prune or compact of the same repository, or a backup of it,
  could fail a restore that would otherwise have succeeded — and a failed restore is rolled back.
  The new `backups.borg.lock_wait_restore` defaults to 120 seconds. It is deliberately shorter than
  `lock_wait_create`: the wait happens with the service stopped and the volume already set aside, so
  it is bounded rather than maximised.
- [FIX] **A restore whose preparation failed partway no longer destroys the volume's contents.**
  Before extracting an archive, a restore stops the service and moves the volume's current contents
  aside into the backup container, so that a restore which fails can be undone. That move is a copy
  followed by a delete rather than a rename, so a failure partway through it leaves part of the
  volume in the container and the rest still in place — and the agent then tore that container down,
  taking the moved part with it, and returned without restarting the service. The contents are now
  put back before the container is torn down, and the service is started again. The put-back never
  overwrites a file the volume still has, so a half-copied file cannot displace the volume's own
  intact copy. Where the put-back itself cannot complete, the service is deliberately left stopped
  and the reported reason says so: a volume that may be missing data is not one to start an
  application over.
- [CHANGE] **WordPress's filesystem write-probe is no longer backed up.** WordPress writes
  `temp-write-test-<uniqid>` beside the directory it is testing, stats it, and deletes it
  milliseconds later, on every install. `borg` lists a directory and then reads each entry in turn,
  and a file removed between those two steps is recorded as a warning — so on WordPress volumes this
  probe could fail a backup daily while naming a file that had no business being in an archive. It
  is now excluded, so `borg` never walks it and no warning is raised. Files and directories whose
  names merely begin with `temp-write-test-` are unaffected: the exclusion matches the probe's exact
  shape, not the prefix.
- [CHANGE] **A restore no longer creates a repository.** On the `ssh` backend a restore used to run
  `borg init` when `borg` reported the repository missing, on the belief that the missing repository
  was the destination's. It is the source's — the volume the archive is read from — so that verdict
  means there is nothing to restore from. A restore whose source has never been backed up now fails
  immediately with `borg`'s own reason, instead of creating an empty repository on the backup server
  and then failing at the archive lookup anyway.
- [CHANGE] **Only a backup creates a repository directory on the `ssh` backup server.** Every
  operation that opened a repository — restores, exports, prunes, archive deletes — used to `mkdir`
  its directory on the backup server on the way past, including for volumes that had never been
  backed up. Creating a repository is now a backup's job alone. An existing backup server may hold
  empty `b-<volume>/backup` directories left behind by those operations; they are inert, and no more
  will appear.
- [CHANGE] **A node publishes repository state only for its own volumes.** The `repositories` row a
  node reports is keyed on the repository's name, and an archive delete can name a repository
  belonging to a different volume than the task does. A node no longer publishes a row for a
  repository it does not own.
- [FIX] **A short pull no longer publishes an apt index that drops every older version.** A release
  publishes by downloading the existing package pool, rebuilding the index from whatever that
  download left on disk, and uploading the result — so a download that quietly came back short
  published an index listing only the release being cut, silently un-installing every version before
  it. The pool download now checks each object against the size the store reported, retries a
  connection reset that happens while the response body is being read (which the AWS SDK's own
  retryer cannot cover, because the request itself already succeeded), gives each attempt a deadline
  so a stalled transfer cannot hang the job with the release already cut, refuses an empty listing,
  and fails the whole pull rather than one object. Re-running a release tag is idempotent now too.
  This matters because the rollback instructions further down these notes depend on older versions
  staying installable.
- [FIX] **A failed backup now reports the warning worth acting on, not the loudest one.** `borg`
  logs one record per file it warned about, and a failed backup reports one of them — the rest are
  only in the node's debug log. It reported whichever came first, and on a volume with an active
  application the files that vanished while `borg` walked them are the overwhelming majority, so the
  one record that says a file could not be READ — the file that is missing from the archive and
  still sitting on the volume — was buried behind a path that had simply been deleted. A warning
  that means data was left out of the archive is now reported ahead of one that only means a file
  went away. Which warnings fail a backup is unchanged; only which of them is quoted.
- [FIX] **Repository names are validated before they are used in commands on the backup server.** The
  agent builds a small number of commands that run on the backup server over SSH — creating and
  removing a repository's directory — and a repository name reached those commands without being
  checked first. Names are now validated against the same rules Docker applies to volume names, and a
  name that does not match is refused rather than used. Names produced by the controller have always
  matched, so this changes nothing for a normal installation.
- [FIX] **On the `ssh` backend, restoring one volume's backup into a different volume opened the wrong
  repository.** Restoring an archive from volume A into volume B — the operation behind a volume
  clone — looked for A's archive in **B's** repository. Depending on whether B had ever been backed
  up, the restore either failed reporting that the archive did not exist, failed reporting an invalid
  repository, or succeeded. It now always reads the repository belonging to the volume the archive
  came from. Restores where the source and destination are the same volume were unaffected, as were
  the `local` and `nfs` backends.
- [FIX] **On the `ssh` backend, a repository that failed to initialize stayed failed.** A volume's
  first backup creates its repository. If the repository's directory had already been created but the
  repository itself had not, the agent did not recognise that state, so the backup failed — and kept
  failing on every subsequent attempt, because the condition that would have retried the setup was
  never met again. That state is now recognised and the repository is initialized, so a volume already
  stuck this way recovers on its next scheduled backup with no operator action.

## v3.1.2

Patch release for v3.1.1. **No migrations (`control.db` stays at schema `v4`), no config changes,
and no breaking API changes** — a task result gains one additive field (`backup_warning`, below).
Upgrade if any volume is written to while it is being backed up: on those volumes every backup is
reported as failed even though the archive was created correctly.

- [FIX] **A backup that `borg` completes with a warning is no longer reported as failed.** `borg`
  exits `1` when a command reaches its normal end but logged a warning, and v3.1.0 began treating
  every non-zero exit as a failure. The common case is a file being written while `borg` reads it
  (`file changed while we backed it up`), which happens on any volume with an active application.
  The archive is complete and restorable in that case, so the task now completes, `last_backup`
  advances, and the repository is synced. **Archives created before upgrading are valid and
  restorable** — the backup itself succeeded; only the reported outcome was wrong, so there is
  nothing to re-run.
- [FIX] **A warning that means data is missing from the archive still fails the backup.** Not every
  `borg` warning is harmless: when `borg` cannot read a file it logs the file, skips it, and commits
  an archive without it — at the same exit code and the same severity as the harmless case. The two
  are told apart by `borg`'s own `msgid`, and only an explicitly recognised harmless warning is
  allowed to complete. An unrecognised warning fails the task and reports `borg`'s message, so a
  backup is never recorded as successful on the strength of a warning that has not been assessed.
  A docker-level fault is also never mistaken for a `borg` warning, whatever exit code it carries.
- [FEATURE] **A completed backup that carried a warning reports it.** The task result now includes
  a `backup_warning` field with `borg`'s own diagnosis — for the case above, the name of the file
  that changed while it was being read, whose copy in that archive may therefore be inconsistent.
- [FIX] **`borg create` no longer runs with `--error`.** The flag suppressed the `WARNING` record
  carrying `borg`'s explanation of its own exit, so a failed backup could only report
  `borg create exited 1: no diagnostic output`. `borg`'s diagnosis is now available to both the
  operator and the task result. This is the same reason `--error` is not passed to `borg delete`.
- [FIX] **A failed backup reports a readable reason.** The reason was rendered as an entire `borg`
  log structure, so what reached the controller was a mostly-empty record wrapped around one line of
  text. Failures now carry `(msgid) reason`, matching every other backup failure path, and a failed
  archive creation records that reason as the task's error rather than a generic "task reported
  failure". Where several files were warned about, the reason names the one that actually failed the
  backup, rather than whichever `borg` happened to encounter first.
- [CHANGE] **A `borg create` that exits on the warning tier is logged at DEBUG, not WARN.** The
  borg-layer log line for a non-zero exit is `Command failed`, which is misleading for a warning the
  agent goes on to accept, and it would otherwise appear on every successful backup of a busy
  volume. **If you alert on that string, note that it no longer appears for `borg create` exit 1.**
  A create that genuinely fails still logs at WARN, from the layer that makes the decision, and
  every other command is unchanged — `borg delete` exiting 1 found no archive to delete and stays
  visible.

**Operator note — a volume with a `PostBackup` command.** While a backup was being misreported as
failed, `postBackup` ran only when `backup_error_cont` was set (the `mysql` and `postgres`
strategies force it, so they were unaffected). A volume on another strategy that configures a
`PostBackup` command did not run it on those backups, so whatever that command undoes may have been
left in place until the volume's next successful backup. Worth checking once after upgrading if you
rely on one.

## v3.1.1

Patch release for v3.1.0. **No migrations (`control.db` stays at schema `v4`), no config changes,
no API changes.** Upgrade if you run MariaDB 11.1 or newer: on those servers every `mysql`-strategy
restore and volume clone fails, and the failure is reported as a success.

- [FIX] **A `mysql` restore accepts a dump from MariaDB 11.1 or newer.** MariaDB 11.1 renamed
  `mariadb-backup`'s metadata files, so a dump from those versions carries
  `mariadb_backup_checkpoints` where an older one carries `xtrabackup_checkpoints`. v3.1.0 looked
  only for the older name and rejected the dump as "not a prepared mysql dump", failing the restore
  and rolling it back; a volume clone of such a service failed the same way and left the new volume
  empty. Both names are now accepted, and the older one stays supported — Percona `xtrabackup` still
  writes it, and archives taken before an upgrade restore for as long as they are retained.
- [FIX] **A restore that rolls back is reported as failed.** When the post-restore step failed, the
  agent restored the volume's previous contents and then reported the task as completed, so a failed
  restore appeared to succeed and a volume clone finished over an empty volume. The task now fails,
  and carries the reason it failed.

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

**Operational note — an interrupted restore should be re-run, not assumed rolled back.** The
rollback described above is what the agent does when a restore *fails*; it is not a recovery
mechanism for the agent itself being stopped mid-restore. A restore cut short by node-level failure
— the process killed, an OOM, a reboot — is marked failed on the next start and is never replayed
automatically, because no task that can destroy data is ever auto-resumed. Treat the volume as
indeterminate and re-run the restore.

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
   renders (`nft list table ip cs_agent`), tasks dispatch, and task status/results flow up the
   changelog with the ack watermark advancing. **Pruning is not a gate for this window** — both
   prune rules are age-floored (acked rows at `changelog.prune_min_age_sec`, 7d; the
   ack-independent fallback at `changelog.prune_max_age_sec`, 30d), so no row can be pruned
   during the cutover no matter how promptly the controller acks. Confirm the changelog is
   shrinking a week later, not here.
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
