# Host testing guide — SPEC-009 single-node real mode (direct run, no k3s)

Clean-slate manual validation of the implemented phases on an **Ubuntu + libvirt**
host, running the `o3k` binary directly (real mode). No Kubernetes/Helm needed —
WS-0/WS-2 are product behavior. Covers **US1 / WS-0** (disk/boot/console) and
**US2 / WS-2** (network reconciler: stable IP across restart + dnsmasq self-heal).

> Run two terminals: **T1** = the `o3k` server, **T2** = the OpenStack CLI.

## 0. Prerequisites (one-time)
```bash
sudo apt install -y qemu-utils genisoimage dnsmasq iproute2 iptables
systemctl status libvirtd        # active
kvm-ok                            # KVM available
docker --version ; go version    # docker (dev Postgres) + Go 1.26+
# OpenStack client in T2: pipx install python-openstackclient  (or apt)
```

## 1. Clean recreate
```bash
sudo mkdir -p /var/lib/o3k/{images,volumes,instances,cloud-init}
# clear any leftovers from a previous run:
sudo pkill -f 'var/lib/o3k/dhcp' 2>/dev/null || true
for n in $(ip netns list 2>/dev/null | awk '{print $1}'); do :; done   # inspect; delete o3k netns if present
```

## 2. Build
```bash
cd /home/fnk/code/sap/ora/o3k/bugfix/503
make build                                    # -> bin/o3k
go build -o bin/o3k-migrate ./cmd/o3k-migrate
```

## 3. Postgres (dev)
```bash
make db-up                                    # postgres on :5432 (o3k/secret/o3k)
```

## 4. Real-mode config
`config/o3k-real.yaml` (copy of `config/o3k.yaml`) with:
```yaml
nova:    { libvirt_mode: real }
neutron: { networking_mode: iptables }        # real networking → bridges + dnsmasq
cinder:  { storage_mode: local }
glance:  { storage_mode: local }
```

## 5. OpenStack environment  ← (the `OS_AUTH_URL` error)
Save `~/.o3k-env` and **source it in every T2 shell**:
```bash
cat > ~/.o3k-env <<'EOF'
export OS_AUTH_URL=http://localhost:35357/v3
export OS_USERNAME=admin
export OS_PASSWORD=secret
export OS_PROJECT_NAME=default
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3
EOF
source ~/.o3k-env
```

## 6. Run O3K — terminal T1 (root: real networking needs netns/iptables + libvirt)
```bash
sudo -E ./bin/o3k --config config/o3k-real.yaml --migrations migrations
```
Leave it running. It auto-migrates and starts the services.

## 7. Build a topology — terminal T2
```bash
source ~/.o3k-env
openstack token issue >/dev/null && echo "auth OK"

# image (once)
[ -f cirros.img ] || wget -O cirros.img https://download.cirros-cloud.net/0.6.2/cirros-0.6.2-x86_64-disk.img
openstack image show cirros >/dev/null 2>&1 || \
  openstack image create cirros --disk-format qcow2 --container-format bare --file cirros.img

# network + DHCP subnet (enable_dhcp is on by default → triggers dnsmasq)
openstack network show net1 >/dev/null 2>&1 || openstack network create net1
openstack subnet show sub1 >/dev/null 2>&1 || \
  openstack subnet create sub1 --network net1 --subnet-range 10.0.0.0/24

# a VM (gives a port with a MAC->IP for the reconciler's reservation)
openstack server create --image cirros --flavor m1.small --network net1 vm1 --wait
openstack server show vm1 -c status -c addresses    # status ACTIVE; addresses net1=10.0.0.x
```

## 8. Verify DHCP is alive (the gate for the US2 tests)
The subnet's dnsmasq is started at subnet-create **and** by the reconciler on its
next tick (≤ ~30s), so wait a moment then check:
```bash
sleep 35
pgrep -fa dnsmasq | grep var/lib/o3k/dhcp && echo "DHCP up" || echo "NO o3k dnsmasq"
ls -l /var/lib/o3k/dhcp/configs/ /var/lib/o3k/dhcp/leases/ 2>/dev/null
ip netns list
```
- **If "DHCP up"** → proceed to §9 (US2 tests).
- **If "NO o3k dnsmasq"** → this is a real-networking finding (likely the dnsmasq
  namespace vs the bridge namespace). **Report back** so I can fix:
  ```bash
  # from T1's o3k log, the relevant lines:
  #   "failed to start DHCP server for subnet"  /  "reconcile: failed to (re)start DHCP"
  #   "reconcile: DHCP (re)started from DB state"
  ip netns list ; ip -br link show type bridge ; cat /var/lib/o3k/dhcp/configs/*.conf 2>/dev/null
  ```

## 9. US2 / WS-2 tests
```bash
# self-heal (automated): kill dnsmasq, reconciler respawns it within one interval
./test/ws2_selfheal_test.sh

# restart-survival (guided — o3k runs foreground):
./test/ws2_restart_survival_test.sh record vm1          # prints the addresses
#   …in T1: Ctrl-C o3k, then re-run the §6 command…
./test/ws2_restart_survival_test.sh verify vm1 '<addresses-from-record>'
```
Pass = dnsmasq respawns after a kill, and `vm1` keeps the **same** IP across the
o3k restart (the reconciler re-spawns dnsmasq with the MAC→IP reservation).

## 10. US1 / WS-0 recap (disk — already host-validated)
```bash
DOM=$(sudo virsh list --name | head -1)
sudo virsh domblklist "$DOM"                            # vda -> /var/lib/o3k/instances/<id>/disk.qcow2
sudo qemu-img info -U /var/lib/o3k/instances/*/disk.qcow2  # backing file = image-<id>.raw, format raw
openstack console log show vm1 | tail                   # real boot output
openstack image delete cirros                           # 409 while vm1 exists
```

## Expected NOT to work yet (later phases)
- The guest's `169.254.169.254` metadata fetch still fails and the NIC may show
  "no carrier" — that's **WS-4 (metadata)** and full L2/L3, not yet implemented.
  US2 only guarantees the **IP survives a control-plane restart**; it does not make
  metadata answer or give the VM internet. SSH-into-VM comes with WS-4.

## Troubleshooting
- **`OS_AUTH_URL: ... source your OpenStack env`** → `source ~/.o3k-env` in T2.
- **"NO o3k dnsmasq"** → see §8 (report the log + `ip netns list`).
- **libvirt can't read the disk** → `sudo chmod -R a+rX /var/lib/o3k/images /var/lib/o3k/instances`.
- **`/var/lib/o3k/images` missing at startup** → Glance falls back to `~/.o3k/images`
  and the overlay backing won't match; recreate the dir (§1) **before** starting o3k.
- **Reset everything** → `make db-down` (Postgres) + Ctrl-C o3k + `sudo rm -rf /var/lib/o3k/*`
  + `sudo pkill -f var/lib/o3k/dhcp`.
