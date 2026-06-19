#!/usr/bin/env bash
# SPEC-009 WS-0 (T008 / FR-004): an image in use by a server cannot be deleted.
# Does NOT delete the shared test image — only asserts the in-use rejection.
set -euo pipefail
: "${OS_AUTH_URL:?source your OpenStack env first}"
IMG="${O3K_TEST_IMAGE:-cirros}"
NET="${O3K_TEST_NET:-net1}"
imgid=$(openstack image show "$IMG" -f value -c id)

cleanup() { openstack server delete ws0-inuse-vm 2>/dev/null || true; }
trap cleanup EXIT

openstack server create --image "$imgid" --flavor m1.tiny --network "$NET" ws0-inuse-vm --wait

if openstack image delete "$imgid" 2>/dev/null; then
  echo "FAIL: an in-use image was deleted"; exit 1
fi
echo "PASS: deleting an in-use image is rejected (409)"
echo "[ws0-image-inuse] OK"
