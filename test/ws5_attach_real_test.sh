#!/usr/bin/env bash
# SPEC-009 WS-5 (T038 / FR-021,FR-022): real-mode volume attach is synchronous,
# the guest sees a usable block device, and data persists across detach/re-attach.
# cirros has no mkfs.ext2, so persistence is checked at the raw block level.
set -euo pipefail
: "${OS_AUTH_URL:?source your OpenStack env first}"
IMG="${O3K_TEST_IMAGE:-cirros}"
NET="${O3K_TEST_NET:-net1}"
KEY="${O3K_TEST_KEY:-k1}"
SSHKEY="${O3K_SSH_KEY:-$HOME/.ssh/id_rsa}"
VM=ws5-attach-vm
VOL=ws5-attach-vol

cleanup() {
	openstack server delete "$VM" 2>/dev/null || true
	sleep 2
	openstack volume delete "$VOL" 2>/dev/null || true
}
trap cleanup EXIT

ssh_vm() { ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes -o ConnectTimeout=5 -i "$SSHKEY" "cirros@$1" "$2"; }

openstack server create --image "$IMG" --flavor m1.small --network "$NET" --key-name "$KEY" "$VM" --wait
IP=$(openstack server show "$VM" -f value -c addresses | grep -oE '([0-9]{1,3}\.){3}[0-9]{1,3}' | head -1)
[ -n "$IP" ] || { echo "FAIL: no IP for $VM"; exit 1; }

openstack volume create --size 1 "$VOL" >/dev/null
status=$(openstack volume show "$VOL" -f value -c status)
[ "$status" = available ] || { echo "FAIL: volume not available after create (got $status)"; exit 1; }

# synchronous attach: the volume must be in-use as soon as the call returns
openstack server add volume "$VM" "$VOL"
status=$(openstack volume show "$VOL" -f value -c status)
[ "$status" = in-use ] || { echo "FAIL: volume not in-use after attach (got $status)"; exit 1; }

for _ in $(seq 1 24); do ssh_vm "$IP" true 2>/dev/null && break; sleep 5; done
ssh_vm "$IP" 'lsblk | grep -q vdb' || { echo "FAIL: guest does not see /dev/vdb"; exit 1; }

ssh_vm "$IP" 'echo O3K-WS5-PERSIST | sudo dd of=/dev/vdb bs=512 count=1 conv=fsync 2>/dev/null; sync'
openstack server remove volume "$VM" "$VOL"
sleep 2
if ssh_vm "$IP" 'lsblk | grep -q vdb' 2>/dev/null; then echo "FAIL: /dev/vdb still present after detach"; exit 1; fi

openstack server add volume "$VM" "$VOL"
sleep 2
got=$(ssh_vm "$IP" 'sudo dd if=/dev/vdb bs=16 count=1 2>/dev/null')
case "$got" in
  O3K-WS5-PERSIST*) echo "PASS: /dev/vdb visible + data persisted across detach/re-attach" ;;
  *) echo "FAIL: persisted data mismatch (got '$got')"; exit 1 ;;
esac
echo "[ws5-attach] OK"
