#!/bin/bash
# SPEC-014 T025: capability-B wireup for the ceph/demo RGW fixture.
#
# The stock ceph/demo entrypoint (/opt/ceph-container/bin/demo) bootstraps a
# single-node Ceph + RGW and creates the local demo user/bucket that Glance uses
# (capability A). This wrapper additionally points the RGW's S3 auth at Lightstack's
# Keystone so *tenant* S3 keys minted by Lightstack (openstack ec2 credentials) validate
# via /v3/s3tokens (capability B) — while the local demo user keeps working (RGW
# tries local users first, then Keystone).
#
# What the wireup requires, learned the hard way against real RGW (squid 19.2):
#   - rgw_s3_auth_use_keystone + rgw_keystone_url + api_version=3
#   - rgw_keystone_implicit_tenants=true   (per-project bucket/account namespace
#     for BOTH S3 and Swift — the Swift API validates X-Auth-Token through the
#     same keystone config and derives AUTH_<project id> accounts from it)
#   - rgw_swift_account_in_url   (Swift account URLs /swift/v1/AUTH_<tenant>)
#   - rgw_keystone_admin_*  — RGW must authenticate ITSELF to Keystone (fetch its
#     own token) before it can POST /v3/s3tokens or validate Swift tokens via
#     GET /v3/auth/tokens; without admin creds it fails with
#     "cannot get token for keystone access".
#   - rgw_dns_name empty   — force path-style so any client Host (localhost, rgw,
#     an IP) is treated as the endpoint, not a vhost bucket. Set on the radosgw
#     command line because it must override the demo's ceph.conf value.
#   - rgw_keystone_token_cache_size=0 — re-validate every request so a revoked
#     (deleted) credential is refused immediately (Lightstack is synchronous/fail-fast).
#   (Lightstack's /v3/s3tokens and /v3/auth/tokens return full Keystone tokens
#    with expires_at and an RGW-safe project.id — RGW tenant names reject dashes.)
#
# rgw_keystone_url is startup-only, so we let demo finish its bootstrap (it starts
# radosgw as a background daemon, then holds the container open with `exec ceph -w`),
# set the options in the mon config store, and restart radosgw in the foreground so
# it re-reads them and its log stays visible in `docker/podman logs`.
set -uo pipefail

KEYSTONE_URL="${LIGHTSTACK_KEYSTONE_URL:-http://lightstack:35357}"
KEYSTONE_ROLES="${LIGHTSTACK_KEYSTONE_ACCEPTED_ROLES:-admin,member,reader}"
ADMIN_USER="${LIGHTSTACK_KEYSTONE_ADMIN_USER:-admin}"
ADMIN_PASSWORD="${LIGHTSTACK_KEYSTONE_ADMIN_PASSWORD:-secret}"
ADMIN_PROJECT="${LIGHTSTACK_KEYSTONE_ADMIN_PROJECT:-default}"
ADMIN_DOMAIN="${LIGHTSTACK_KEYSTONE_ADMIN_DOMAIN:-Default}"
RGW_NAME="${RGW_NAME:-rgw}"
DEMO_UID="${CEPH_DEMO_UID:-lightstack}"
RGW_SECTION="client.rgw.${RGW_NAME}"
RGW_KEYRING="/var/lib/ceph/radosgw/ceph-rgw.${RGW_NAME}/keyring"

log() { echo "[rgw-keystone-wireup] $*"; }

# Run the stock demo bootstrap in the background; it ends in `exec ceph -w`, which
# keeps the container alive — we wait on it (and the RGW) at the end.
/opt/ceph-container/bin/demo &
demo_pid=$!

# Wait until radosgw is serving and demo has created its local user, so we don't
# restart mid-bootstrap and lose the local user/bucket (capability A) setup.
log "waiting for demo bootstrap (radosgw up + user '${DEMO_UID}')..."
for _ in $(seq 1 150); do
  if pgrep -x radosgw >/dev/null 2>&1 &&
     radosgw-admin user info --uid="$DEMO_UID" >/dev/null 2>&1; then
    break
  fi
  kill -0 "$demo_pid" 2>/dev/null || { log "demo exited during bootstrap"; wait "$demo_pid"; exit 1; }
  sleep 2
done

log "wiring RGW keystone auth (S3 + Swift) to Keystone at ${KEYSTONE_URL}"
ceph config set "$RGW_SECTION" rgw_s3_auth_use_keystone true
ceph config set "$RGW_SECTION" rgw_keystone_url "$KEYSTONE_URL"
ceph config set "$RGW_SECTION" rgw_keystone_api_version 3
# true (not s3): implicit tenants for BOTH protocols, so the Swift API
# (/swift/v1/AUTH_<project id>) and tenant S3 derive the same per-project
# tenant from the keystone token.
ceph config set "$RGW_SECTION" rgw_keystone_implicit_tenants true
ceph config set "$RGW_SECTION" rgw_keystone_accepted_roles "$KEYSTONE_ROLES"
ceph config set "$RGW_SECTION" rgw_keystone_verify_ssl false
ceph config set "$RGW_SECTION" rgw_keystone_admin_user "$ADMIN_USER"
ceph config set "$RGW_SECTION" rgw_keystone_admin_password "$ADMIN_PASSWORD"
ceph config set "$RGW_SECTION" rgw_keystone_admin_project "$ADMIN_PROJECT"
ceph config set "$RGW_SECTION" rgw_keystone_admin_domain "$ADMIN_DOMAIN"
ceph config set "$RGW_SECTION" rgw_keystone_token_cache_size 0
# Swift account URLs carry the tenant id (AUTH_<id>): keep them enabled so the
# Swift round-trip test can address the implicit tenant directly.
ceph config set "$RGW_SECTION" rgw_swift_account_in_url true

# Restart radosgw so it re-reads the (startup-only) keystone URL. Wait for the
# demo's radosgw to fully exit first so the new one can bind :8080 (avoids a
# two-daemon race), escalating to SIGKILL if it lingers.
log "restarting radosgw to apply keystone config"
pkill -x radosgw 2>/dev/null || true
for _ in $(seq 1 15); do pgrep -x radosgw >/dev/null 2>&1 || break; sleep 1; done
pgrep -x radosgw >/dev/null 2>&1 && { pkill -9 -x radosgw 2>/dev/null || true; sleep 2; }

# Foreground so radosgw's log reaches the container's stdout; empty rgw_dns_name
# forces path-style. Backgrounded in the shell so we can wait on both it and the
# demo keep-alive process.
radosgw --cluster ceph --setuser ceph --setgroup ceph \
  --default-log-to-stderr=true --err-to-stderr=true --default-log-to-file=false \
  --rgw-dns-name= --foreground \
  -n "$RGW_SECTION" -k "$RGW_KEYRING" &
rgw_pid=$!

log "wireup complete; RGW validates tenant S3 keys via ${KEYSTONE_URL}/v3/s3tokens and Swift tokens via ${KEYSTONE_URL}/v3/auth/tokens"
# Exit (and let the container restart) if either the RGW or the cluster dies.
wait -n "$demo_pid" "$rgw_pid"
