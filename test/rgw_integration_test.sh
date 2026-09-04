#!/usr/bin/env bash
# SPEC-014 T004/T008: Glance-on-Ceph-RGW integration test.
# Drives the docker-compose.rgw-test.yml stack (Lightstack + ceph/demo real RGW):
#   - T004: image upload -> object lands in the RGW bucket -> download is byte-identical
#   - T008: a large (streamed) image round-trips without a total-op timeout (FR-014)
#
#   podman compose -p lightstack-rgwtest -f deployments/docker-compose.rgw-test.yml up -d --build
#   ./test/rgw_integration_test.sh
set -euo pipefail

OS_AUTH_URL="${OS_AUTH_URL:-http://localhost:45357/v3}"
RGW_URL="${RGW_URL:-http://localhost:48080}"
RGW_BUCKET="${RGW_BUCKET:-images}"
export OS_AUTH_URL
export OS_USERNAME=admin OS_PASSWORD="${OS_PASSWORD:-secret}" OS_PROJECT_NAME=default
export OS_USER_DOMAIN_NAME=Default OS_PROJECT_DOMAIN_NAME=Default OS_IDENTITY_API_VERSION=3
# The Lightstack service catalog advertises glance at the in-container port (9292). This
# stack remaps it to a non-colliding host port, so override the image endpoint to
# the host mapping (keystoneauth honors OS_IMAGE_ENDPOINT_OVERRIDE) — otherwise the
# CLI would follow the catalog to :9292 and hit an unrelated Lightstack on the host.
export OS_IMAGE_ENDPOINT_OVERRIDE="${OS_IMAGE_ENDPOINT_OVERRIDE:-http://localhost:49292}"
export AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID:-lightstacktestaccess}"
export AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY:-lightstacktestsecret}"
export AWS_DEFAULT_REGION=us-east-1

log() { printf '\n== %s\n' "$*"; }
die() { echo "FAIL: $*" >&2; exit 1; }

# Assert the object landed in the RGW bucket under Lightstack's key convention
# (images/image-<id>.raw). Primary check runs radosgw-admin inside the fixture's
# rgw container — reliable and identical under docker-compose (CI) and podman.
# Falls back to an S3 client where container exec isn't available.
assert_object_in_bucket() {
  local key="images/image-$1.raw" rt cid
  for rt in docker podman; do
    command -v "$rt" >/dev/null 2>&1 || continue
    # Match the 'rgw' compose service token (…_rgw_1 / …-rgw-1), not a substring —
    # the project name (lightstack-rgwtest) itself contains "rgw".
    cid="$("$rt" ps --format '{{.Names}}' 2>/dev/null | grep -m1 -E '[-_]rgw[-_][0-9]+$')" || true
    [ -n "$cid" ] || continue
    "$rt" exec "$cid" radosgw-admin bucket list --bucket="$RGW_BUCKET" 2>/dev/null \
      | grep -qF "\"$key\"" && return 0
  done
  # Fallback: S3 client (only where the host aws CLI is healthy).
  command -v aws >/dev/null 2>&1 &&
    AWS_S3_ADDRESSING_STYLE=path aws --endpoint-url "$RGW_URL" \
      s3api head-object --bucket "$RGW_BUCKET" --key "$key" >/dev/null 2>&1
}

# --- wait for Lightstack + rgw ---
log "waiting for Lightstack keystone ($OS_AUTH_URL) + rgw ($RGW_URL)"
for _ in $(seq 1 80); do curl -sf "$OS_AUTH_URL" >/dev/null 2>&1 && break; sleep 3; done
curl -sf "$OS_AUTH_URL" >/dev/null 2>&1 || die "Lightstack not ready at $OS_AUTH_URL"
for _ in $(seq 1 80); do curl -s "$RGW_URL" >/dev/null 2>&1 && break; sleep 3; done
curl -s "$RGW_URL" >/dev/null 2>&1 || die "rgw not ready at $RGW_URL"
openstack token issue >/dev/null || die "auth failed (check OS_PASSWORD)"
echo "Lightstack + rgw ready"

work="$(mktemp -d)"
trap 'rm -rf "$work"; openstack image delete rgw-rt-small rgw-rt-large 2>/dev/null || true' EXIT

# --- T004: small image round-trip through RGW ---
log "T004: image upload -> RGW -> download (byte-identical)"
head -c 1048576 /dev/urandom > "$work/small.raw" # 1 MiB
openstack image create rgw-rt-small --disk-format raw --container-format bare --file "$work/small.raw" >/dev/null
IMG=$(openstack image show rgw-rt-small -f value -c id)
assert_object_in_bucket "$IMG" \
  || die "image object images/image-${IMG}.raw not found in the RGW bucket"
openstack image save rgw-rt-small --file "$work/small.out"
cmp "$work/small.raw" "$work/small.out" || die "round-trip content mismatch"
echo "T004 OK — object stored in RGW and round-trip is byte-identical"

# --- T008: large streamed image (proves no total-op timeout; FR-014) ---
SIZE="${LIGHTSTACK_TEST_IMAGE_SIZE:-1G}"
log "T008: large image ($SIZE, sparse) streamed round-trip"
truncate -s "$SIZE" "$work/large.raw"
md5in=$(md5sum "$work/large.raw" | awk '{print $1}')
openstack image create rgw-rt-large --disk-format raw --container-format bare --file "$work/large.raw" >/dev/null
openstack image save rgw-rt-large --file "$work/large.out"
md5out=$(md5sum "$work/large.out" | awk '{print $1}')
[ "$md5in" = "$md5out" ] || die "large image round-trip md5 mismatch ($md5in != $md5out)"
echo "T008 OK — $SIZE image streamed end-to-end, md5 match"

echo
echo "RGW GLANCE INTEGRATION: PASS"
