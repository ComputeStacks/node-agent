# cs-agent — native packaging & apt distribution

Production runs the agent as a **native binary under systemd**, installed from a
self-hosted, GPG-signed apt repo on S3. The container build (`Dockerfile`) is kept
for local dev only.

## Release flow (automated)

`git tag vX.Y.Z && git push --tags` → `.github/workflows/release.yml`:

1. GoReleaser builds amd64+arm64, packages the `.deb` (nfpm), and cuts the GitHub
   Release with notes extracted from `CHANGELOG.md`.
2. `apt-publish pull` downloads the existing `pool/` from S3.
3. The new `.deb`s are dropped into the pool; `build-apt-repo.sh` rebuilds the
   index from the **full** pool and signs `Release`/`InRelease`.
4. `apt-publish push` uploads the repo (signed Release files last).

The pool is **append-only** (every version stays installable → rollback); the
index is a regenerable function of the pool (self-healing).

Because step 3 rebuilds the index from whatever step 2 left on disk, a pull that
quietly comes back short would publish an index listing only the release being
cut — silently un-installing every older version. So `pull`:

- **refuses an empty listing.** `--allow-empty` overrides it, but deliberately
  cannot be reached from CI — see "Bootstrapping a new pool" below;
- **fails the whole pull if any single object fails**, after retrying it;
- writes each object to a `.part` file and renames on success, and checks the
  bytes received against the size the listing reported — so a file present in the
  pool is a whole object, not a truncated one whose own hashes the index would
  happily publish as authoritative (a corruption the daily reconcile cannot
  detect, because it re-derives the index from the same bytes);
- retries each object up to 4 times with exponential backoff. The AWS SDK's own
  retryer cannot cover this: a reset while draining the response body happens
  *after* `GetObject` returned success, so the request layer never sees it. An
  unretried reset there failed the v3.1.2 publish;
- gives each attempt a 5-minute deadline, because a *stalled* stream — as opposed
  to a reset one — produces no error for the retry loop to react to, and would
  otherwise hang the job until the runner's own limit with the release already
  cut. Both jobs also carry `timeout-minutes` as a backstop, which matters because
  a hung job holds the shared `apt-publish` concurrency group and so blocks the
  reconcile that would heal the repo;
- does not retry a 4xx other than 408/429 — a rotated key or a missing object
  cannot be fixed by waiting, and burning the backoff only delays an error the
  operator has to act on;
- rejects a key that would resolve outside the working directory. Keys are data
  from the store, and the pool is intended to be shared with other packages' CI.

Re-running a tag is safe: `replace_existing_artifacts` lets GoReleaser overwrite
assets it already uploaded, so a failure in steps 2–4 can simply be retried.

## Bootstrapping a new pool

Neither workflow can pass `--allow-empty`, and that is on purpose. `release.yml`
holds the signing credentials behind a protected Environment with required
reviewers, and an accidental `--allow-empty` there would publish an index built
from an empty pool — listing only the release being cut and silently
un-installing every older version, which is the failure the refusal exists to
prevent. Widening that workflow's trigger surface to carry an escape hatch costs
more than the rare manual step below.

So an empty pool — a brand-new bucket, or a changed `APT_S3_PREFIX` — is seeded by
hand, once, before the first release. With the same `APT_S3_*` variables and AWS
credentials the workflows use, exported locally:

```sh
mkdir -p aptrepo
go run ./cmd/apt-publish pull aptrepo --allow-empty   # confirms creds; downloads nothing
mkdir -p aptrepo/pool/main/c/cs-agent                 # pull created no pool/ — it was empty
cp /path/to/cs-agent_X.Y.Z_*.deb aptrepo/pool/main/c/cs-agent/
APT_GPG_KEY_ID=... ./scripts/build-apt-repo.sh aptrepo
go run ./cmd/apt-publish push aptrepo
```

After that the pool is non-empty and both workflows run unmodified — `pull` finds
objects and never needs the flag again.

Two things to know if you are doing this:

- **`--allow-empty` applies to `pull` only.** `parseArgs` rejects it on `push`, so
  a mistyped command fails rather than doing something adjacent to what was meant.
- **A relative directory works**, including `.` — `apt-publish pull .` is a valid
  invocation. It was not always: until v3.2.0 the containment check rejected every
  key when the target directory was `.` or empty, which broke exactly this
  bootstrap path while leaving CI (which passes `aptrepo`) unaffected.

## One-time setup

**GitHub Actions — Environment `release`** (add required reviewers):

| Kind | Name | Notes |
|---|---|---|
| secret | `GPG_PRIVATE_KEY` | dedicated **signing subkey**, ASCII-armored (not the master key) |
| secret | `GPG_PASSPHRASE` | passphrase for the subkey |
| secret | `APT_S3_ACCESS_KEY` | S3 access key (→ `AWS_ACCESS_KEY_ID`) |
| secret | `APT_S3_SECRET_KEY` | S3 secret key (→ `AWS_SECRET_ACCESS_KEY`) |
| var | `APT_S3_ENDPOINT` | S3 endpoint, e.g. `https://s3.example.com` |
| var | `APT_S3_REGION` | e.g. `us-east-1` |
| var | `APT_S3_BUCKET` | shared **public-read** bucket for all ComputeStacks OSS packages (e.g. `cs-packages`); separate from the private backup bucket |
| var | `APT_S3_PREFIX` | **shared** repo root — the **same** for every package (empty = bucket root, or e.g. `apt/`); all packages publish into the one `dists/`+`pool/` |
| var | `APT_PULL_CONCURRENCY` | *optional*, default `8`. In-flight `GetObject`s during `pull`. Lower it if the store dislikes parallel reads; an unparseable value falls back to the default rather than failing the publish |

**S3 bucket (one shared repo):** a **single** apt repo serves every ComputeStacks **OSS**
package — one `dists/` index over one `pool/` holding all packages — signed with **one
shared key**. Nodes add it **once** and `apt install <any-cs-package>`; new packages need
no node change. Make the bucket **anonymously readable over HTTPS** via a **bucket policy**
(not per-object ACLs — some S3-compatible stores handle those differently); fits the
existing bucket-setup tooling.

> **Public bucket = OSS packages only.** Keep any proprietary/internal package out of it
> (distribute those via GitLab or a private, credentialed repo). cs-agent is OSS → fine here.

## Node setup (install + upgrade)

Dropped **once** at provisioning — never touched again, even as new packages ship:

```sh
# one shared keyring for ALL ComputeStacks packages
curl -fsSL https://<base>/computestacks.gpg.asc | gpg --dearmor \
  | sudo tee /etc/apt/keyrings/computestacks.gpg >/dev/null

# ONE source line for the whole ComputeStacks repo
echo "deb [signed-by=/etc/apt/keyrings/computestacks.gpg] https://<base> stable main" \
  | sudo tee /etc/apt/sources.list.d/computestacks.list

sudo apt-get update && sudo apt-get install -y cs-agent          # ...or: cs-agent foo-package
```

`<base>` = the public HTTPS URL for the shared repo (e.g. `https://pkg.computestacks.com`
fronting `s3://cs-packages` at `APT_S3_PREFIX`).

Bucket layout (one index, one pool, many packages):

```
s3://cs-packages/                 (public-read; one shared signing key)
  dists/stable/main/binary-{amd64,arm64}/Packages   ← single shared index
  dists/stable/{Release,InRelease,Release.gpg}
  pool/main/c/cs-agent/cs-agent_*.deb
  pool/main/f/foo-package/foo-package_*.deb          ← future packages just join the pool
```

## Scaling to multiple packages (the shared-repo tooling)

Today **cs-agent's `release.yml` is the sole publisher** of the shared index, so it's
race-free. Two pieces keep it that way as the repo grows:

- **`reconcile-apt-repo.yml`** (scheduled + manual) rebuilds the index from the **full
  pool** and re-signs, on the same `apt-publish` concurrency group as releases. This
  guarantees the index always matches the pool — self-healing any partial publish — and is
  the seed of the single index-builder below.
- **When package #2 ships** (a second, independent CI): do **not** have it rebuild + sign
  the shared index too — that races on the index and copies the signing key into a second
  CI. Instead, package CIs only **upload their `.deb` to the shared `pool/`**, and a
  **single index-builder** (one workflow, holding the *only* copy of the signing key)
  rebuilds + signs + publishes the index — triggered by the package CIs (`repository_dispatch`)
  and/or the reconcile cron. Race-free, and the key lives in exactly one place.
- **Decision for then:** where that index-builder + key live. Recommend a **dedicated infra
  repo** (not the public cs-agent repo) — especially since some future packages are internal
  (GitLab), and they'd dispatch to a neutral builder rather than into an OSS GitHub repo.

Updates are just `apt-get update && apt-get upgrade` (per node today; fleet-wide
via Ansible later).

## Rollback

Every version stays in the pool:

```sh
apt list -a cs-agent                          # see all available versions
sudo apt-get install --allow-downgrades cs-agent=1.9.0
sudo apt-mark hold cs-agent                   # pin so `upgrade` won't move it back up
```

> **Rollback safety:** the binary rolls back trivially, but the agent runs SQLite
> migrations on boot. Rollback is only safe because migrations are
> **additive-by-default + schema-version-guarded**. Don't
> roll back across a non-additive migration without the matching down-migration.

## Status / caveats

- The systemd unit ships with `Type=simple`. `Type=notify` + `WatchdogSec` are
  commented out until the agent implements `sd_notify`.
- **`push` has an inconsistency window on every push, not just a failed one.**
  Uploads are ordered `Packages` → pool → `Release`/`InRelease`, so for the
  duration of the push the *new* `Packages` is live while the *old* `InRelease`
  still describes it. A node running `apt-get update` inside that window fetches
  a matched pair from neither side and fails with a hash-sum mismatch, and there
  is no by-hash path to fall back to. Ordering Release-meta last only shrinks the
  window; it does not close it. That window is currently ~14s, twice a day
  (release + reconcile), and grows with the pool because the whole pool is
  re-uploaded each time. **Enabling by-hash in `build-apt-repo.sh` is the actual
  fix** (see the note at the end of that script) — this caveat exists to inform
  that decision, so don't read it as a rare-failure footnote.
- `push` re-uploads the entire pool every release, not just the new `.deb`s, and
  has no equivalent of `pull`'s size verification.
- **The pool is never pruned.** It is append-only by design, so that every version
  stays installable — but nothing bounds its growth, and both the transfer cost
  and the push window above scale with release history. Pruning is a policy call
  (it removes the ability to pin or roll back to a pruned version), so it is
  deliberately not automated; revisit when the pool becomes inconvenient.
- **`pull`'s parallel reads are new, and only partly verified.** Every release up
  to and including v3.1.2 pulled the pool one object at a time. Because addressing
  is path-style with an empty prefix, the authenticated URL and the public apt URL
  are the same URL, so the read path can be measured anonymously with `curl`:
  12 objects / 75 MB took **29.7s serial, 9.5s at 4-way, 5.9s at 8-way**, all
  `200`, no `503 SlowDown` and no resets. That was from a workstation, *not* from
  a GitHub Actions runner, so the network path in CI is still unproven. If the
  store throttles or caps concurrent reads there, the symptom is a failed `pull`
  — recoverable by setting `APT_PULL_CONCURRENCY=1` (an environment variable, no
  release needed) and re-running the tag.
- Validate the `.deb` `Depends:` package names (`borgbackup`, `iptables`,
  `ca-certificates`) on Debian 12/13.
