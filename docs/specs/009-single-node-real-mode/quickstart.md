# Quickstart: Validating Single-Node Real Mode (v1)

End-to-end validation runbook for the acceptance criteria (SC-001…SC-010). Real-mode VMs cannot be exercised in a non-Linux sandbox, so this runs on the project's validation target: **one Ubuntu node with KVM + libvirt**. `docs/specs/009-single-node-real-mode/HOST-TESTING.md` is the detailed, battle-tested step list (§1–§15) including cleanup/reset; this file is the higher-level SC map.

> **What was validated (2026-06-22):** all steps below passed on Ubuntu + libvirt running the **binary directly** with a real-mode config (Option B). The Helm path (Option A) deploys the same binary with the same modes via the SPEC-008 chart.

## 0. Prerequisites

1. **Host**: Ubuntu with KVM + libvirtd running (`systemctl status libvirtd`, `kvm-ok`). Note the uplink: `ip route get 1.1.1.1`.
2. **Image tools**: the Lightstack binary/image needs `qemu-img` + `genisoimage` + `dnsmasq` on PATH (the reference Dockerfile installs them).
3. **Database**: a reachable PostgreSQL (e.g. a `postgres` container on the host).
4. **Test image**: `cirros` (`cirros.img`, ~20 MB) — small, cloud-init-aware, DHCP + SSH. Note: cirros busybox has **no `mkfs`**, so volume persistence is checked at the raw block level.
5. **Floating IP**: on a single host the FIP is hosted as a `/32` on the uplink and DNAT'd to the VM (no MetalLB needed). A routable **pool** to a hosting node is v2 (#52).

## 1. Build

```bash
make build                                       # bin/lightstack
# (Helm only) container image with the real-mode tools:
docker build -t lightstack:dev -f build/package/Dockerfile .
docker save lightstack:dev -o /tmp/lightstack-dev.tar && sudo k3s ctr images import /tmp/lightstack-dev.tar
```

## 2. Deploy in real mode — pick one

**Option A — Helm (the chart deliverable).** `deploymentMode` defaults to `single-node`; real mode is gated by `realMode.enabled`:

```bash
helm install lightstack ./charts/lightstack \
  --set image.repository=lightstack --set image.tag=dev --set image.pullPolicy=Never \
  --set database.url='postgres://lightstack:<pw>@<host>:5432/lightstack?sslmode=disable' \
  --set secrets.jwtSecret='<strong-random>' --set secrets.adminPassword='<non-default>' \
  --set nova.libvirtMode=real \
  --set neutron.networkingMode=iptables --set neutron.securityGroupMode=iptables \
  --set cinder.storageMode=local --set glance.storageMode=local \
  --set realMode.enabled=true --set realMode.privileged=true --set realMode.hostNetwork=true \
  --set podSecurity.applyPrivilegedNamespaceLabel=true
```

**Option B — binary directly (the validated path).** Set `nova.libvirt_mode: real`, `neutron.networking_mode: iptables`, `cinder.storage_mode: local`, `glance.storage_mode: local` in your config, then:

```bash
sudo -E ./bin/lightstack --config config/lightstack.yaml --migrations migrations
```

Then, either way:

```bash
source ~/.lightstack-env            # OS_AUTH_URL etc.
openstack token issue
```

## 3. WS-0 — boot a real VM (SC-001, SC-005, SC-006)

```bash
openstack image create cirros --disk-format qcow2 --container-format bare --file cirros.img
openstack image show cirros -f value -c disk_format            # → raw   (normalized on ingest, R2)
[ -f ~/.ssh/id_rsa.pub ] || ssh-keygen -t rsa -N '' -f ~/.ssh/id_rsa
openstack keypair create --public-key ~/.ssh/id_rsa.pub k1
openstack network create net1
openstack subnet create sub1 --network net1 --subnet-range 10.0.0.0/24
openstack server create --image cirros --flavor m1.small --key-name k1 --network net1 vm1 --wait
openstack server show vm1 -f value -c status                   # → ACTIVE
# on the host (needs sudo):
sudo virsh domblklist instance-<vm1-id8>                       # disk = /var/lib/lightstack/instances/<id>/disk.qcow2
qemu-img info -U /var/lib/lightstack/instances/<vm1-id>/disk.qcow2    # backing = image-<id>.raw, format raw
openstack server create --image cirros --flavor m1.small --network net1 vm2 --wait   # separate overlay (isolation)
openstack image delete cirros                                  # → 409 in use (vm1/vm2 reference it)
openstack console log show vm1                                 # real serial boot output
```

## 4. WS-2 — survive a restart (SC-003, SC-008, SC-010)

```bash
openstack server show vm1 -f value -c addresses                # note the IP
# restart lightstack: Helm → kubectl rollout restart deploy/lightstack ; binary → Ctrl-C + re-run
openstack server show vm1 -f value -c addresses                # SAME IP (reservation rebuilt from DB)
ssh -i ~/.ssh/id_rsa cirros@<ip> 'echo ok'                     # connectivity + disk intact
# no duplicates after a second restart:
sudo iptables -t nat -S | sort | uniq -d                       # empty
# self-heal: kill dnsmasq, wait one reconcile interval, lease/route restored
sudo pkill -f 'dnsmasq.*br-' ; sleep 35 ; ssh -i ~/.ssh/id_rsa cirros@<ip> 'echo ok'
```

## 5. WS-4 — cloud-init, login, user_data (SC-002, SC-007)

```bash
printf '#!/bin/sh\ntouch /tmp/userdata-ran\n' > /tmp/ud.sh
openstack server create --image cirros --flavor m1.small --key-name k1 \
  --user-data /tmp/ud.sh --network net1 vm3 --wait
ssh -i ~/.ssh/id_rsa cirros@<vm3-ip>                            # key login (cirros pulls the key via the EC2 metadata route)
ssh ... 'ls /tmp/userdata-ran'                                  # user_data applied
ssh ... 'curl -s http://169.254.169.254/openstack/latest/meta_data.json | head'   # own metadata
```

## 6. WS-3 — north-south (SC-004)

```bash
openstack network create ext-net --external ; openstack subnet create ext-sub --network ext-net --subnet-range 172.24.4.0/24 --no-dhcp
openstack router create r1 ; openstack router set r1 --external-gateway ext-net
openstack router add subnet r1 sub1
ssh -i ~/.ssh/id_rsa cirros@<vm1-ip> 'ping -c1 1.1.1.1'         # egress, masqueraded out the uplink
FIP=$(openstack floating ip create ext-net -f value -c floating_ip_address)
openstack server add floating ip vm1 $FIP
ssh -i ~/.ssh/id_rsa cirros@$FIP 'echo reachable'               # reaches the VM via the FIP's OUTPUT DNAT — host-local, so not SG-gated
openstack server remove floating ip vm1 $FIP                    # ingress stops
```
> The default security group seeds egress only — no ingress at all (cloud-storage#528), so a fresh VM is closed inbound. To prove SG gating, add/remove a rule and watch the per-VM `LS-NSI-<ip>` chain (HOST-TESTING.md §14). Host-local SSH to the *fixed* IP bypasses the FORWARD-based SG; the chain content is the enforcement for forwarded/external traffic.

## 7. WS-5 — block storage + delete semantics (SC-009, FR-023)

```bash
# attach: synchronous, guest sees the device, data persists (block level — cirros has no mkfs)
openstack volume create --size 1 v1                             # → available immediately
openstack server add volume vm1 v1                              # returns only after libvirt attached it (in-use)
ssh ... cirros@<vm1-ip> 'lsblk | grep vdb'
ssh ... 'echo PERSIST | sudo dd of=/dev/vdb bs=512 count=1 conv=fsync 2>/dev/null; sync'
openstack server remove volume vm1 v1 ; openstack server add volume vm1 v1
ssh ... 'sudo dd if=/dev/vdb bs=8 count=1 2>/dev/null'          # → PERSIST  (survived detach/re-attach)

# volume-from-image + boot-from-volume
openstack volume create --size 1 --image cirros bootvol         # bootable=True, available
openstack server create --volume bootvol --flavor m1.small --network net1 --key-name k1 vm4 --wait
ssh -i ~/.ssh/id_rsa cirros@<vm4-ip> 'echo booted'              # root disk IS the volume (no image overlay)

# delete semantics (FR-023)
openstack server delete vm4 ; openstack volume show bootvol -f value -c status   # → available (released, not deleted)
DID=$(openstack volume create --size 1 --image cirros dotvol -f value -c id)     # BDM uuid must be the ID, not the name
openstack server create --flavor m1.small --network net1 --key-name k1 \
  --block-device uuid=$DID,source_type=volume,boot_index=0,delete_on_termination=true vm5 --wait
openstack server delete vm5 ; openstack volume show dotvol      # → No Volume found (deleted with the server)
```

## 8. Terraform compatibility (SC-007)

The OpenStack provider drives the same API unmodified. `test/terraform/` ships `provider.tf.example` + `README.md`; a dedicated SPEC-009 fixture (`test/terraform/ws5/`) is a deferred task (T038). Minimal check:

```bash
cp test/terraform/provider.tf.example /tmp/lightstack.tf   # point at OS_AUTH_URL; add a resource block
cd /tmp && terraform init && terraform apply         # instance / keypair / network / volume
terraform destroy
```

## Pass criteria
Every step succeeds with the **unmodified** OpenStack CLI / gophercloud / Terraform OpenStack provider. Map each step back to its SC in `spec.md`. Real-mode steps (3–8) are host-gated and not run in CI; the stub-mode unit tests (`internal/nova`, `pkg/storage`, `pkg/hypervisor`) run in CI.
