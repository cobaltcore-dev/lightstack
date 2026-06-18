# Quickstart: Validating Single-Node Real Mode (v1)

End-to-end validation runbook for the acceptance criteria (SC-001…SC-010). Real-mode VMs cannot be exercised in a non-Linux sandbox, so this runs on the project's validation target: **one Ubuntu node with KVM + libvirt, a single-node k3s cluster, Helm**.

## 0. Prerequisites (the four logistics)

1. **Host**: Ubuntu with KVM + libvirtd running (`systemctl status libvirtd`; `kvm-ok` passes). The o3k pod connects to the host libvirt socket and uses the host's primary uplink.
2. **Cluster**: single-node k3s, `helm` installed. Real mode needs the pod privileged + hostNetwork + NET_ADMIN + the host libvirt socket mounted (the SPEC-008 chart's real-mode path).
3. **Database**: a reachable PostgreSQL (external, per the chart) — e.g. a `postgres` container on the host, connection string supplied to the chart.
4. **Test image**: `cirros` qcow2 (~20 MB) — small, cloud-init-aware, DHCP + SSH.
5. **Floating-IP pool**: a spare IP (or small range) on the node's L2, delivered via MetalLB or a routed/L2 subnet. Note the uplink iface: `ip route get 1.1.1.1`.

## 1. Build the image (with the new tools) and import into k3s

The reference image must now include `qemu-img` + `genisoimage` (and `dnsmasq`, already present).

```bash
make build                        # or the repo's image build
docker build -t o3k:dev -f build/package/Dockerfile .
docker save o3k:dev -o /tmp/o3k-dev.tar
sudo k3s ctr images import /tmp/o3k-dev.tar      # containerd, not docker
```

## 2. Deploy in real mode

```bash
helm install o3k charts/o3k \
  --set image.repository=o3k --set image.tag=dev --set image.pullPolicy=Never \
  --set mode.node=single \
  --set nova.libvirtMode=real \
  --set neutron.networkingMode=iptables \
  --set cinder.storageMode=local --set glance.storageMode=local \
  --set externalDatabase.url=<postgres-dsn> \
  --set secrets.jwt=<...> --set secrets.adminPassword=<...> \
  --set realMode.privileged=true            # mounts host libvirt socket, hostNetwork, NET_ADMIN
# wait for /readyz, then:
source ~/.o3k-env                            # OS_AUTH_URL etc.
openstack token issue
```

## 3. WS-0 — boot a real VM (SC-001, SC-005, SC-006)

```bash
openstack image create cirros --disk-format qcow2 --container-format bare --file cirros.qcow2
openstack image show cirros -f value -c disk_format            # → raw   (normalized on ingest, R2)
openstack keypair create --public-key ~/.ssh/id_rsa.pub k1
openstack server create --image cirros --flavor m1.tiny --key-name k1 --network net1 vm1
openstack server show vm1 -f value -c status                   # → ACTIVE
# on the host:
sudo virsh list                                                # domain running
sudo virsh domblklist <domain>                                 # disk = /var/lib/o3k/instances/<id>/disk.qcow2
qemu-img info /var/lib/o3k/instances/<id>/disk.qcow2           # backing file = image-<id>.raw, format raw
openstack server create --image cirros --flavor m1.tiny --network net1 vm2   # isolation: separate overlay
openstack image delete cirros                                  # → 409 in use (vm1/vm2 reference it)
openstack console log show vm1                                 # boot output
```

## 4. WS-2 — survive a control-plane restart (SC-003, SC-008, SC-010)

```bash
IP=$(openstack server show vm1 -f value -c addresses)
kubectl rollout restart deploy/o3k                             # pod roll
# after ready:
openstack server show vm1 -f value -c addresses                # same IP
ssh -i ~/.ssh/id_rsa cirros@$IP 'echo ok'                       # connectivity intact, disk intact
kubectl rollout restart deploy/o3k                             # twice → no dup netns/bridges/leases
sudo ip netns list; sudo iptables -t nat -S | sort | uniq -d   # no duplicates
# self-heal: kill dnsmasq, wait one reconcile interval, lease restored
sudo pkill -f "dnsmasq.*<net>"; sleep 35; ssh ... cirros@$IP 'echo ok'
```

## 5. WS-4 — cloud-init, login, user_data, isolation (SC-002, SC-007)

```bash
printf '#!/bin/sh\ntouch /tmp/userdata-ran\n' > ud.sh
openstack server create --image cirros --flavor m1.tiny --key-name k1 \
  --user-data ud.sh --network net1 vm3
ssh -i ~/.ssh/id_rsa cirros@<vm3-ip>                            # key login works (≤2 min after ACTIVE)
ssh ... 'ls /tmp/userdata-ran'                                  # user_data applied
ssh ... 'curl -s http://169.254.169.254/openstack/latest/meta_data.json | head'   # own metadata
# isolation: a 2nd tenant on an overlapping 10.0.0.0/24 sees only its own metadata
```

## 6. WS-3 — north-south (SC-004)

```bash
openstack router create r1; openstack router set r1 --external-gateway ext-net
openstack router add subnet r1 subnet1
ssh ... cirros@<vm-ip> 'ping -c1 1.1.1.1'                        # egress out the uplink
FIP=$(openstack floating ip create ext-net -f value -c floating_ip_address)
openstack server add floating ip vm1 $FIP
openstack security group rule create --proto tcp --dst-port 22 default
ssh -i ~/.ssh/id_rsa cirros@$FIP 'echo reachable'               # ingress via FIP (SG-gated)
openstack server remove floating ip vm1 $FIP                    # access stops
```

## 7. WS-5 — block storage (SC-009)

```bash
openstack volume create --size 1 v1
openstack server add volume vm1 v1                              # synchronous; returns after attach
ssh ... cirros@<ip> 'lsblk'                                     # new device visible
sudo virsh domblklist <domain>                                  # disk present
ssh ... 'sudo mkfs.ext4 /dev/vdb && sudo mount /dev/vdb /mnt && echo hi | sudo tee /mnt/x'
openstack server remove volume vm1 v1; openstack server add volume vm1 v1
ssh ... 'sudo mount /dev/vdb /mnt && cat /mnt/x'                # data persisted
# boot-from-volume:
openstack volume create --image cirros --size 2 --bootable bootvol
openstack server create --volume bootvol --flavor m1.tiny --network net1 vm4
sudo virsh domblklist <vm4-domain>                              # root = the volume
```

## 8. Terraform compatibility (SC-007)

```bash
cd test/terraform/ws-smoke
terraform init && terraform apply   # instance + key_pair + user_data + floating IP + volume
terraform destroy
```

## Pass criteria
All of the above succeed with the **unmodified** OpenStack CLI, gophercloud, and Terraform OpenStack provider. Map each step back to its SC in `spec.md`. Real-mode steps (3–8) are host-gated and not run in CI; the stub-mode unit tests and gophercloud contract tests run in CI.
