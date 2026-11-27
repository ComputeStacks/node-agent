# Bucket setup — export + apt (UpCloud Managed Object Storage / Dell ECS)

Tooling to provision the buckets the cs-agent exports backups to. The target is
**UpCloud Managed Object Storage** — S3-compatible, on a **Dell ECS** backend
(`server: ViPR/1.0`, `urn:ecs:iam::...` URNs), reached over a custom-domain endpoint
(`backups.export.s3.endpoint`). The cs-agent uses **S3 only** (PutObject + presigned
GET); IAM/STS are only used here, at setup time.

It satisfies the two requirements:
- **Deny access except via signed URLs** — ECS buckets are private by default, so the
  only read path is a request signed by an object user (the agent's presigned URLs).
  The agent runs as a dedicated least-privilege user in a group.
- **Auto-delete exports** — a 1-day bucket lifecycle rule (1-day is the S3 minimum).

## `setup-export-bucket.sh`

Idempotent, additive, safe to re-run. Given a bucket (and optionally a group + user):

1. creates the **bucket** if absent;
2. creates the **group** if absent, and ensures a group-managed policy grants export
   access to the bucket — **additive**: an existing policy gains the bucket via a new
   policy version, so buckets already attached keep their access (never overwritten);
3. creates the **user** if absent and adds it to the group (every group member can
   then access all the group's buckets);
4. applies the **1-day lifecycle** to the bucket;
5. prints a **paste-ready `agent.yml`** (`backups.export.s3`) block for the node.

```bash
# full setup (bucket + group + user), mint a key and print it into the YAML:
BUCKET=team-exports GROUP=team USER=team-agent CREATE_KEY=1 \
  ENDPOINT=https://s3.example.com ./setup-export-bucket.sh

# add another bucket to the SAME group (additive — existing buckets keep access):
BUCKET=team-exports-2 GROUP=team ENDPOINT=https://s3.example.com ./setup-export-bucket.sh

# bucket + lifecycle only (no IAM):
BUCKET=solo-exports ENDPOINT=https://s3.example.com ./setup-export-bucket.sh
```

**Env vars:** `BUCKET` and `ENDPOINT` required. `GROUP` optional; `USER` optional
(requires `GROUP`). Options: `PREFIX=exports/`, `POLICY=<group>-export`,
`LIFECYCLE=1`, `CREATE_KEY=0`. Credentials/region come from the **default CLI
profile** — use an account/owner key, not the scoped agent user.

`CREATE_KEY=1` mints an access key for `USER`, fills it into the printed YAML, and
also writes the YAML to `./<user>.creds` (chmod 600 — copy to the node, then delete).
Without it the YAML has `REPLACE_WITH_*` placeholders and instructions.

Per-bucket access granted by the group policy (least privilege for the export agent):
`PutObject`/`GetObject`/`AbortMultipartUpload`/`ListMultipartUploadParts` on
`<bucket>/<prefix>*`, and `ListBucketMultipartUploads` on `<bucket>`.

### Output

The script ends with the exact block to paste into the node's `agent.yml`:

```yaml
backups:
  export:
    workers: 1
    tar_filter: "gzip"
    timeout_sec: 14400
    cleanup_freq: "*/30 * * * *"
    failed_retention_sec: 86400
    s3:
      endpoint: "https://s3.example.com"
      region: "europe-1"
      bucket: "team-exports"
      prefix: "exports/"
      access_key: "AKIA..."      # filled when CREATE_KEY=1, else REPLACE_WITH_*
      secret_key: "..."
      force_path_style: false
      part_size_mb: 64
      concurrency: 4
      sse: "AES256"
      default_ttl_sec: 43200
      max_ttl_sec: 86400
```

## `setup-store-backup-bucket.sh`

Provisions the bucket for the off-node SQLite backups (`store.backup`). Idempotent.
Creates the bucket (with **no lifecycle rule**: the agent prunes its own snapshots),
a group with a policy for that bucket, and a user in the group, then prints a
`store.backup` block for `agent.yml`.

```bash
BUCKET=cs-store-backups GROUP=cs-store-backup USER=cs-store-backup-agent \
  RECIPIENT=age1... CREATE_KEY=1 ENDPOINT=https://s3.example.com \
  ./setup-store-backup-bucket.sh
```

Access granted: `PutObject`/`GetObject`/`DeleteObject`/`AbortMultipartUpload`/
`ListMultipartUploadParts` on `<bucket>/<prefix>*`, and `ListBucket`/
`ListBucketMultipartUploads` on `<bucket>`. `PREFIX` defaults to `sqlite/`. An
existing policy is left unchanged. Generate the age key pair with `age-keygen` on a
machine other than the node, and keep the identity file off the node.

## `setup-apt-bucket.sh`

Provisions the **public apt repository** bucket — the read-OPEN counterpart to the export
bucket. One bucket holds the whole repo (`dists/` + `pool/`) for every ComputeStacks **OSS**
package, served **anonymously over HTTPS** so nodes `apt-get` with no credentials. Idempotent /
additive; shares the `~/.aws/config` prereqs + ECS gotchas below.

Differs from the export bucket: **anonymous public-read** (`GetObject` only — no `ListBucket`, so
it can't be enumerated; verified that ECS honors a `Principal:"*"` policy + serves anonymous GET
with a real 404 on a missing key), **no object-expiry lifecycle** (`pool/` is append-only for
rollback), and the IAM user is a **CI publisher** (Put/Get/Delete on `<bucket>/*` + ListBucket)
whose key becomes the GitHub `release` Environment's `APT_S3_*` secrets.

```bash
# bucket + anonymous-read policy only:
PROFILE=cs-repo ENDPOINT=https://repo.computestacks.com BUCKET=public ./setup-apt-bucket.sh

# + publisher IAM user, mint its key (secret → ./<iam_user>.creds, chmod 600, NEVER printed):
PROFILE=cs-repo ENDPOINT=https://repo.computestacks.com BUCKET=public \
  GROUP=apt-publishers IAM_USER=apt-publisher CREATE_KEY=1 ./setup-apt-bucket.sh
```

**Env vars:** `ENDPOINT` required; `BUCKET` (default `public`); `PROFILE` (CLI profile to use);
`GROUP` optional; `IAM_USER` optional (requires `GROUP` — note `IAM_USER`, not `USER`, to avoid the
shell's login-name env var); `CREATE_KEY=1` mints + writes the key to `./<iam_user>.creds`;
`ABORT_MPU_DAYS` (default `0` = no lifecycle at all). It prints the GitHub `release` Environment
vars + secrets and the node `sources.list` line.

**Live:** `public` bucket provisioned at `https://repo.computestacks.com/public` (anonymous read
verified); signing key served at `…/public/computestacks.gpg.asc`; publisher user `apt-publisher`.

## Prerequisites — `~/.aws/config`

The CLI must point at this ECS instance and work around its quirks. The default
profile here is already set up like this (see the gotchas below for why):

```
[default]
region = europe-1
services = upcloud
request_checksum_calculation = when_required
response_checksum_validation = when_required
[services upcloud]
s3  = { endpoint_url = https://s3.example.com }
iam = { endpoint_url = https://s3.example.com:4443/iam }
sts = { endpoint_url = https://s3.example.com:4443/sts }
```

## ECS gotchas (all discovered + worked around)

1. **IAM/STS are on port `4443`, not 443.** ECS serves S3 on 443 but the IAM/STS
   control API on 4443. With the UI-provided 443 endpoints, `aws iam`/`aws sts` fail
   with `SignatureDoesNotMatch`. STS `GetCallerIdentity` is unimplemented (returns
   `InvalidAction`) — test connectivity with `aws iam get-user`.
2. **Default request checksums (CRC32) are rejected** with `XAmzContentSHA256Mismatch`
   on uploads. CLI: `request_checksum_calculation = when_required`. **Agent**
   (`s3upload.go`) needs it in **two** independent places — the S3 client (single
   PutObject) **and** the `manager.Uploader` (multipart `UploadPart`, whose own setting
   defaults to `WhenSupported`). Both are now set; validated by a 12 MiB multipart
   round-trip.
3. **`PutBucketLifecycleConfiguration` needs `Content-MD5`**, which the new CLI no
   longer sends — so the script applies the lifecycle via `curl --aws-sigv4` with a
   computed `Content-MD5`, not the CLI.
4. **One lifecycle rule per prefix** — ECS rejects duplicate prefixes, so expiration +
   multipart-cleanup share one rule.
5. `create-bucket` and IAM **policy versions** are supported (used above).

## Status

Live `dev` bucket already provisioned: group `dev` → policy `dev-export` (scoped to
`dev/exports/*`) → user `dev-agent`; 1-day lifecycle applied. End-to-end upload
(multipart) + presign + download verified against it.
