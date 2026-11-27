#!/usr/bin/env bash
# Provision the bucket for the agent's off-node SQLite backups (store.backup) on
# UpCloud Managed Object Storage (Dell ECS backend). Idempotent — safe to re-run.
#
# Given a bucket, group and user it will:
#   1. create the BUCKET if absent (no lifecycle rule: the agent prunes its own
#      snapshots, and a bucket expiry would delete the snapshots it keeps);
#   2. create GROUP if absent, and a group-managed policy granting the agent
#      access to BUCKET (left unchanged if it already exists);
#   3. create USER if absent and add it to GROUP;
#   4. print a paste-ready agent.yml (store.backup) block.
#
# Access granted (what the agent's backup and `store-restore` need):
#   PutObject, GetObject, DeleteObject, AbortMultipartUpload,
#   ListMultipartUploadParts on <bucket>/<prefix>* ;
#   ListBucket, ListBucketMultipartUploads on <bucket>.
#
# Run with an account/owner key (the default CLI profile), NOT the agent user.
#
# Usage:
#   BUCKET=cs-store-backups GROUP=cs-store-backup USER=cs-store-backup-agent \
#   ENDPOINT=https://s3.example.com CREATE_KEY=1 ./setup-store-backup-bucket.sh
#
# Options (env): PREFIX=sqlite/  POLICY=<group>-store-backup  CREATE_KEY=0
#   RECIPIENT=age1...  (fills the age public key into the printed YAML)
#   CREATE_KEY=1 mints an access key for USER, fills it into the YAML, and writes
#   the YAML to ./<user>.creds (chmod 600).
set -euo pipefail

: "${BUCKET:?set BUCKET}"
: "${GROUP:?set GROUP}"
: "${USER:?set USER}"
: "${ENDPOINT:?set ENDPOINT, e.g. https://s3.example.com}"
: "${PREFIX:=sqlite/}"
POLICY="${POLICY:-${GROUP}-store-backup}"
RECIPIENT="${RECIPIENT:-REPLACE_WITH_AGE_PUBLIC_KEY}"
REGION="${AWS_REGION:-$(aws configure get region || echo us-east-1)}"

tmp="$(mktemp -d)"; trap 'rm -rf "$tmp"' EXIT
log(){ printf '==> %s\n' "$*"; }

# ---- 1. bucket ---------------------------------------------------------------
if aws s3api head-bucket --bucket "$BUCKET" >/dev/null 2>&1; then
  log "bucket '$BUCKET' already exists"
else
  log "creating bucket '$BUCKET'"
  aws s3api create-bucket --bucket "$BUCKET" >/dev/null
fi

# ---- 2. group + policy -------------------------------------------------------
if group_arn="$(aws iam get-group --group-name "$GROUP" --query 'Group.Arn' --output text 2>/dev/null)"; then
  log "group '$GROUP' already exists"
else
  log "creating group '$GROUP'"
  group_arn="$(aws iam create-group --group-name "$GROUP" --query 'Group.Arn' --output text)"
fi
acct="$(printf '%s' "$group_arn" | cut -d: -f5)"
policy_arn="urn:ecs:iam::${acct}:policy/${POLICY}"

cat > "$tmp/policy.json" <<JSON
{
  "Version": "2012-10-17",
  "Statement": [
    {"Sid": "StoreBackupObjects", "Effect": "Allow",
     "Action": ["s3:PutObject", "s3:GetObject", "s3:DeleteObject",
                "s3:AbortMultipartUpload", "s3:ListMultipartUploadParts"],
     "Resource": ["arn:aws:s3:::${BUCKET}/${PREFIX}*"]},
    {"Sid": "StoreBackupBucket", "Effect": "Allow",
     "Action": ["s3:ListBucket", "s3:ListBucketMultipartUploads"],
     "Resource": ["arn:aws:s3:::${BUCKET}"]}
  ]
}
JSON

if aws iam get-policy --policy-arn "$policy_arn" >/dev/null 2>&1; then
  log "policy '$POLICY' already exists — left unchanged"
else
  log "creating policy '$POLICY' for '$BUCKET/$PREFIX'"
  aws iam create-policy --policy-name "$POLICY" --policy-document "file://$tmp/policy.json" >/dev/null
fi
if ! aws iam list-attached-group-policies --group-name "$GROUP" --query 'AttachedPolicies[].PolicyArn' --output text 2>/dev/null | grep -qF "$policy_arn"; then
  log "attaching policy '$POLICY' to group '$GROUP'"
  aws iam attach-group-policy --group-name "$GROUP" --policy-arn "$policy_arn" >/dev/null
fi

# ---- 3. user + membership ----------------------------------------------------
if aws iam get-user --user-name "$USER" >/dev/null 2>&1; then
  log "user '$USER' already exists"
else
  log "creating user '$USER'"
  aws iam create-user --user-name "$USER" >/dev/null
fi
if aws iam get-group --group-name "$GROUP" --query 'Users[].UserName' --output text 2>/dev/null | grep -qw "$USER"; then
  log "user '$USER' already in group '$GROUP'"
else
  log "adding user '$USER' to group '$GROUP'"
  aws iam add-user-to-group --group-name "$GROUP" --user-name "$USER" >/dev/null
fi

# ---- 4. paste-ready agent.yml ------------------------------------------------
akid="REPLACE_WITH_ACCESS_KEY"
asec="REPLACE_WITH_SECRET_KEY"
note="# credentials are placeholders — mint a key with: aws iam create-access-key --user-name ${USER}  (or re-run with CREATE_KEY=1)"
if [ "${CREATE_KEY:-0}" = "1" ]; then
  creds="$(aws iam create-access-key --user-name "$USER" --output json)"
  akid="$(printf '%s' "$creds" | python3 -c 'import sys,json;print(json.load(sys.stdin)["AccessKey"]["AccessKeyId"])')"
  asec="$(printf '%s' "$creds" | python3 -c 'import sys,json;print(json.load(sys.stdin)["AccessKey"]["SecretAccessKey"])')"
  note="# access key minted for '${USER}' — the secret is shown ONLY here (and ./${USER}.creds); store it now."
fi

yaml="$(cat <<YAML
store:
  backup:
    enabled: true
    recipients: ["${RECIPIENT}"]
    s3:
      endpoint: "${ENDPOINT}"
      region: "${REGION}"
      bucket: "${BUCKET}"
      prefix: "${PREFIX}"
      access_key: "${akid}"
      secret_key: "${asec}"
      force_path_style: false
      sse: "AES256"
YAML
)"

echo
echo "========== agent.yml  (merge into the node's existing store: section) =========="
echo "$note"
echo "$yaml"
echo "================================================================================"
if [ "${CREATE_KEY:-0}" = "1" ]; then
  umask 077; printf '%s\n' "$yaml" > "./${USER}.creds"
  echo "(also written to ./${USER}.creds — chmod 600; delete after copying onto the node)"
fi
log "done."
