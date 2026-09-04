#!/usr/bin/env bash
# SPEC-014 T009 (FR-016 / SC-011): node-local image base cache eviction.
#   - the raw base materializes on first boot;
#   - it stays PINNED while ANY instance still references the image;
#   - it is RECLAIMED when the last referencing instance is deleted.
#
# Run ON the libvirt host against a running real-mode Lightstack (nova libvirt_mode:real,
# glance storage_mode:s3). The image must be RGW-backed. Source your OpenStack env:
#   OS_AUTH_URL=... ./test/cache_eviction_test.sh
set -euo pipefail
: "${OS_AUTH_URL:?source your OpenStack env first}"

IMG="${LIGHTSTACK_TEST_IMAGE:-cirros}"
NET="${LIGHTSTACK_TEST_NET:-net1}"
FLAVOR="${LIGHTSTACK_TEST_FLAVOR:-m1.tiny}"
BASE_DIR="${LIGHTSTACK_IMAGE_DIR:-/var/lib/lightstack/images}"

imgid=$(openstack image show "$IMG" -f value -c id)
base="$BASE_DIR/image-${imgid}.raw"

cleanup(){ openstack server delete t009-a t009-b 2>/dev/null || true; }
trap cleanup EXIT

present(){ sudo test -f "$base"; }   # exit 0 if the base file exists

# clean slate: no leftover instances referencing the image, no local base.
openstack server delete t009-a t009-b 2>/dev/null || true
sleep 2
sudo rm -f "$base"

echo "== boot A: base materializes from RGW"
openstack server create --image "$imgid" --flavor "$FLAVOR" --network "$NET" t009-a --wait >/dev/null
present || { echo "FAIL: base not created after first boot ($base)"; exit 1; }
echo "PASS: base present after first boot"

echo "== boot B (same image): base shared, still present"
openstack server create --image "$imgid" --flavor "$FLAVOR" --network "$NET" t009-b --wait >/dev/null
present || { echo "FAIL: base missing after booting B"; exit 1; }
echo "PASS: base present with two instances referencing it"

echo "== delete A: base PINNED (B still references the image)"
openstack server delete t009-a --wait
present || { echo "FAIL: base reclaimed while B still references it — should be pinned"; exit 1; }
echo "PASS: base pinned while another instance references the image"

echo "== delete B (last reference): base RECLAIMED"
openstack server delete t009-b --wait
if present; then echo "FAIL: base NOT reclaimed after the last reference was deleted ($base)"; exit 1; fi
echo "PASS: base reclaimed once no instance references the image"

echo
echo "[cache-eviction] OK — base pinned while referenced, reclaimed when unused"
