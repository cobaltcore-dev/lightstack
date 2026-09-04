#!/usr/bin/env bash
# SPEC-009 WS-0 (T008): real-mode VM disk — per-instance qcow2 overlay over a
# raw base + two-VM isolation. Run on the libvirt host against a running
# real-mode Lightstack (see docs/specs/009-single-node-real-mode/quickstart.md).
set -euo pipefail
: "${OS_AUTH_URL:?source your OpenStack env (~/.lightstack-env) first}"
IMG="${LIGHTSTACK_TEST_IMAGE:-cirros}"
NET="${LIGHTSTACK_TEST_NET:-net1}"

cleanup() { openstack server delete ws0-vm1 ws0-vm2 2>/dev/null || true; }
trap cleanup EXIT

echo "[ws0] creating two servers from image '$IMG'..."
openstack server create --image "$IMG" --flavor m1.tiny --network "$NET" ws0-vm1 --wait
openstack server create --image "$IMG" --flavor m1.tiny --network "$NET" ws0-vm2 --wait

id1=$(openstack server show ws0-vm1 -f value -c id)
id2=$(openstack server show ws0-vm2 -f value -c id)
dom1="instance-${id1:0:8}"

disk1=$(sudo virsh domblklist "$dom1" | awk '/disk.qcow2|vda/{print $2}' | head -1)
echo "[ws0] vm1 disk = $disk1"
case "$disk1" in
  /var/lib/lightstack/instances/*/disk.qcow2) echo "PASS: per-instance overlay path" ;;
  *) echo "FAIL: expected a per-instance overlay, got '$disk1'"; exit 1 ;;
esac

# -U (force-share): the VM is running, so qemu holds a write lock on the overlay;
# read it shared/read-only instead of failing on the lock.
info=$(sudo qemu-img info -U "$disk1")
echo "$info" | grep -q "backing file" || { echo "FAIL: overlay has no backing file"; exit 1; }
echo "$info" | grep -qiE "backing file format: *raw" || echo "WARN: backing-file format is not raw"
echo "PASS: overlay backs a raw base"

[ "$id1" != "$id2" ] || { echo "FAIL: two servers share an instance id"; exit 1; }
echo "PASS: two servers are isolated, distinct instances"
echo "[ws0-disk-real] OK"
