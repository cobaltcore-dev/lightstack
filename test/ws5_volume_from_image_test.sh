#!/usr/bin/env bash
# SPEC-009 WS-5 (T038 / FR-021): a volume created from an image is bootable and
# reaches `available` synchronously (real qcow2 backing via qemu-img).
set -euo pipefail
: "${OS_AUTH_URL:?source your OpenStack env first}"
IMG="${LIGHTSTACK_TEST_IMAGE:-cirros}"
VOL=ws5-fromimg-vol

cleanup() { openstack volume delete "$VOL" 2>/dev/null || true; }
trap cleanup EXIT

openstack volume create --size 1 --image "$IMG" "$VOL" >/dev/null
status=$(openstack volume show "$VOL" -f value -c status)
bootable=$(openstack volume show "$VOL" -f value -c bootable)

[ "$status" = available ] || { echo "FAIL: status=$status (want available)"; exit 1; }
case "$bootable" in
  True | true) ;;
  *) echo "FAIL: bootable=$bootable (want True)"; exit 1 ;;
esac
echo "PASS: volume-from-image is bootable + available"
echo "[ws5-volume-from-image] OK"
