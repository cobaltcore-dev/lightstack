#!/usr/bin/env bash
# SPEC-009 WS-0 (T008 / FR-001b): a real-mode VM exposes a serial console log.
set -euo pipefail
: "${OS_AUTH_URL:?source your OpenStack env first}"
IMG="${LIGHTSTACK_TEST_IMAGE:-cirros}"
NET="${LIGHTSTACK_TEST_NET:-net1}"

cleanup() { openstack server delete ws0-console-vm 2>/dev/null || true; }
trap cleanup EXIT

openstack server create --image "$IMG" --flavor m1.tiny --network "$NET" ws0-console-vm --wait
sleep 15 # give the guest time to emit boot output to the serial console
out=$(openstack console log show ws0-console-vm 2>&1 || true)
if [ -z "$out" ]; then
  echo "FAIL: console log is empty"; exit 1
fi
# Require recognizable boot output, not just any non-empty string — a stub could
# return junk and still look non-empty. Kernel timestamps ([ 0.000000]) appear on
# every real Linux boot; cirros/login also confirm the guest actually started.
if ! printf '%s' "$out" | grep -qiE 'login:|cirros|Linux version|Booting|SeaBIOS|\[[[:space:]]*[0-9]+\.[0-9]+\]'; then
  echo "FAIL: console log has no recognizable boot output:"; printf '%s\n' "$out" | head -5; exit 1
fi
echo "PASS: console log shows real boot output ($(printf '%s\n' "$out" | wc -l) lines)"
echo "[ws0-console] OK"
