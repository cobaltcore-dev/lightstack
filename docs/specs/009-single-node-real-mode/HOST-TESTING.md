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

## 1. Clean recreate  (**stop o3k first** — Ctrl-C in T1)
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
cd /path/to/o3k                               # ← your local o3k checkout (repo root)
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

# restart-survival (guided — o3k runs foreground; record writes the addresses to
# a file, so verify needs no copy/paste):
./test/ws2_restart_survival_test.sh record vm1
#   …in T1: Ctrl-C o3k, then re-run the §6 command…
./test/ws2_restart_survival_test.sh verify vm1
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

<<<<<<< HEAD
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

## 12. US4 / WS-3 — egress to the internet  (**rebuild + restart o3k first**)
A VM reaches the internet once it's on a tenant network attached to a router with
an external gateway. O3K masquerades it out the node's **uplink** (default-route
iface) — no operator `br-ex`/NIC. Rebuild + restart o3k (§6), then in T2:
```bash
# external network + subnet (the gateway target; CIDR is nominal on single node)
openstack network show ext-net >/dev/null 2>&1 || openstack network create ext-net
openstack subnet show ext-sub >/dev/null 2>&1 || \
  openstack subnet create ext-sub --network ext-net --subnet-range 172.24.4.0/24 --no-dhcp

# router: external gateway + attach the tenant subnet (sub1 on net1)
openstack router show r1 >/dev/null 2>&1 || openstack router create r1
openstack router set r1 --external-gateway ext-net
openstack router add subnet r1 sub1
```
### 12a. Host side — egress NAT is programmed
```bash
UP=$(ip route show default | awk '{print $5; exit}'); echo "uplink: $UP"
cat /proc/sys/net/ipv4/ip_forward                                  # 1
sudo iptables -t nat -S POSTROUTING | grep MASQUERADE              # -s 10.0.0.0/24 -o $UP -j MASQUERADE
sudo iptables -S FORWARD | grep '10.0.0.0/24'                      # the two ACCEPT rules
```
### 12b. Guest side — reach the internet (existing VM is fine; no rebuild needed)
```bash
ssh -o StrictHostKeyChecking=no cirros@$VMIP        # or virsh console
#  inside the VM:
#   $ ping -c3 1.1.1.1        # ICMP egress works (masqueraded out the uplink)
#   $ curl -sI http://1.1.1.1 | head -1   # TCP egress (DNS may be unset — use an IP)
```
**Pass** = `ping 1.1.1.1` from the guest succeeds and `POSTROUTING` shows the MASQUERADE.
If ping fails but the MASQUERADE rule is present, paste `sudo iptables -S FORWARD | head` and the
guest's `ip route` — it's usually the host FORWARD policy (Docker) and I'll adjust the rule ordering.

## 13. US4 / WS-3 — floating IP (inbound reachability)  (**rebuild + restart o3k first**)
Allocate a floating IP from the external network and associate it to the VM; O3K
hosts the FIP as a `/32` on the uplink and DNATs it to the VM's fixed IP. Rebuild
+ restart o3k (§6), then in T2 (uses the `ext-net`/`r1`/`sub1` topology from §12):
```bash
FIP=$(openstack floating ip create ext-net -f value -c floating_ip_address); echo "FIP: $FIP"
openstack server add floating ip vm-md "$FIP"          # use your VM's name
```
### 13a. Host side — the FIP is hosted + DNAT'd
```bash
ip -br addr show enp1s0 | tr ' ' '\n' | grep "$FIP"     # FIP/32 on the uplink
sudo iptables -t nat -S | grep "$FIP"                   # PREROUTING + OUTPUT DNAT -> 10.0.0.x
```
### 13b. Reach the VM **at the floating IP**, from the host (host-local via the OUTPUT DNAT)
```bash
ssh -o StrictHostKeyChecking=no cirros@"$FIP"           # logs into the VM through its FIP
```
### 13c. Disassociate → access stops, rules removed
```bash
openstack server remove floating ip vm-md "$FIP"
sudo iptables -t nat -S | grep "$FIP" || echo "FIP NAT removed"
```
**Pass** = `ssh cirros@$FIP` reaches the VM, and after disassociate the DNAT/address are gone.
- Tested **from the host** (host-local) so it works regardless of LAN routing. To reach the FIP
  from **another machine** — and for the VM's egress to appear *as* the FIP — the external subnet
  must be your **real uplink LAN** with a free address (an operator-supplied routable pool, T036),
  not the nominal `172.24.4.0/24`. v1 single-node FIP = inbound reachability; egress stays on the
  node's shared SNAT.
- Inbound is currently allowed on **all ports** — security-group gating (default-deny, open per
  rule) is the next slice (**T034**).

## 14. US4 / WS-3 — security groups on north-south  (**rebuild + restart o3k first**)
A floating IP is **default-deny** inbound: external traffic to it is gated by the
VM's per-instance ingress chain `O3K-NSI-<fixed-ip>` (conntrack-established + one
ACCEPT per SG ingress rule + default DROP). Needs a **fresh VM** — the default SG
is attached at port creation. Rebuild + restart o3k (§6), then in T2:
```bash
openstack server create --image cirros --flavor m1.small --network net1 --key-name k1 vm-sg --wait
FIP2=$(openstack floating ip create ext-net -f value -c floating_ip_address); echo "FIP2: $FIP2"
openstack server add floating ip vm-sg "$FIP2"
CH="O3K-NSI-$(openstack server show vm-sg -f value -c addresses | grep -oE '10\.0\.0\.[0-9]+' | head -1 | tr . -)"; echo "chain: $CH"
```
> **Why we verify the chain, not SSH-from-here:** security groups gate **forwarded**
> traffic (external client → uplink → `FORWARD` → `O3K-NSI`). SSH *from this host*
> to the FIP takes the `OUTPUT`/host-local path (the host is the hypervisor — its
> own access isn't SG-gated), so it **bypasses the chain** and is not a valid probe.
> The chain content below is the enforcement a real external client hits.

### 14a. Default SG already permits its seeded ingress (tcp/22)
```bash
sudo iptables -S "$CH"     # -p tcp --dport 22 -j ACCEPT (from the seeded default-SG rule)
                           #   + conntrack RELATED,ESTABLISHED -j ACCEPT  then  -j DROP
```
The default SG seeds a `tcp/22` ingress rule (migration 002), so SSH is open by default and
**everything else is denied** — that DROP is the default-deny.

### 14b. Open a *non-seeded* port (8080) → the ACCEPT appears immediately
```bash
openstack security group rule create --proto tcp --dst-port 8080 --remote-ip 0.0.0.0/0 default
sudo iptables -S "$CH"     # now also: -p tcp --dport 8080 -j ACCEPT  (before the DROP)
```
### 14c. Remove the rule → the 8080 ACCEPT disappears (22 stays — it's seeded)
```bash
RID=$(openstack security group rule list default -f value -c ID -c "Port Range" | awk '$2 ~ /8080/{print $1; exit}')
openstack security group rule delete "$RID"
sudo iptables -S "$CH"     # back to :22 + conntrack + DROP (no :8080)
```
**Pass** = the chain carries the seeded `:22`, gains `--dport 8080 -j ACCEPT` the instant you add the
rule, and loses it when you remove it — with the default `DROP` throughout. (A real end-to-end probe
would come from another machine on the uplink LAN — its SYN transits `FORWARD`/`O3K-NSI`; SSH *from
this host* bypasses the chain via `OUTPUT`, so it isn't a valid probe.)

## 15. US5 / WS-5 — block storage  (**rebuild + restart o3k first**)
Volumes are real local qcow2 files under `/var/lib/o3k/volumes`. Needs `net1`/`sub1`
+ a keyed VM. Rebuild + restart o3k (§6), recreate `net1`/`sub1` (§7), then in T2:

### 15a. Volume attach (synchronous) — guest sees the device, data persists
```bash
openstack server create --image cirros --flavor m1.small --network net1 --key-name k1 vm-vol --wait
VMIP=$(openstack server show vm-vol -f value -c addresses | grep -oE '10\.0\.0\.[0-9]+' | head -1)
openstack volume create --size 1 datavol          # status should be 'available' immediately
openstack server add volume vm-vol datavol         # synchronous: returns only after libvirt attached it
ssh cirros@$VMIP 'lsblk | grep vdb; sudo mkfs.ext2 -q /dev/vdb && sudo mount /dev/vdb /mnt && echo hi | sudo tee /mnt/f && sudo umount /mnt'
openstack server remove volume vm-vol datavol
openstack server add volume vm-vol datavol
ssh cirros@$VMIP 'sudo mount /dev/vdb /mnt && cat /mnt/f && sudo umount /mnt'   # → "hi" (persisted)
```
**Pass** = the guest sees `/dev/vdb`, and the file survives detach/re-attach. (Attach is synchronous: a libvirt failure returns an error and reverts, never a false success.)

### 15b. Volume-from-image (bootable)
```bash
openstack volume create --size 1 --image cirros bootvol
openstack volume show bootvol -f value -c bootable -c status   # → True  available
```

### 15c. Boot-from-volume
```bash
openstack server create --flavor m1.small --volume bootvol --network net1 --key-name k1 vm-bfv --wait
BFVIP=$(openstack server show vm-bfv -f value -c addresses | grep -oE '10\.0\.0\.[0-9]+' | head -1)
ssh cirros@$BFVIP 'echo "booted from volume on $(hostname)"'    # logs in → booted off the volume
```
**Pass** = `vm-bfv` boots off the volume (its root disk is the volume's qcow2, no image overlay).

### 15d. Delete semantics — release vs. delete-on-termination (FR-023)
Deleting a server releases its attached volumes (`available`), or **deletes** them when
the attachment is `delete_on_termination=true` (e.g. boot-from-volume created with that flag).
```bash
# (a) release: deleting vm-bfv frees bootvol (NOT deleted — file persists)
openstack server delete vm-bfv
openstack volume show bootvol -f value -c status          # → available (was in-use)
BVID=$(openstack volume show bootvol -f value -c id); ls /var/lib/o3k/volumes/volume-$BVID.qcow2   # still there
# (b) release of a data volume: delete vm-vol → datavol freed
openstack server delete vm-vol
openstack volume show datavol -f value -c status          # → available
# (c) delete-on-termination: boot a VM whose root volume is flagged, delete it → volume gone
openstack volume create --size 1 --image cirros bootvol2
openstack server create --flavor m1.small --network net1 --key-name k1 \
  --block-device uuid=bootvol2,source_type=volume,boot_index=0,delete_on_termination=true vm-dot --wait
openstack server delete vm-dot
openstack volume show bootvol2                            # → No Volume found (deleted)
DID=$(...); ls /var/lib/o3k/volumes/volume-<bootvol2-id>.qcow2 2>&1   # → No such file
```
**Pass** = (a)/(b) the volume returns to `available` and its backing file survives; (c) the
flagged volume is gone (DB 404 + backing file removed). RBD delete-on-termination cleanup of
the backing image routes through Cinder (local backing is what's removed here).

||||||| parent of d29c106 (feat(realmode): SPEC-009 US3 — metadata reachability + cloud-init key/user_data (WS-4))
=======
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

>>>>>>> d29c106 (feat(realmode): SPEC-009 US3 — metadata reachability + cloud-init key/user_data (WS-4))
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
- **Reset everything** → see **Cleanup — full reset** above (undefine VMs + bridges/netns/iptables +
  `rm -rf /var/lib/o3k/*` + `make db-down`), or just reboot the host then clear those three.
