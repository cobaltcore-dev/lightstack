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
# clear leftovers from previous runs (o3k must be stopped first):
sudo pkill -f 'var/lib/o3k/dhcp' 2>/dev/null || true
sudo virsh list --name 2>/dev/null | xargs -r -n1 sudo virsh destroy 2>/dev/null || true
# delete stale o3k bridges — they otherwise linger with a duplicate 10.x gateway
# IP and split host routing across bridges (only `make db-down` won't remove them):
for b in $(ip -br link show type bridge 2>/dev/null | awk '{print $1}' | grep '^br-'); do sudo ip link del "$b"; done
```
> With the SPEC-009 fix, `openstack network delete` now removes its bridge; this
> sweep is only for bridges left by earlier runs / `make db-down`.

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

## 11. US3 / WS-4 — metadata + cloud-init  (**rebuild + restart o3k first**)
Stop o3k (T1), `make build`, restart (§6) so the metadata DNAT + the dnsmasq
classless-static-route option are active. Then in T2:
```bash
# a keypair (once) → SSH login for the VM
[ -f ~/.ssh/id_rsa.pub ] || ssh-keygen -t rsa -N '' -f ~/.ssh/id_rsa
openstack keypair show k1 >/dev/null 2>&1 || openstack keypair create --public-key ~/.ssh/id_rsa.pub k1

# user_data: a shell script (cirros runs #!/bin/sh user-data reliably)
cat > /tmp/ud.sh <<'EOF'
#!/bin/sh
echo "ws4 user_data applied" > /var/tmp/o3k-userdata-ok
EOF

openstack server create --image cirros --flavor m1.small --network net1 \
  --key-name k1 --user-data /tmp/ud.sh vm-md --wait
VMIP=$(openstack server show vm-md -f value -c addresses | grep -oE '[0-9]+(\.[0-9]+){3}' | head -1); echo "VM IP: $VMIP"
```

### 11a. Host side — the redirect + route are programmed
```bash
sudo iptables -t nat -S PREROUTING | grep 169.254.169.254     # -i br-… -d 169.254.169.254 … DNAT --to <gw>:8775
grep dhcp-option=121 /var/lib/o3k/dhcp/configs/*.conf          # 169.254.169.254/32,<gw>,0.0.0.0/0,<gw>
```

### 11b. Guest side — console login (`cirros` / `gocubsgo` — the exact password the login banner prints)
```bash
DOM=$(sudo virsh list --name | grep -i "$(openstack server show vm-md -f value -c id | cut -c1-8)" | head -1)
sudo virsh console "$DOM"      # ENTER for prompt; Ctrl-] to exit
#   $ curl -s http://169.254.169.254/openstack/latest/meta_data.json   # JSON with THIS vm's uuid/hostname
#   $ curl -s http://169.254.169.254/2009-04-04/meta-data/instance-id  # this vm's id
#   $ curl -s http://169.254.169.254/openstack/latest/user_data        # the #!/bin/sh above
#   $ cat /var/tmp/o3k-userdata-ok                                     # "ws4 user_data applied" (cloud-init ran it)
#   $ cat ~/.ssh/authorized_keys                                       # your key (injected)
```

### 11c. SSH — config-drive path (works even if 169.254 is unreachable)
```bash
ssh -o StrictHostKeyChecking=no cirros@$VMIP      # logs in with k1 (key via the config-drive ISO)
```
**Pass** = metadata returns *this* VM's data, `user_data` is served (and applied), and SSH with `k1` works.
If `169.254.169.254` times out in the guest but SSH works, the config-drive path is fine and the
DNAT/route is the host-iterated piece — paste §11a output + the guest's `ip route` and I'll chase it (like WS-2).

## Expected NOT to work yet (later phases)
- **Internet / floating IPs** — the VM cannot reach the internet and is not reachable
  via a floating IP yet. That's **US4 / WS-3** (north-south / NAT).
- **Block storage** — volume attach and boot-from-volume are **US5 / WS-5**.

## Troubleshooting
- **`OS_AUTH_URL: ... source your OpenStack env`** → `source ~/.o3k-env` in T2.
- **"NO o3k dnsmasq"** → see §8 (report the log + `ip netns list`).
- **libvirt can't read the disk** → `sudo chmod -R a+rX /var/lib/o3k/images /var/lib/o3k/instances`.
- **`/var/lib/o3k/images` missing at startup** → Glance falls back to `~/.o3k/images`
  and the overlay backing won't match; recreate the dir (§1) **before** starting o3k.
- **Reset everything** → `make db-down` (Postgres) + Ctrl-C o3k + `sudo rm -rf /var/lib/o3k/*`
  + `sudo pkill -f var/lib/o3k/dhcp`.
