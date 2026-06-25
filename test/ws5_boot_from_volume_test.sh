#!/usr/bin/env bash
# SPEC-009 WS-5 (T038 / FR-023): boot-from-volume boots off the volume's qcow2,
# and server delete honors the standard delete semantics — a plain boot volume is
# released (-> available), a delete_on_termination volume is deleted.
set -euo pipefail
: "${OS_AUTH_URL:?source your OpenStack env first}"
IMG="${O3K_TEST_IMAGE:-cirros}"
NET="${O3K_TEST_NET:-net1}"
KEY="${O3K_TEST_KEY:-k1}"
SSHKEY="${O3K_SSH_KEY:-$HOME/.ssh/id_rsa}"
BVOL=ws5-bfv-vol
VM=ws5-bfv-vm
DOTVOL=ws5-dot-vol
DOTVM=ws5-dot-vm

cleanup() {
	openstack server delete "$VM" "$DOTVM" 2>/dev/null || true
	sleep 2
	openstack volume delete "$BVOL" "$DOTVOL" 2>/dev/null || true
}
trap cleanup EXIT

ssh_vm() { ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o BatchMode=yes -o ConnectTimeout=5 -i "$SSHKEY" "cirros@$1" "$2"; }

# --- boot-from-volume (existing volume; delete_on_termination defaults false) ---
openstack volume create --size 1 --image "$IMG" "$BVOL" >/dev/null
openstack server create --flavor m1.small --volume "$BVOL" --network "$NET" --key-name "$KEY" "$VM" --wait
status=$(openstack server show "$VM" -f value -c status)
[ "$status" = ACTIVE ] || { echo "FAIL: $VM status=$status (want ACTIVE)"; exit 1; }
IP=$(openstack server show "$VM" -f value -c addresses | grep -oE '([0-9]{1,3}\.){3}[0-9]{1,3}' | head -1)
for _ in $(seq 1 24); do ssh_vm "$IP" true 2>/dev/null && break; sleep 5; done
ssh_vm "$IP" true || { echo "FAIL: cannot SSH into boot-from-volume VM"; exit 1; }
echo "PASS: booted from volume + SSH in"

# --- release on delete (flag false): the volume returns to available ---
openstack server delete "$VM"
sleep 2
status=$(openstack volume show "$BVOL" -f value -c status)
[ "$status" = available ] || { echo "FAIL: $BVOL not released on delete (status=$status)"; exit 1; }
echo "PASS: boot volume released to available on server delete"

# --- delete-on-termination: the volume is deleted on server delete ---
openstack volume create --size 1 --image "$IMG" "$DOTVOL" >/dev/null
DID=$(openstack volume show "$DOTVOL" -f value -c id)   # BDM uuid must be the volume ID, not its name
openstack server create --flavor m1.small --network "$NET" --key-name "$KEY" \
	--block-device "uuid=$DID,source_type=volume,destination_type=volume,boot_index=0,delete_on_termination=true" \
	"$DOTVM" --wait
openstack server delete "$DOTVM"
sleep 2
if openstack volume show "$DOTVOL" >/dev/null 2>&1; then
	echo "FAIL: delete_on_termination volume still exists after server delete"; exit 1
fi
echo "PASS: delete_on_termination volume deleted on server delete"
echo "[ws5-boot-from-volume] OK"
