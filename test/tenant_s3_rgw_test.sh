#!/usr/bin/env bash
# SPEC-014 T020: tenant object-storage (capability B) integration test.
# Drives the docker-compose.rgw-test.yml stack (o3k + ceph/demo RGW wired to o3k
# Keystone) and proves an o3k-minted EC2 credential authenticates S3 requests
# against the real RGW via POST /v3/s3tokens, with fail-closed negatives:
#   - valid key         -> bucket create + object put/get round-trips (SC-004)
#   - tampered secret   -> denied (signature mismatch)              (SC-005)
#   - unknown key       -> denied
#   - revoked (deleted) -> denied on the next request               (SC-005)
#   - cross-project     -> a tenant cannot see another project's bucket (FR-007)
#
#   podman compose -p o3k-rgwtest -f deployments/docker-compose.rgw-test.yml up -d --build
#   ./test/tenant_s3_rgw_test.sh
#
# Identity ops go straight to Keystone (curl) to avoid the service catalog, which
# advertises in-container ports. S3 ops use boto3 (the library the aws CLI is built
# on) for a portable SigV4 client.
set -uo pipefail

OS_AUTH_URL="${OS_AUTH_URL:-http://localhost:45357/v3}"
RGW_URL="${RGW_URL:-http://localhost:48080}"
ADMIN_USER="${OS_USERNAME:-admin}"
ADMIN_PASS="${OS_PASSWORD:-secret}"
ADMIN_PROJECT="${OS_PROJECT_NAME:-default}"
ADMIN_DOMAIN="${OS_USER_DOMAIN_NAME:-Default}"
TS="$(date +%s)"

pass=0; fail=0
ok()   { echo "  PASS: $*"; pass=$((pass+1)); }
bad()  { echo "  FAIL: $*"; fail=$((fail+1)); }
log()  { printf '\n== %s\n' "$*"; }
die()  { echo "FATAL: $*" >&2; exit 1; }

command -v python3 >/dev/null || die "python3 required"
python3 -c 'import boto3' 2>/dev/null || die "boto3 required (pip install boto3)"

# --- wait for o3k + rgw ---
log "waiting for o3k keystone ($OS_AUTH_URL) + rgw ($RGW_URL)"
for _ in $(seq 1 80); do curl -sf "$OS_AUTH_URL" >/dev/null 2>&1 && break; sleep 3; done
curl -sf "$OS_AUTH_URL" >/dev/null 2>&1 || die "o3k not ready"
for _ in $(seq 1 80); do curl -s "$RGW_URL" >/dev/null 2>&1 && break; sleep 3; done
curl -s "$RGW_URL" >/dev/null 2>&1 || die "rgw not ready"

# --- admin token + identity ids ---
tokhdr="$(mktemp)"; tokbody="$(mktemp)"
curl -s -D "$tokhdr" -o "$tokbody" -X POST "$OS_AUTH_URL/auth/tokens" -H 'Content-Type: application/json' -d @- <<JSON
{"auth":{"identity":{"methods":["password"],"password":{"user":{"name":"$ADMIN_USER","domain":{"name":"$ADMIN_DOMAIN"},"password":"$ADMIN_PASS"}}},"scope":{"project":{"name":"$ADMIN_PROJECT","domain":{"name":"$ADMIN_DOMAIN"}}}}}
JSON
TOKEN="$(grep -i '^x-subject-token' "$tokhdr" | tr -d '\r' | awk '{print $2}')"
[ -n "$TOKEN" ] || die "admin auth failed"
ADMIN_UID="$(python3 -c 'import json;print(json.load(open("'"$tokbody"'"))["token"]["user"]["id"])')"
DEFAULT_PID="$(python3 -c 'import json;print(json.load(open("'"$tokbody"'"))["token"]["project"]["id"])')"
DOMAIN_ID="$(python3 -c 'import json;print(json.load(open("'"$tokbody"'"))["token"]["project"].get("domain",{}).get("id","default"))')"
echo "admin uid=$ADMIN_UID  default project=$DEFAULT_PID"

kAPI() { curl -s -H "X-Auth-Token: $TOKEN" "$@"; }        # authenticated keystone call
jget() { python3 -c "import sys,json;print(json.load(sys.stdin)$1)"; }

MEMBER_RID="$(kAPI "$OS_AUTH_URL/roles" | python3 -c 'import sys,json;print(next(r["id"] for r in json.load(sys.stdin)["roles"] if r["name"]=="member"))')"
[ -n "$MEMBER_RID" ] || die "member role not found"

# mint_ec2 <user_id> <tenant_id> -> "<access> <secret>"
mint_ec2() {
  kAPI -X POST "$OS_AUTH_URL/users/$1/credentials/OS-EC2" -H 'Content-Type: application/json' \
    -d "{\"tenant_id\":\"$2\"}" | python3 -c 'import sys,json;d=json.load(sys.stdin)["credential"];print(d["access"],d["secret"])'
}
del_ec2() { kAPI -X DELETE "$OS_AUTH_URL/users/$1/credentials/OS-EC2/$2" -o /dev/null -w '%{http_code}'; }

# s3op <access> <secret> <op> [bucket] [key] [body]
# ops: mb | put | get (prints body) | ls (prints names). Exit 0 = success; on
# failure prints "<Code> <HTTPStatus>" to stderr and exits 1. No client retries.
s3op() {
  S3_ACCESS="$1" S3_SECRET="$2" S3_OP="$3" S3_BUCKET="${4:-}" S3_KEY="${5:-}" S3_BODY="${6:-}" S3_ENDPOINT="$RGW_URL" \
  python3 - <<'PY'
import os, sys, boto3, botocore
from botocore.client import Config
c = boto3.client("s3", endpoint_url=os.environ["S3_ENDPOINT"],
    aws_access_key_id=os.environ["S3_ACCESS"], aws_secret_access_key=os.environ["S3_SECRET"],
    region_name="us-east-1",
    config=Config(s3={"addressing_style": "path"}, signature_version="s3v4", retries={"max_attempts": 1}))
op, b, k = os.environ["S3_OP"], os.environ.get("S3_BUCKET"), os.environ.get("S3_KEY")
try:
    if op == "mb":    c.create_bucket(Bucket=b)
    elif op == "put": c.put_object(Bucket=b, Key=k, Body=os.environ.get("S3_BODY", "").encode())
    elif op == "get": sys.stdout.write(c.get_object(Bucket=b, Key=k)["Body"].read().decode())
    elif op == "ls":  sys.stdout.write(",".join(x["Name"] for x in c.list_buckets()["Buckets"]))
    else: print("bad op", file=sys.stderr); sys.exit(2)
except botocore.exceptions.ClientError as e:
    print("%s %s" % (e.response["Error"].get("Code"), e.response["ResponseMetadata"].get("HTTPStatusCode")), file=sys.stderr)
    sys.exit(1)
PY
}

# --- cleanup ---
PROJ_B=""; USER_B=""; declare -a CREDS_A=() CREDS_B=()
cleanup() {
  set +e
  for a in "${CREDS_A[@]}"; do del_ec2 "$ADMIN_UID" "$a" >/dev/null 2>&1; done
  for a in "${CREDS_B[@]}"; do [ -n "$USER_B" ] && del_ec2 "$USER_B" "$a" >/dev/null 2>&1; done
  [ -n "$USER_B" ] && kAPI -X DELETE "$OS_AUTH_URL/users/$USER_B" -o /dev/null
  [ -n "$PROJ_B" ] && kAPI -X DELETE "$OS_AUTH_URL/projects/$PROJ_B" -o /dev/null
  rm -f "$tokhdr" "$tokbody"
}
trap cleanup EXIT

BUCKET_A="tenant-a-$TS"; BUCKET_B="tenant-b-$TS"; OBJ="secret.txt"; DATA="o3k-rgw-tenant-$TS"

# --- Test 1: valid key round-trips (SC-004) ---
log "T1: valid EC2 key -> bucket + object round-trip"
read -r ACC_A SEC_A < <(mint_ec2 "$ADMIN_UID" "$DEFAULT_PID"); CREDS_A+=("$ACC_A")
[ -n "$ACC_A" ] || die "mint (admin) failed"
if s3op "$ACC_A" "$SEC_A" mb "$BUCKET_A" 2>/dev/null &&
   s3op "$ACC_A" "$SEC_A" put "$BUCKET_A" "$OBJ" "$DATA" 2>/dev/null &&
   [ "$(s3op "$ACC_A" "$SEC_A" get "$BUCKET_A" "$OBJ" 2>/dev/null)" = "$DATA" ]; then
  ok "valid key created a bucket and round-tripped an object via RGW+/v3/s3tokens"
else
  bad "valid key round-trip failed"
fi

# --- Test 2: tampered secret is denied (SC-005) ---
log "T2: tampered secret -> denied"
if s3op "$ACC_A" "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef" ls 2>/dev/null; then
  bad "tampered secret was ACCEPTED"
else
  ok "tampered secret denied"
fi

# --- Test 3: unknown access key is denied ---
log "T3: unknown key -> denied"
if s3op "0000000000000000unknownkey000000" "0000000000000000unknownsecret000000000000" ls 2>/dev/null; then
  bad "unknown key was ACCEPTED"
else
  ok "unknown key denied"
fi

# --- Test 4: revoked (deleted) key is denied on the next request (SC-005) ---
log "T4: revoke (delete) key -> denied immediately"
read -r ACC_R SEC_R < <(mint_ec2 "$ADMIN_UID" "$DEFAULT_PID"); CREDS_A+=("$ACC_R")
s3op "$ACC_R" "$SEC_R" ls >/dev/null 2>&1 || die "freshly-minted key should work before revocation"
code="$(del_ec2 "$ADMIN_UID" "$ACC_R")"; [ "$code" = "204" ] || bad "delete returned $code (want 204)"
if s3op "$ACC_R" "$SEC_R" ls 2>/dev/null; then
  bad "revoked key still ACCEPTED (RGW token cache not disabled?)"
else
  ok "revoked key denied on next request"
fi

# --- Test 5: cross-project isolation (FR-007) ---
log "T5: cross-project isolation -> tenant B cannot see tenant A's bucket"
PROJ_B="$(kAPI -X POST "$OS_AUTH_URL/projects" -H 'Content-Type: application/json' \
  -d "{\"project\":{\"name\":\"proj-b-$TS\",\"domain_id\":\"$DOMAIN_ID\"}}" | jget '["project"]["id"]')"
USER_B="$(kAPI -X POST "$OS_AUTH_URL/users" -H 'Content-Type: application/json' \
  -d "{\"user\":{\"name\":\"user-b-$TS\",\"password\":\"secret\",\"domain_id\":\"$DOMAIN_ID\",\"default_project_id\":\"$PROJ_B\"}}" | jget '["user"]["id"]')"
[ -n "$PROJ_B" ] && [ -n "$USER_B" ] || die "failed to create project/user B"
kAPI -X PUT "$OS_AUTH_URL/projects/$PROJ_B/users/$USER_B/roles/$MEMBER_RID" -o /dev/null -w '%{http_code}' | grep -qE '20[0-9]' || bad "role assign B failed"
read -r ACC_B SEC_B < <(mint_ec2 "$USER_B" "$PROJ_B"); CREDS_B+=("$ACC_B")
[ -n "$ACC_B" ] || die "mint (B) failed"

# B works in its OWN namespace ...
if s3op "$ACC_B" "$SEC_B" mb "$BUCKET_B" 2>/dev/null; then
  ok "tenant B can create a bucket in its own namespace"
else
  bad "tenant B could not create its own bucket"
fi
# ... but cannot see tenant A's bucket (different implicit-tenant namespace).
if s3op "$ACC_B" "$SEC_B" get "$BUCKET_A" "$OBJ" 2>/dev/null; then
  bad "tenant B READ tenant A's object (isolation broken!)"
else
  ok "tenant B cannot read tenant A's object (isolated)"
fi

log "RESULTS: $pass passed, $fail failed"
[ "$fail" -eq 0 ] || exit 1
echo; echo "TENANT S3 (KEYSTONE) INTEGRATION: PASS"
