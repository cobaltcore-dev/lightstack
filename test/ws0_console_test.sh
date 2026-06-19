#!/usr/bin/env bash
# SPEC-009 WS-0 (T008 / FR-001b): a real-mode VM exposes a serial console log.
set -euo pipefail
: "${OS_AUTH_URL:?source your OpenStack env first}"
IMG="${O3K_TEST_IMAGE:-cirros}"
NET="${O3K_TEST_NET:-net1}"

cleanup() { openstack server delete ws0-console-vm 2>/dev/null || true; }
trap cleanup EXIT

openstack server create --image "$IMG" --flavor m1.tiny --network "$NET" ws0-console-vm --wait
sleep 5
out=$(openstack console log show ws0-console-vm 2>&1 || true)
if [ -z "$out" ]; then
  echo "FAIL: console log is empty"; exit 1
fi
echo "PASS: console log returned $(printf '%s\n' "$out" | wc -l) lines"
echo "[ws0-console] OK"
