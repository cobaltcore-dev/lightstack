#!/usr/bin/env bash
# SPEC-014: capability-C integration test — Swift API against the real RGW
# through Lightstack keystone tokens. Drives the docker-compose.rgw-test.yml
# stack (Lightstack with keystone.rgw_compat_tenant_ids=true + ceph/demo RGW
# wired to Lightstack Keystone for BOTH S3 and Swift) and proves the PR's core
# claim end to end against real Ceph RGW, not an emulation:
#   - a password-issued scoped token authenticates Swift requests (RGW
#     validates it via GET /v3/auth/tokens — the TokenEnvelope this PR builds)
#   - the token's project.id (underscored by the flag) is the Swift account:
#     /swift/v1/AUTH_<project id> — a valid RGW tenant name
#   - container + object round-trip byte-identically under that account
#   - an unscoped token is denied (the envelope carries no project — RGW's
#     decoder rejects it; fail closed)
#   - a garbage token is denied
#   - another project's token cannot read the container (tenant isolation)
#   - radosgw-admin shows the implicit tenant named by the underscored id
#
#   podman compose -p lightstack-rgwtest -f deployments/docker-compose.rgw-test.yml up -d --build
#   ./test/swift_rgw_test.sh
#
# Identity ops go straight to Keystone (curl) to avoid the service catalog,
# which advertises in-container ports. Swift ops are raw curl — exactly the
# HTTP Ceph RGW serves.
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
jget() { python3 -c "import sys,json;print(json.load(sys.stdin)$1)"; }

# --- wait for Lightstack + rgw ---
log "waiting for Lightstack keystone ($OS_AUTH_URL) + rgw ($RGW_URL)"
for _ in $(seq 1 80); do curl -sf "$OS_AUTH_URL" >/dev/null 2>&1 && break; sleep 3; done
curl -sf "$OS_AUTH_URL" >/dev/null 2>&1 || die "Lightstack not ready"
for _ in $(seq 1 80); do curl -s "$RGW_URL" >/dev/null 2>&1 && break; sleep 3; done
curl -s "$RGW_URL" >/dev/null 2>&1 || die "rgw not ready"

# --- admin token, scoped to the default project ---
tokhdr="$(mktemp)"; tokbody="$(mktemp)"
curl -s -D "$tokhdr" -o "$tokbody" -X POST "$OS_AUTH_URL/auth/tokens" -H 'Content-Type: application/json' -d @- <<JSON
{"auth":{"identity":{"methods":["password"],"password":{"user":{"name":"$ADMIN_USER","domain":{"name":"$ADMIN_DOMAIN"},"password":"$ADMIN_PASS"}}},"scope":{"project":{"name":"$ADMIN_PROJECT","domain":{"name":"$ADMIN_DOMAIN"}}}}}
JSON
TOKEN="$(grep -i '^x-subject-token' "$tokhdr" | tr -d '\r' | awk '{print $2}')"
[ -n "$TOKEN" ] || die "admin auth failed"
PID="$(jget '["token"]["project"]["id"]' < "$tokbody")"
DOMAIN_ID="$(jget '["token"]["project"].get("domain",{}).get("id","default")' < "$tokbody")"
[ -n "$PID" ] || die "no project id in the token envelope"
echo "scoped token ok; advertised project id = $PID"
# The fixture must run with the flag on: the advertised id is underscored
# (RGW tenant names reject dashes) — that is the form under test.
case "$PID" in
  *_*) echo "project id is underscored (keystone.rgw_compat_tenant_ids on)" ;;
  *)   die "project id '$PID' is not underscored — LIGHTSTACK_KEYSTONE_RGW_COMPAT_TENANT_IDS must be true in the fixture" ;;
esac

# Unscoped token (same credentials, no scope) for the negative test.
unshdr="$(mktemp)"
curl -s -D "$unshdr" -o /dev/null -X POST "$OS_AUTH_URL/auth/tokens" -H 'Content-Type: application/json' -d @- <<JSON
{"auth":{"identity":{"methods":["password"],"password":{"user":{"name":"$ADMIN_USER","domain":{"name":"$ADMIN_DOMAIN"},"password":"$ADMIN_PASS"}}}}}
JSON
UNSCOPED="$(grep -i '^x-subject-token' "$unshdr" | tr -d '\r' | awk '{print $2}')"
[ -n "$UNSCOPED" ] || die "unscoped auth failed"

kAPI() { curl -s -H "X-Auth-Token: $TOKEN" "$@"; }

# --- cleanup state ---
PROJ_C=""; USER_C=""
cleanup() {
  set +e
  # Best effort: empty the container via Swift before removing the project.
  curl -s -X DELETE -H "X-Auth-Token: $TOKEN" \
    "$RGW_URL/swift/v1/AUTH_$PID/swift-c-$TS/obj-$TS.txt" >/dev/null 2>&1
  curl -s -X DELETE -H "X-Auth-Token: $TOKEN" \
    "$RGW_URL/swift/v1/AUTH_$PID/swift-c-$TS" >/dev/null 2>&1
  [ -n "$USER_C" ] && kAPI -X DELETE "$OS_AUTH_URL/users/$USER_C" -o /dev/null
  [ -n "$PROJ_C" ] && kAPI -X DELETE "$OS_AUTH_URL/projects/$PROJ_C" -o /dev/null
  rm -f "$tokhdr" "$tokbody" "$unshdr"
}
trap cleanup EXIT

ACCOUNT="$RGW_URL/swift/v1/AUTH_$PID"
CONTAINER="swift-c-$TS"; OBJECT="obj-$TS.txt"; DATA="lightstack-swift-$TS"
sw() { # sw <method> <token> <path> [data] -> status code
  local method="$1" tok="$2" path="$3" data="${4:-}"
  if [ -n "$data" ]; then
    curl -s -o /dev/null -w '%{http_code}' -X "$method" -H "X-Auth-Token: $tok" -d "$data" "$ACCOUNT/$path"
  else
    curl -s -o /dev/null -w '%{http_code}' -X "$method" -H "X-Auth-Token: $tok" "$ACCOUNT/$path"
  fi
}

# --- Test 1: container + object round-trip (the PR's core claim) ---
log "T1: scoped keystone token -> Swift container + object round-trip"
c_code="$(sw PUT "$TOKEN" "$CONTAINER")"
o_code="$(sw PUT "$TOKEN" "$CONTAINER/$OBJECT" "$DATA")"
got="$(curl -s -H "X-Auth-Token: $TOKEN" "$ACCOUNT/$CONTAINER/$OBJECT")"
if [ "$c_code" = "201" ] || [ "$c_code" = "204" ]; then ok "container created ($c_code)"; else bad "container PUT -> $c_code"; fi
if [ "$o_code" = "201" ]; then ok "object PUT -> 201"; else bad "object PUT -> $o_code"; fi
if [ "$got" = "$DATA" ]; then ok "object GET round-trips byte-identically"; else bad "object GET -> '$got' (want '$DATA')"; fi
listing="$(curl -s -H "X-Auth-Token: $TOKEN" "$ACCOUNT/$CONTAINER")"
case "$listing" in *"$OBJECT"*) ok "container listing shows the object" ;; *) bad "container listing '$listing' lacks $OBJECT" ;; esac

# --- Test 2: unscoped token is denied (envelope has no project) ---
log "T2: unscoped token -> denied (RGW's decoder requires a project)"
u_code="$(sw GET "$UNSCOPED" "$CONTAINER")"
if [ "$u_code" = "401" ]; then ok "unscoped token denied (401)"; else bad "unscoped token -> $u_code (want 401)"; fi

# --- Test 3: garbage token is denied ---
log "T3: garbage token -> denied"
g_code="$(sw GET "not-a-token" "$CONTAINER")"
if [ "$g_code" = "401" ]; then ok "garbage token denied (401)"; else bad "garbage token -> $g_code (want 401)"; fi

# --- Test 4: another project's token cannot read the container ---
log "T4: cross-project token -> cannot read tenant A's container"
PROJ_C="$(kAPI -X POST "$OS_AUTH_URL/projects" -H 'Content-Type: application/json' \
  -d "{\"project\":{\"name\":\"swift-proj-c-$TS\",\"domain_id\":\"$DOMAIN_ID\"}}" | jget '["project"]["id"]')"
USER_C="$(kAPI -X POST "$OS_AUTH_URL/users" -H 'Content-Type: application/json' \
  -d "{\"user\":{\"name\":\"swift-user-c-$TS\",\"password\":\"secret\",\"domain_id\":\"$DOMAIN_ID\",\"default_project_id\":\"$PROJ_C\"}}" | jget '["user"]["id"]')"
if [ -z "$PROJ_C" ] || [ -z "$USER_C" ]; then die "failed to create project/user C"; fi
MEMBER_RID="$(kAPI "$OS_AUTH_URL/roles" | python3 -c 'import sys,json;print(next(r["id"] for r in json.load(sys.stdin)["roles"] if r["name"]=="member"))')"
kAPI -X PUT "$OS_AUTH_URL/projects/$PROJ_C/users/$USER_C/roles/$MEMBER_RID" -o /dev/null
chdr="$(mktemp)"; cbody="$(mktemp)"
curl -s -D "$chdr" -o "$cbody" -X POST "$OS_AUTH_URL/auth/tokens" -H 'Content-Type: application/json' -d @- <<JSON
{"auth":{"identity":{"methods":["password"],"password":{"user":{"name":"swift-user-c-$TS","domain":{"name":"$ADMIN_DOMAIN"},"password":"secret"}}},"scope":{"project":{"id":"$PROJ_C"}}}}
JSON
TOKEN_C="$(grep -i '^x-subject-token' "$chdr" | tr -d '\r' | awk '{print $2}')"
rm -f "$chdr" "$cbody"
[ -n "$TOKEN_C" ] || die "project C auth failed"
x_code="$(curl -s -o /dev/null -w '%{http_code}' -H "X-Auth-Token: $TOKEN_C" "$ACCOUNT/$CONTAINER")"
if [ "$x_code" = "403" ] || [ "$x_code" = "404" ]; then ok "cross-project read denied ($x_code)"; else bad "cross-project read -> $x_code (want 403/404)"; fi

# --- Test 5: radosgw-admin shows the implicit tenant named by the underscored id ---
log "T5: radosgw-admin shows the implicit tenant for the underscored id"
admin_check() {
  local rt cid out
  for rt in docker podman; do
    command -v "$rt" >/dev/null 2>&1 || continue
    cid="$("$rt" ps --format '{{.Names}}' 2>/dev/null | grep -m1 -E '[-_]rgw[-_][0-9]+$')" || true
    [ -n "$cid" ] || continue
    # A found RGW container makes this a REAL check: an exec failure or an
    # empty listing must fail the test, not silently skip it (an unnoticed
    # skip would let the tenant-placement assertion pass without running).
    if ! out="$("$rt" exec "$cid" radosgw-admin bucket list 2>&1)"; then
      echo "radosgw-admin failed in container $cid: $out" >&2
      return 1
    fi
    if printf '%s' "$out" | grep -qF "\"$PID/$CONTAINER\""; then
      return 0
    fi
    echo "radosgw-admin bucket list does not carry '$PID/$CONTAINER': $out" >&2
    return 1
  done
  echo "no container runtime / rgw container found; skipping the radosgw-admin check" >&2
  return 2
}
if admin_check; then
  ok "radosgw-admin lists the bucket under the implicit tenant '$PID'"
else
  rc=$?
  if [ "$rc" = "2" ]; then ok "radosgw-admin check skipped (no container runtime)"; else bad "implicit tenant not visible in radosgw-admin (rc=$rc)"; fi
fi

log "results: $pass passed, $fail failed"
[ "$fail" = "0" ] || exit 1
