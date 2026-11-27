# Host testing guide — SPEC-033 round 2 (real mode, one node)

Validates cloud-storage#579's round 2 on one real host (FR-048; research R45): the chart in real mode on one
Kubernetes node (k3s), with Horizon. It covers the network node's router and DHCP namespaces, the port firewall,
a revived guest, a gateway change, deletions, reconciliation, restarts and readiness. The automated suites cover the
rest (`baseline-gates.md`, Round 2
results).

Run all commands from the repo root, top to bottom: §0, §1, §2, §3. §0 only reads; §3 undoes what §1 and §2
change. Shell T1 runs everything; T2 and T3 join where a step says. The run keeps its records in a database of its
own, `ls579`, inside the PostgreSQL `make db-up` starts, so the shared test database is neither read nor changed. To
start over, run §3 first.

| Validates | Proved in |
|---|---|
| FR-023 (a), SC-002: a rule on a non-default group opens and closes floating-IP traffic and traffic from another VM, through the compute API | 2.5 |
| FR-023 (b), FR-017: the same through the Networking API | 2.6 |
| SC-003: the last group's removal answers `202` and closes new connections; an established one stays | 2.7 |
| SC-010: one attempt per second to a closed port gets nothing through, across passes and changes | 2.4, read in 2.11 |
| FR-008b, SC-013, FR-041: a rule added and deleted, a remote group resolved to its members | 2.8 |
| FR-049, FR-038: a port forwarding through the router namespace; a guest without a floating IP out through the gateway's SNAT | 2.9 |
| SC-014, FR-008a: a floating IP moved, disassociated and deleted | 2.10 |
| FR-010: a NAT rule and a port-chain rule no record holds, removed within a pass | 2.11 |
| FR-033, FR-035, FR-037: the namespaces, legs, addresses and proxy entries; no bridge holds an address | 2.1 |
| FR-037, R29: DHCP and metadata through the DHCP port's address | 2.2 |
| FR-038, FR-041: the gateway address; egress through the port's chains and the router's NAT | 2.3 |
| FR-039: the refusals | 2.12 |
| FR-036: the pod restarted with an established connection flowing | 2.13 |
| FR-046: readiness failing, naming `bridge-netfilter`, while the bridge settings do not hold | 2.14 |
| FR-021, SC-006: Horizon's instance *Edit Security Groups* | 2.15 |
| FR-041, SPEC-025 FR-025: a guest revived by its start has its TAP named for its port, on its network's bridge, and filtered by its chains | 2.16 |
| FR-045: a subnet's gateway change moves the default route of every router with a gateway port on the subnet | 2.17 |
| FR-040, FR-008d, FR-008e, SC-014: a deletion of each kind removes its host state; a deleted and a force-deleted server release their floating IPs | 2.18 |

---

## 0. Prerequisites

One block; it needs no root. This host is the Kubernetes node and the client. It needs:
- libvirt with KVM (`/dev/kvm`), its socket at `/var/run/libvirt/libvirt-sock` (on a modular-daemon host,
  `virtproxyd.socket` provides it), and `virsh`, which warms libvirt and reads the domains.
- The kernel modules `br_netfilter`, `xt_physdev`, `xt_comment` and `xt_CT`, loaded, built in or loadable: the port
  firewall and the routers' NAT. The image brings its own `iptables`.
- SELinux, if the host has it, in permissive mode, and no firewalld: enforcing SELinux keeps QEMU off Lightstack's
  state under `/var/lib/lightstack`, and firewalld drops the guests' DHCP and the routers' NAT. On openSUSE, the
  once-per-boot host block in `docs/HOST_TESTING_OPENSUSE.md` sets both and makes the libvirt socket.
- Nothing you want to keep under `/var/lib/lightstack`: teardown purges its storage directories (`images`,
  `instances`, `volumes`, `cloud-init`, `dhcp`, the console directories) whole, earlier deployments' files included.
- `k3s`, on your `PATH` or in `/usr/local/bin`, where its installer puts it. The steps call it through `sudo` by its
  full path: `sudo`'s `secure_path` may lack `/usr/local/bin`, and openSUSE's does.
- `kubectl` reaching the cluster as you, and `helm` 3.12 or later (the chart's README). k3s writes its kubeconfig to
  `/etc/rancher/k3s/k3s.yaml`, readable by root only; copy it to `~/.kube/config`.
- Docker or Podman, either: builds the image and runs `make db-up`'s PostgreSQL.
- `iptables-nft`, for teardown's last check: the image's `iptables` is the nf_tables one, so a legacy `iptables` on
  the host lists none of its rules (the test steps read them through the pod).
- `make`, `ip` (iproute2, with `-j` and `netns`), `ss`, `python3`, `jq`, `curl`, `ssh`, `ssh-keygen`, `openstack`
  (python-openstackclient), `sudo`.

```bash
missing=0
for t in make ip ss virsh python3 jq curl ssh ssh-keygen openstack sudo kubectl helm; do command -v $t >/dev/null || { echo "MISSING: $t"; missing=1; }; done
command -v k3s >/dev/null || test -x /usr/local/bin/k3s || { echo "MISSING: k3s"; missing=1; }
command -v iptables-nft >/dev/null || test -x /usr/sbin/iptables-nft || test -x /sbin/iptables-nft || { echo "MISSING: iptables-nft"; missing=1; }
command -v docker >/dev/null || command -v podman >/dev/null || { echo "MISSING: docker or podman"; missing=1; }
for m in br_netfilter xt_physdev xt_comment xt_CT; do test -d /sys/module/$m || grep -qE "/$m\.ko" /lib/modules/$(uname -r)/modules.dep /lib/modules/$(uname -r)/modules.builtin 2>/dev/null || { echo "MISSING: kernel module $m"; missing=1; }; done
test -c /dev/kvm || { echo "MISSING: /dev/kvm (KVM)"; missing=1; }
test -S /var/run/libvirt/libvirt-sock || { echo "MISSING: /var/run/libvirt/libvirt-sock (libvirt; on a modular-daemon host, virtproxyd.socket provides it)"; missing=1; }
[ "$(cat /sys/fs/selinux/enforce 2>/dev/null)" = 1 ] && { echo "MISSING: SELinux in permissive mode (it is enforcing)"; missing=1; }
pgrep -x firewalld >/dev/null && { echo "MISSING: firewalld stopped (it is running)"; missing=1; }
kubectl get nodes 2>/dev/null || { echo "MISSING: a cluster kubectl reaches (copy /etc/rancher/k3s/k3s.yaml to ~/.kube/config, or set KUBECONFIG)"; missing=1; }
helm version --short 2>/dev/null; virsh --version 2>/dev/null; ip -V; openstack --version; jq --version; python3 --version
[ "$missing" = 0 ] && echo "PREREQS OK" || echo "PREREQS FAILED: install the missing tools above, then re-run"
```

**Pass =** `PREREQS OK`, no `MISSING:` line, and `kubectl get nodes` lists one `Ready` node.

Install anything reported missing yourself before going on; this runbook installs nothing.

---

## 1. Setup steps

### 1.1 Lightstack and Horizon on k3s (T1)

**Shell T1 (client)** — keep it for the whole run.

Floating IPs are addresses of the host's own LAN: the network node answers ARP for them on its uplink (FR-035).
Pick at least four **unused** addresses on that LAN and export them first, for example
`export POOL_START=192.168.1.240 POOL_END=192.168.1.249`. The block derives the uplink, its subnet and its gateway,
refuses a pool outside that subnet or holding this host or the gateway, and refuses to install while Lightstack's
ports are taken (it runs `hostNetwork`) or while a namespace `lightstack` exists, whose release the install would
take over and §3 deletes. It then records what teardown restores (the PostgreSQL container's state,
k3s's images, the host settings real mode changes, whether `/var/lib/lightstack` existed) in `/tmp/ls579.env`,
starts `make db-up`'s PostgreSQL if it is not running and creates the database `ls579` in it, builds the image and
imports it into k3s. It warms libvirt: Lightstack reads libvirt's capabilities once at start, within a second, and
refuses every guest start if that read fails, which a socket-activated daemon starting cold can make it do. Then it
installs the chart in real mode, then Horizon. `dataRetentionPolicy=delete` makes
`helm uninstall` run the chart's own teardown hooks in §3: scale to zero, then `lightstack cleanup` with the host
mounts.

```bash
export NODE_IP=$(ip -j route get 1.1.1.1 | jq -r '.[0].prefsrc')
export UPLINK=$(ip -j route show default | jq -r '.[0].dev')
export EXT_GW=$(ip -j route show default | jq -r '.[0].gateway')
export EXT_NET=$(python3 -c "import ipaddress,sys; print(ipaddress.ip_interface(sys.argv[1]).network)" "$(ip -j -4 addr show dev $UPLINK | jq -r '.[0].addr_info[0] | "\(.local)/\(.prefixlen)"')")
POOL_OK=$(python3 -c "import ipaddress as i,sys; n=i.ip_network(sys.argv[1]); a,b,h,g=(i.ip_address(x) for x in sys.argv[2:6]); print('yes' if a in n and b in n and int(b)-int(a)>=3 and not a<=h<=b and not a<=g<=b else 'no')" "$EXT_NET" "${POOL_START:-0.0.0.0}" "${POOL_END:-0.0.0.0}" "$NODE_IP" "$EXT_GW" 2>/dev/null)
BUSY=$(ss -lnt | grep -cE ':(35357|8774|8775|8776|8778|9292|9696) ')
echo "node $NODE_IP uplink $UPLINK net $EXT_NET gateway $EXT_GW pool ${POOL_START:-unset}-${POOL_END:-unset}"
if [ ! -f config/lightstack.yaml ]; then
  echo "STOP: run this from the repo root"
elif [ "$POOL_OK" != yes ]; then
  echo "STOP: export POOL_START and POOL_END, at least four unused addresses inside $EXT_NET holding neither $NODE_IP nor $EXT_GW, then re-run this block"
elif kubectl get namespace lightstack >/dev/null 2>&1; then
  echo "STOP: the namespace lightstack exists; this run installs into it and §3 deletes it, so remove it or run on another host, then re-run this block"
elif [ "$BUSY" -gt 0 ]; then
  echo "STOP: Lightstack's ports are taken (ss -lnt shows which); stop what listens there, often the stub Compose stack, then re-run this block"
else
  sudo -v
  ENGINE=$(command -v docker || command -v podman)
  K3S=$(command -v k3s || echo /usr/local/bin/k3s)
  DB_STATE=$($ENGINE inspect -f '{{.State.Running}}' lightstack-postgres 2>/dev/null || echo absent)
  sudo $K3S ctr images ls -q > /tmp/ls579-k3s-images-before
  printf 'export %s=%s\n' OS_AUTH_URL "http://$NODE_IP:35357/v3" OS_USERNAME admin OS_PASSWORD secret OS_PROJECT_NAME default OS_USER_DOMAIN_NAME Default OS_PROJECT_DOMAIN_NAME Default OS_IDENTITY_API_VERSION 3 ENGINE "$ENGINE" K3S "$K3S" NODE_IP "$NODE_IP" UPLINK "$UPLINK" EXT_GW "$EXT_GW" EXT_NET "$EXT_NET" POOL_START "$POOL_START" POOL_END "$POOL_END" DB_STATE "$DB_STATE" FWD_BEFORE "$(cat /proc/sys/net/ipv4/ip_forward)" NF4_BEFORE "$(cat /proc/sys/net/bridge/bridge-nf-call-iptables 2>/dev/null || echo unloaded)" NF6_BEFORE "$(cat /proc/sys/net/bridge/bridge-nf-call-ip6tables 2>/dev/null || echo unloaded)" ROOT_BEFORE "$(test -d /var/lib/lightstack && echo kept || echo absent)" > /tmp/ls579.env
  mkdir -p /tmp/ls579-bin; command -v docker >/dev/null || ln -sf "$(command -v podman)" /tmp/ls579-bin/docker
  [ "$DB_STATE" = false ] && $ENGINE start lightstack-postgres
  [ "$DB_STATE" = absent ] && PATH=/tmp/ls579-bin:$PATH make db-up
  for i in $(seq 1 60); do $ENGINE exec lightstack-postgres pg_isready -h 127.0.0.1 -U lightstack -q && break; sleep 1; done
  $ENGINE exec lightstack-postgres dropdb -U lightstack --if-exists --force ls579
  $ENGINE exec lightstack-postgres createdb -U lightstack ls579
  $ENGINE build -f build/package/Dockerfile -t docker.io/library/lightstack:spec033 .
  rm -f /tmp/lightstack-spec033.tar
  $ENGINE save -o /tmp/lightstack-spec033.tar docker.io/library/lightstack:spec033
  sudo $K3S ctr images import /tmp/lightstack-spec033.tar
  sudo virsh -c qemu:///system capabilities >/dev/null && echo "libvirt warm"
  helm upgrade --install lightstack charts/lightstack --namespace lightstack --create-namespace --set image.repository=docker.io/library/lightstack --set image.tag=spec033 --set image.pullPolicy=Never --set deploymentMode=single-node --set realMode.enabled=true --set realMode.hostNetwork=true --set nova.libvirtMode=real --set neutron.networkingMode=iptables --set neutron.securityGroupMode=iptables --set cinder.storageMode=local --set glance.storageMode=local --set endpointHost=$NODE_IP --set secrets.jwtSecret=spec033-host-test --set secrets.adminPassword=secret --set database.url='postgres://lightstack:secret@127.0.0.1:5432/ls579?sslmode=disable' --set dataRetentionPolicy=delete --set teardown.kubectlImage=alpine/k8s:1.31.0
  kubectl -n lightstack rollout status deploy/lightstack-lightstack --timeout=600s
  helm upgrade --install horizon charts/horizon --namespace lightstack
  kubectl -n lightstack rollout status deploy/horizon-lightstack-horizon --timeout=900s
  kubectl -n lightstack get pod -l app.kubernetes.io/name=lightstack -o jsonpath='{.items[0].spec.containers[0].volumeMounts[*].mountPath}'; echo
  curl -s -w ' %{http_code}\n' http://$NODE_IP:35357/readyz
fi
```

**Pass =** no `STOP:` line, the image builds, `libvirt warm`, both rollouts finish, the mount paths include
`/run/netns` and `/lib/modules`, and `/readyz` prints `ok 200`.

### 1.2 Fixtures (T1)

The guests are cirros 0.6.2 with an ssh key, and a user-data script that answers on ports 8080, 8081 and 8082 with
`<hostname>:<port>`, so every answer names the guest that gave it. `ls579-vm1` carries `default` and `ls579-ssh`;
`ls579-vm2` carries `ls579-ssh` and `ls579-client`, and is no member of `default`, whose members admit each other
(FR-043), so only `ls579-web` can open `ls579-vm1` to it. Groups go by id where the client resolves them: an
administrator sees every project's `default` (FR-057). Everything later blocks need lands in `/tmp/ls579.env`, with
these helpers:
- `probe <address> <port>`: one connection attempt from this host with a one-second timeout, printing `open` and
  what the guest sent, `timeout`, `refused` (a packet reached the guest and found the port closed), or
  `unreachable` (no route to the address);
- `ssh1 <address> <command>`: a command on a guest through its floating IP;
- `from_vm2 <address> <port>`: a connection attempt from `ls579-vm2`, printing what it got and ` rc=`;
- `ipt …` and `nsipt <namespace> …`: `iptables` in the Lightstack pod, which wrote the rules, on the host and in a
  namespace. Where other programs on the host keep rules in the legacy tables (k3s may), these print `# Warning:
  iptables-legacy tables present` to stderr; Lightstack's rules are all in the nf_tables ones, and no count is
  affected.

```bash
source /tmp/ls579.env
test -s /tmp/ls579-cirros.img || { curl -fsSL -o /tmp/ls579-cirros.img.part http://download.cirros-cloud.net/0.6.2/cirros-0.6.2-x86_64-disk.img && mv /tmp/ls579-cirros.img.part /tmp/ls579-cirros.img; }
if [ ! -s /tmp/ls579-cirros.img ]; then
  echo "STOP: the cirros image did not download, curl says why; re-run this block"
else
  openstack image create --disk-format qcow2 --container-format bare --public --file /tmp/ls579-cirros.img cirros >/dev/null
  test -s /tmp/ls579.key || ssh-keygen -q -t rsa -b 3072 -N '' -f /tmp/ls579.key
  openstack keypair create --public-key /tmp/ls579.key.pub ls579 >/dev/null
  printf '\043!/bin/sh\nfor p in 8080 8081 8082; do (while true; do echo "$(hostname):$p" | nc -l -p $p; done) & done\n' > /tmp/ls579-userdata.sh
  cat > /tmp/ls579-probe.py <<'PY'
import errno, socket, sys
s = socket.socket()
s.settimeout(1)
try:
    s.connect((sys.argv[1], int(sys.argv[2])))
except socket.timeout:
    print("timeout")
except ConnectionRefusedError:
    print("refused")
except OSError as e:
    print("unreachable" if e.errno in (errno.EHOSTUNREACH, errno.ENETUNREACH) else "error", e.strerror)
else:
    try:
        data = s.recv(100).decode(errors="replace").strip()
    except socket.timeout:
        data = ""
    print("open", data)
finally:
    s.close()
PY
  PROJECT=$(openstack project show default -f value -c id)
  openstack network create --external ls579-ext >/dev/null
  openstack subnet create ls579-ext-sub --network ls579-ext --subnet-range $EXT_NET --gateway $EXT_GW --no-dhcp --allocation-pool start=$POOL_START,end=$POOL_END >/dev/null
  NET=$(openstack network create ls579-net -f value -c id)
  openstack subnet create ls579-sub --network ls579-net --subnet-range 10.79.0.0/24 >/dev/null
  R=$(openstack router create ls579-r -f value -c id)
  openstack router set --external-gateway ls579-ext ls579-r
  openstack router add subnet ls579-r ls579-sub
  SG_DEFAULT=$(openstack security group list --project $PROJECT -f json | jq -r '.[] | select(.Name=="default") | .ID')
  SG_SSH=$(openstack security group create ls579-ssh -f value -c id)
  SG_WEB=$(openstack security group create ls579-web -f value -c id)
  SG_CLIENT=$(openstack security group create ls579-client -f value -c id)
  openstack security group rule create --protocol tcp --dst-port 22 $SG_SSH >/dev/null
  openstack security group rule create --protocol tcp --dst-port 8080 $SG_WEB >/dev/null
  openstack server create --flavor m1.tiny --image cirros --network ls579-net --key-name ls579 --user-data /tmp/ls579-userdata.sh --security-group $SG_DEFAULT --security-group $SG_SSH ls579-vm1 >/dev/null
  openstack server create --flavor m1.tiny --image cirros --network ls579-net --key-name ls579 --user-data /tmp/ls579-userdata.sh --security-group $SG_SSH --security-group $SG_CLIENT ls579-vm2 >/dev/null
  for v in ls579-vm1 ls579-vm2; do for i in $(seq 1 60); do s=$(openstack server show $v -f value -c status); [ "$s" = ACTIVE ] || [ "$s" = ERROR ] && break; sleep 5; done; echo "$v $s"; done
  P1=$(openstack port list --server ls579-vm1 -f value -c ID); P2=$(openstack port list --server ls579-vm2 -f value -c ID)
  IP1=$(openstack port show $P1 -f json | jq -r '.fixed_ips[0].ip_address'); IP2=$(openstack port show $P2 -f json | jq -r '.fixed_ips[0].ip_address')
  VM1_ID=$(openstack server show ls579-vm1 -f value -c id); VM2_ID=$(openstack server show ls579-vm2 -f value -c id)
  FIP1_ID=$(openstack floating ip create ls579-ext --port $P1 -f value -c id); FIP2_ID=$(openstack floating ip create ls579-ext --port $P2 -f value -c id)
  FIP1=$(openstack floating ip show $FIP1_ID -f value -c floating_ip_address); FIP2=$(openstack floating ip show $FIP2_ID -f value -c floating_ip_address)
  GW_ADDR=$(openstack router show ls579-r -f json | jq -r '.external_gateway_info.external_fixed_ips[0].ip_address')
  printf 'export %s=%s\n' NET "$NET" R "$R" SG_DEFAULT "$SG_DEFAULT" SG_SSH "$SG_SSH" SG_WEB "$SG_WEB" SG_CLIENT "$SG_CLIENT" P1 "$P1" P2 "$P2" IP1 "$IP1" IP2 "$IP2" VM1_ID "$VM1_ID" VM2_ID "$VM2_ID" FIP1 "$FIP1" FIP2 "$FIP2" FIP1_ID "$FIP1_ID" FIP2_ID "$FIP2_ID" GW_ADDR "$GW_ADDR" >> /tmp/ls579.env
  cat >> /tmp/ls579.env <<'ENV'
probe() { python3 /tmp/ls579-probe.py "$1" "$2"; }
ssh1() { ssh -i /tmp/ls579.key -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 -o LogLevel=ERROR "cirros@$1" "$2"; }
from_vm2() { ssh1 "$FIP2" "nc -w 2 $1 $2 </dev/null; echo \" rc=\$?\""; }
ipt() { kubectl -n lightstack exec deploy/lightstack-lightstack -c lightstack -- iptables "$@"; }
nsipt() { local ns=$1; shift; kubectl -n lightstack exec deploy/lightstack-lightstack -c lightstack -- ip netns exec "$ns" iptables "$@"; }
ENV
  source /tmp/ls579.env
  for a in $FIP1 $FIP2; do for i in $(seq 1 60); do ssh1 $a true 2>/dev/null && break; sleep 5; done; done
  echo "vm1 $IP1 via $FIP1, vm2 $IP2 via $FIP2, gateway $GW_ADDR"
  ssh1 $FIP1 hostname; ssh1 $FIP2 hostname
  ssh1 $FIP1 'netstat -lnt' | grep -cE ':808[012] '; ssh1 $FIP2 'netstat -lnt' | grep -cE ':808[012] '
fi
```

**Pass =** no `STOP:` line; `ls579-vm1 ACTIVE` and `ls579-vm2 ACTIVE`; both fixed addresses in `10.79.0.0/24`, the
gateway and both
floating IPs from your pool; `ls579-vm1` and `ls579-vm2` (key, DHCP, the router and both floating IPs work); then
`3` and `3` (each guest's three listeners run).

---

## 2. Test steps

In T1 unless a step says otherwise.

### 2.1 The layout (read only)

```bash
source /tmp/ls579.env
ip netns list | grep -E "qrouter-$R|qdhcp-$NET"
sudo ip netns exec qrouter-$R ip -br addr | grep -E '^(qg|qr)-'
sudo ip netns exec qdhcp-$NET ip -br addr | grep '^tap'
ip -br link | grep -E '^(lg|lr|ld)-' | awk '{print $1}' | tr '\n' ' '; echo
ip -4 -o addr show dev br-${NET:0:8} | wc -l
ip neigh show proxy dev $UPLINK | grep -cwF -e "$GW_ADDR" -e "$FIP1" -e "$FIP2"
nsipt qrouter-$R -t nat -S | grep -c 'comment lightstack'
ipt -S | grep -E '^-N ls-' | awk '{print $2}' | sort | tr '\n' ' '; echo
ipt -t raw -S ls-ct | grep -c physdev
```

**Expect:** `qrouter-<R>` and `qdhcp-<NET>`; `qg-…` holds `$GW_ADDR` with the LAN's prefix, `$FIP1/32` and
`$FIP2/32`, and `qr-…` holds `10.79.0.1/24`; the `tap…` in `qdhcp-` holds the DHCP port's address, the subnet's
lowest pool address (normally `10.79.0.2/24`), and `169.254.169.254/32`; one `lg-`, one `lr-` and one `ld-` leg;
`0` (the bridge `br-<net[:8]>` holds no IPv4 address); `3` proxy entries (the gateway and both floating
addresses); a non-zero count of marked NAT rules; the chains `ls-filter-top`, `ls-l3-fwd`, `ls-local`,
`ls-sg-chain`, `ls-sg-fallback`, and `ls-i`, `ls-o`, `ls-s` with the first ten characters of each port's id; `2`
zone rules, one per port.

### 2.2 DHCP and metadata through the DHCP address

```bash
source /tmp/ls579.env
DHCP_PORT=$(openstack port list --network ls579-net --device-owner network:dhcp -f value -c ID)
openstack port show $DHCP_PORT -f json | jq -r '.fixed_ips[0].ip_address'
sudo ip netns exec qdhcp-$NET ss -lntu | grep -E '169.254.169.254:80|:67 '
openstack console log show ls579-vm1 | grep -E 'ip-route:(default|169.254.169.254)'
ssh1 $FIP1 'curl -s -w "\n" http://169.254.169.254/latest/meta-data/hostname'
ssh1 $FIP1 'curl -s -w "\n" http://169.254.169.254/latest/meta-data/instance-id'
```

**Expect:** the DHCP port's address (normally `10.79.0.2`); in the namespace, Lightstack listening on
`169.254.169.254:80` and dnsmasq on `:67`; the guest's routes from DHCP option 121, `ip-route:default via
10.79.0.1` and `ip-route:169.254.169.254 via <the DHCP port's address>`; then `ls579-vm1` and an `i-…` id: the
metadata service answers the guest through the DHCP address and knows it by network and address.

### 2.3 Egress through the port's chains, and the router's NAT

```bash
source /tmp/ls579.env
ssh1 $FIP1 "ping -c 2 -W 2 $EXT_GW"; echo "rc=$?"
nsipt qrouter-$R -t nat -S ls-l3-snat
nsipt qrouter-$R -t nat -S ls-l3-float-snat
```

**Expect:** two replies and `rc=0`: the guest's egress leaves through its chains and the router, and the replies
find their way back through the proxy entry. `ls-l3-snat` holds the jump to `ls-l3-float-snat`, `-o qg-… -j SNAT
--to-source $GW_ADDR` and the hairpin rule (`--ctstate DNAT … --to-source $GW_ADDR`); `ls-l3-float-snat` holds
`-s $IP1/32 … --to-source $FIP1` and the same for vm2; every rule is marked `lightstack`. vm1 leaves as its
floating address; 2.9 sends a guest without one out through the gateway.

### 2.4 Start the closed-port probe (T2)

**Shell T2 (probe)** — leave it running until 2.11 reads it. One connection attempt per second to vm1's port 8081,
where a listener runs and no rule ever admits it.

```bash
source /tmp/ls579.env
rm -f /tmp/ls579-sc010.log
while true; do python3 /tmp/ls579-probe.py $FIP1 8081 | tee -a /tmp/ls579-sc010.log; done
```

**Expect:** a `timeout` line a second, read in 2.11. An `open` line means a connection got through, a `refused`
line that a packet passed the firewall and found the port closed (SC-010); both are failures. While 2.10 has the
floating IP away, `unreachable` lines may appear: the address leads nowhere then, which is not a pass.

### 2.5 A group through the compute API (SC-002)

```bash
source /tmp/ls579.env
probe $FIP1 8080; from_vm2 $IP1 8080
openstack server add security group ls579-vm1 ls579-web
probe $FIP1 8080; from_vm2 $IP1 8080
openstack server remove security group ls579-vm1 ls579-web
probe $FIP1 8080; from_vm2 $IP1 8080
```

**Expect:** first `timeout`, and from vm2 no `ls579-vm1:8080` line, only its ` rc=`; after the add,
`open ls579-vm1:8080`, and `ls579-vm1:8080` from vm2; after the removal, as at first. Each change is in force when
the command returns, with no restart.

### 2.6 A group through the Networking API

```bash
source /tmp/ls579.env
openstack port set --security-group $SG_WEB $P1
probe $FIP1 8080; openstack port show $P1 -f json | jq -r '.security_group_ids | length'
openstack port unset --security-group $SG_WEB $P1
probe $FIP1 8080; openstack port show $P1 -f json | jq -r '.security_group_ids | length'
```

**Expect:** `open ls579-vm1:8080` and `3` groups; then `timeout` and `2`.

### 2.7 The last group, and an established connection (SC-003)

**Shell T3 (stream)** — leave it running until the end of this step:

```bash
source /tmp/ls579.env
ssh1 $FIP1 'while true; do date; sleep 1; done'
```

Back in **T1**:

```bash
source /tmp/ls579.env
openstack server remove security group ls579-vm1 ls579-ssh
openstack server remove security group ls579-vm1 default; echo "rc=$?"
openstack port show $P1 -f json | jq -r '.security_group_ids | length'
probe $FIP1 22; ssh1 $FIP1 true; echo "new ssh rc=$?"
openstack server add security group ls579-vm1 default
openstack server add security group ls579-vm1 ls579-ssh
ssh1 $FIP1 true; echo "new ssh rc=$?"
```

**Expect:** the last group's removal is accepted (`rc=0`) and leaves `0` groups; then `timeout` and
`new ssh rc=255`, while **T3** keeps printing a line every second; with the groups back, `new ssh rc=0`. Then stop
**T3** with Ctrl-C.

### 2.8 A rule added and deleted; a remote group resolved to its members (SC-013)

```bash
source /tmp/ls579.env
openstack server add security group ls579-vm1 ls579-web
RULE=$(openstack security group rule create --protocol tcp --dst-port 8082 --remote-group $SG_CLIENT $SG_WEB -f value -c id)
from_vm2 $IP1 8082; probe $FIP1 8082
openstack security group rule delete $RULE
from_vm2 $IP1 8082; ipt -S ls-i${P1:0:10} | grep -c 8082
```

**Expect:** with the rule, `ls579-vm1:8082` from vm2, a member of `ls579-client`, and `timeout` through the
floating IP, since this host is no member; after the deletion, no `ls579-vm1:8082` from vm2, and the port's
ingress chain holds no `8082` rule (`0`). `ls579-web` stays on vm1 from here on.

### 2.9 A port forwarding, and a guest without a floating IP out through the gateway (FR-049, FR-038)

vm2 gives up its floating IP. A port forwarding on that floating IP, now no port's, external port 2222 to vm2's
port 22, reaches it, and vm2 pings out: with no floating IP of its own it leaves as the router's gateway address.
The forwarding finds its router through vm2's port, as upstream's, and a floating IP a port uses is refused.

```bash
source /tmp/ls579.env
openstack floating ip unset --port $FIP2_ID
PF=$(openstack floating ip port forwarding create --internal-ip-address $IP2 --port $P2 --internal-protocol-port 22 --external-protocol-port 2222 --protocol tcp $FIP2_ID -f value -c id)
nsipt qrouter-$R -t nat -S | grep -c 2222
ssh -p 2222 -i /tmp/ls579.key -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 -o LogLevel=ERROR cirros@$FIP2 "hostname; ping -c 2 -W 2 $EXT_GW"; echo "rc=$?"
openstack floating ip port forwarding delete $FIP2_ID $PF
nsipt qrouter-$R -t nat -S | grep -c 2222; probe $FIP2 2222
```

**Expect:** `1` forwarding rule; `ls579-vm2`, two replies and `rc=0`: vm2 is reached through the forwarding and
gets out through the gateway's SNAT; after the deletion, `0` and `timeout`. vm2 stays without a floating IP until
2.10 gives it back.

### 2.10 A floating IP moved, disassociated and deleted (SC-014)

```bash
source /tmp/ls579.env
openstack server add security group ls579-vm2 ls579-web
openstack floating ip set --port $P2 $FIP1_ID
probe $FIP1 8080; nsipt qrouter-$R -t nat -S | grep -F -- "-d $FIP1/32"
openstack floating ip unset --port $FIP1_ID
probe $FIP1 8080; nsipt qrouter-$R -t nat -S | grep -cF -- "$FIP1/32"; sudo ip netns exec qrouter-$R ip -br addr | grep -cF "$FIP1/32"
openstack floating ip delete $FIP1_ID
ip neigh show proxy dev $UPLINK | grep -cwF "$FIP1"
FIP1_ID=$(openstack floating ip create ls579-ext --port $P1 -f value -c id)
openstack floating ip set --port $P2 $FIP2_ID
printf 'export %s=%s\n' FIP1_ID "$FIP1_ID" >> /tmp/ls579.env
openstack floating ip show $FIP1_ID -f value -c floating_ip_address; probe $FIP1 8080; probe $FIP2 8080
```

**Expect:** after the move, `open ls579-vm2:8080` at once, and both DNAT rules for `$FIP1` point at `$IP2`; after
the disassociation, `timeout` or `unreachable`, `0` NAT rules and `0` addresses; after the deletion, `0` proxy
entries. The new floating IP for vm1 has the same address as `$FIP1` (a floating IP takes the lowest free one), so
**T2** keeps probing vm1; then `open ls579-vm1:8080` and `open ls579-vm2:8080`.

### 2.11 Reconciliation removes what no record holds (FR-010); the probe's result (SC-010)

One rule is added by hand to the router's NAT and one to vm1's ingress chain; neither opens anything.

```bash
source /tmp/ls579.env
nsipt qrouter-$R -t nat -A ls-l3-PREROUTING -d 198.51.100.77/32 -m comment --comment lightstack -j DNAT --to-destination 10.79.0.99
ipt -I ls-i${P1:0:10} -p tcp --dport 65000 -j DROP
nsipt qrouter-$R -t nat -S ls-l3-PREROUTING | grep -c 198.51.100.77; ipt -S ls-i${P1:0:10} | grep -c 65000
sleep 40
nsipt qrouter-$R -t nat -S ls-l3-PREROUTING | grep -c 198.51.100.77; ipt -S ls-i${P1:0:10} | grep -c 65000
probe $FIP1 8080
awk '{print $1}' /tmp/ls579-sc010.log | sort | uniq -c
```

**Expect:** `1` and `1`, then `0` and `0` after one reconciliation pass (30 s); `open ls579-vm1:8080`. The probe's
summary holds `timeout` lines, perhaps a few `unreachable` ones from 2.10, and no `open` and no `refused` line
(SC-010). Then stop **T2** with Ctrl-C.

### 2.12 The refusals (FR-039)

```bash
source /tmp/ls579.env
openstack router remove subnet ls579-r ls579-sub; echo "rc=$?"
openstack router unset --external-gateway ls579-r; echo "rc=$?"
sudo ip netns exec qrouter-$R ip -br addr | grep -cE '^(qg|qr)-'
```

**Expect:** the first command fails with `ConflictException: 409` and "Router interface for subnet … cannot be
deleted, as it is required by one or more floating IPs." (`RouterInterfaceInUseByFloatingIP`), `rc=1`; the second
with `409` and "Gateway cannot be updated for router …, since a gateway to external network … is required by one or
more floating IPs." (`RouterExternalGatewayInUseByFloatingIp`), `rc=1`; both legs are still there (`2`).

### 2.13 The pod restarted, traffic and an established connection flowing (FR-036)

**Shell T3 (stream)** again:

```bash
source /tmp/ls579.env
ssh1 $FIP1 'while true; do date; sleep 1; done'
```

In **T1**:

```bash
source /tmp/ls579.env
sudo stat -c %i /run/netns/qrouter-$R /run/netns/qdhcp-$NET > /tmp/ls579-ns-before
OLD=$(kubectl -n lightstack get pod -l app.kubernetes.io/name=lightstack -o jsonpath='{.items[0].metadata.name}')
kubectl -n lightstack delete pod $OLD
kubectl -n lightstack wait --for=condition=Ready pod -l app.kubernetes.io/name=lightstack --timeout=300s
sudo stat -c %i /run/netns/qrouter-$R /run/netns/qdhcp-$NET | diff - /tmp/ls579-ns-before && echo "namespaces kept"
probe $FIP1 8080
sleep 40
sudo ip netns exec qdhcp-$NET ss -lntu | grep -E '169.254.169.254:80|:67 '
```

**Expect:** **T3** prints a line every second throughout, with no gap and no disconnect; `namespaces kept`;
`open ls579-vm1:8080`; after one pass, the metadata listener and dnsmasq are back in `qdhcp-<NET>`. Stop **T3**
with Ctrl-C.

### 2.14 Readiness follows the bridge netfilter settings (FR-046)

Setting `0` lets bridged traffic bypass every port's chains while it lasts: do this on a test host only.

```bash
source /tmp/ls579.env
curl -s -w ' %{http_code}\n' http://$NODE_IP:35357/readyz
sudo sysctl -qw net.bridge.bridge-nf-call-iptables=0
curl -s -w ' %{http_code}\n' http://$NODE_IP:35357/readyz
sudo sysctl -qw net.bridge.bridge-nf-call-iptables=1
curl -s -w ' %{http_code}\n' http://$NODE_IP:35357/readyz
```

**Expect:** `ok 200`; then `bridge-netfilter unavailable 503`; then `ok 200` again, with no restart.

### 2.15 Horizon's instance *Edit Security Groups* (FR-021, SC-006)

In a browser: `http://<NODE_IP>:30080/dashboard`, user `admin`, password `secret`, domain `Default`. *Project →
Compute → Instances → ls579-vm1 → Edit Security Groups*. Move `ls579-web` out of *Instance Security Groups*,
then *Save*. Then:

```bash
source /tmp/ls579.env
openstack port show $P1 -f json | jq -r '.security_group_ids[]' | sort | tr '\n' ' '; echo
printf '%s\n' $SG_DEFAULT $SG_SSH | sort | tr '\n' ' '; echo
probe $FIP1 8080
```

**Expect:** the dialog opens with exactly `default`, `ls579-ssh` and `ls579-web` selected, no other group; after
saving, the two lines are equal (the port carries `default` and `ls579-ssh`), and `timeout`.

### 2.16 A guest revived by its start: its TAP named, its chains in force (FR-041; SPEC-033 T173)

vm1's domain is destroyed and undefined under Lightstack, as libvirt losing its record of a guest would leave it.
Its start re-creates the guest from what Lightstack stores (SPEC-025's self-heal), binds its port to this node and
names its TAP for the port, `tap<port id[:11]>`, on the network's bridge, so the port's chains match it. A TAP
libvirt named itself, `vnetN`, would match none of them and pass the guest's traffic unfiltered. vm1 carries
`default` and `ls579-ssh` since 2.15, so ssh reaches it and a port nothing listens on is dropped: `timeout`, where
an unfiltered guest would answer `refused`.

```bash
source /tmp/ls579.env
sudo virsh -c qemu:///system destroy instance-${VM1_ID:0:8}
sudo virsh -c qemu:///system undefine instance-${VM1_ID:0:8}
openstack server start ls579-vm1; echo "rc=$?"
for i in $(seq 1 60); do ssh1 $FIP1 true 2>/dev/null && break; sleep 5; done
openstack server show ls579-vm1 -f value -c status
sudo virsh -c qemu:///system domiflist instance-${VM1_ID:0:8} | grep -c "tap${P1:0:11}"
ip -br link show master br-${NET:0:8} | grep -c "tap${P1:0:11}"
ipt -S ls-local | grep -c "tap${P1:0:11}"
ssh1 $FIP1 hostname; probe $FIP1 8083
```

**Expect:** the domain destroyed, then undefined; `rc=0`; `ACTIVE`; `1`: the revived domain's interface is
`tap<P1[:11]>`; `1`: that TAP is on the network's bridge; `3`: the port's rules in `ls-local`; `ls579-vm1`; and
`timeout` for port 8083.

### 2.17 A subnet's gateway change reaches its router (FR-045; SPEC-033 T189)

The external subnet's gateway is the router's next hop. Setting it to another address moves the router's default
route before the answer, and setting it back restores route and egress. The other address is the subnet's nearest
to the pool outside it, neither this host's nor the gateway's; it may be another host's on the LAN, so nothing goes
through the router while it is set.

```bash
source /tmp/ls579.env
NEW_GW=$(python3 -c "import ipaddress as i,sys; n=i.ip_network(sys.argv[1]); e,s,g,h=(i.ip_address(x) for x in sys.argv[2:6]); print(next(a for k in range(1,n.num_addresses) for a in (e+k,s-k) if a in n and a not in (n.network_address,n.broadcast_address,g,h)))" "$EXT_NET" "$POOL_END" "$POOL_START" "$EXT_GW" "$NODE_IP" 2>/dev/null)
if [ -z "$NEW_GW" ]; then
  echo "STOP: $EXT_NET holds no address outside the pool that is neither this host nor the gateway; skip this step"
else
  sudo ip netns exec qrouter-$R ip route show default
  openstack subnet set --gateway $NEW_GW ls579-ext-sub; echo "rc=$?"
  sudo ip netns exec qrouter-$R ip route show default
  openstack subnet set --gateway $EXT_GW ls579-ext-sub; echo "rc=$?"
  sudo ip netns exec qrouter-$R ip route show default
  ssh1 $FIP1 "ping -c 2 -W 2 $EXT_GW"; echo "rc=$?"
fi
```

**Expect:** no `STOP:` line; `default via <EXT_GW> dev qg-…`; `rc=0`; `default via <NEW_GW> dev qg-…`, the address
just set; `rc=0`; `default via <EXT_GW> dev qg-…` again; two replies and `rc=0`: vm1 gets out through the gateway.

### 2.18 A deletion of each kind removes its host state (FR-040, FR-008d, FR-008e)

```bash
source /tmp/ls579.env
openstack floating ip delete $FIP1_ID
nsipt qrouter-$R -t nat -S | grep -cF -- "$FIP1/32"; ip neigh show proxy dev $UPLINK | grep -cwF "$FIP1"
openstack server delete --wait ls579-vm2
openstack floating ip show $FIP2_ID -f json | jq -r '.status, .fixed_ip_address, .port_id'
nsipt qrouter-$R -t nat -S | grep -cF -- "$FIP2/32"; ipt -S | grep -c "ls-o${P2:0:10}"; sudo virsh -c qemu:///system list --all --name | grep -c "instance-${VM2_ID:0:8}"
openstack server delete --force --wait ls579-vm1
ipt -S | grep -c "ls-o${P1:0:10}"; sudo virsh -c qemu:///system list --all --name | grep -c "instance-${VM1_ID:0:8}"
openstack floating ip delete $FIP2_ID
openstack subnet delete ls579-sub; echo "rc=$?"
openstack router remove subnet ls579-r ls579-sub; ip -br link | grep -c '^lr-'
openstack router unset --external-gateway ls579-r; ip -br link | grep -c '^lg-'; ip neigh show proxy dev $UPLINK | grep -cwF "$GW_ADDR"
openstack router delete ls579-r; ip netns list | grep -c "qrouter-$R"
openstack subnet delete ls579-sub; ip netns list | grep -c "qdhcp-$NET"
openstack network delete ls579-net; ip link show br-${NET:0:8} 2>&1 | grep -c 'does not exist'
```

**Expect:** after deleting vm1's associated floating IP, `0` NAT rules and `0` proxy entries; vm2's deletion
leaves its floating IP `DOWN`, `null`, `null`, with `0` NAT rules, `0` chains and `0` domains; after vm1's
force-deletion, `0` chains and `0` domains; the first subnet deletion fails with `409` and "One or more ports have
an IP allocation from this subnet." (`SubnetInUse`: the router's interface uses it), `rc=1`; then `0` `lr-` legs;
`0` `lg-` legs and `0` proxy entries for the gateway; no `qrouter-` namespace (`0`); no `qdhcp-` namespace once
the last subnet is gone (`0`); and `1`: the network's bridge is gone.

---

## 3. Teardown steps

Stop **T2** and **T3** if they still run. Uninstalling Lightstack runs the chart's teardown hooks: the deployment
scaled to zero, then `lightstack cleanup` with the host mounts, which removes the guests, the port firewall, the
routers' and networks' namespaces and legs, the bridges and the node-local trees under `/var/lib/lightstack`; a
failed purge stops the uninstall. Then the block removes the images the run added to k3s, the built image and the
database `ls579`, puts the PostgreSQL container, the host settings and `/var/lib/lightstack` back as 1.1 found
them, checks, and deletes the run's files. It may run again, after a reboot emptied `/tmp` say: what the run's
file no longer says is found again or left alone.

```bash
[ -f /tmp/ls579.env ] && source /tmp/ls579.env
ENGINE=${ENGINE:-$(command -v docker || command -v podman)}
K3S=${K3S:-$(command -v k3s || echo /usr/local/bin/k3s)}
VM1_ID=${VM1_ID:-unset-vm1}; VM2_ID=${VM2_ID:-unset-vm2}
helm uninstall horizon -n lightstack --wait --timeout 10m
helm uninstall lightstack -n lightstack --wait --timeout 15m
kubectl delete namespace lightstack --ignore-not-found
test -s /tmp/ls579-k3s-images-before && sudo $K3S ctr images ls -q | grep -vxF -f /tmp/ls579-k3s-images-before | xargs -r sudo $K3S ctr images rm
[ -n "$ENGINE" ] && $ENGINE rmi docker.io/library/lightstack:spec033
[ -n "$ENGINE" ] && $ENGINE exec lightstack-postgres dropdb -U lightstack --if-exists --force ls579
[ "$DB_STATE" = absent ] && PATH=/tmp/ls579-bin:$PATH make db-down
[ "$DB_STATE" = false ] && [ -n "$ENGINE" ] && $ENGINE stop lightstack-postgres
[ -n "$FWD_BEFORE" ] && sudo sysctl -qw net.ipv4.ip_forward=$FWD_BEFORE
[ "$NF4_BEFORE" = unloaded ] && sudo modprobe -r br_netfilter
[ -n "$NF4_BEFORE" ] && [ "$NF4_BEFORE" != unloaded ] && sudo sysctl -qw net.bridge.bridge-nf-call-iptables=$NF4_BEFORE net.bridge.bridge-nf-call-ip6tables=$NF6_BEFORE
[ "$ROOT_BEFORE" = absent ] && sudo rmdir /var/lib/lightstack
ip netns list | grep -cE 'qrouter-|qdhcp-'; sudo iptables-nft -S | grep -c ' ls-'; sudo virsh -c qemu:///system list --all --name | grep -cE "instance-(${VM1_ID:0:8}|${VM2_ID:0:8})"
rm -rf /tmp/ls579-bin
rm -f /tmp/ls579.env /tmp/ls579-probe.py /tmp/ls579-userdata.sh /tmp/ls579-ns-before /tmp/ls579-sc010.log /tmp/ls579-k3s-images-before /tmp/lightstack-spec033.tar /tmp/ls579.key /tmp/ls579.key.pub /tmp/ls579-cirros.img /tmp/ls579-cirros.img.part
```

**Pass =** both releases uninstalled, and the check line prints `0`, `0` and `0`. The base images and the build
cache the image build pulled stay with the container engine.

---

## Troubleshooting

- **`/readyz` answers `bridge-netfilter unavailable`.** `br_netfilter` is not loaded or a setting is `0`: `sudo
  modprobe br_netfilter` and `sudo sysctl -w net.bridge.bridge-nf-call-iptables=1
  net.bridge.bridge-nf-call-ip6tables=1`. Readiness recovers without a restart.
- **Every new server ends `ERROR`, with no fault, and Lightstack's log says `console socket directory not ready`.**
  libvirt was cold when
  Lightstack started, and it reads libvirt's capabilities only then. Warm it (`sudo virsh -c qemu:///system
  capabilities`) and restart Lightstack: `kubectl -n lightstack delete pod -l app.kubernetes.io/name=lightstack`.
- **An earlier run's guests still run after starting over.** That run was not torn down with its teardown step,
  whose purge finds the guests through the run's database. The new run's reconciliation removes the earlier
  namespaces, bridges and chains, which no record holds, but never a guest. Remove each with `sudo virsh -c
  qemu:///system destroy <name>` and `… undefine <name>`; their files go with the next teardown.
- **`MISSING: /var/run/libvirt/libvirt-sock` on a host whose libvirt runs.** Its libvirt is modular (`virtqemud`)
  without the proxy daemon: `sudo systemctl enable --now virtproxyd.socket` provides the socket, or the symlink in
  `docs/HOST_TESTING_OPENSUSE.md`.
- **`iptables -S` on the host shows none of Lightstack's chains.** The image's `iptables` is the
  nf_tables one; a host whose `iptables` is the legacy one lists other tables. Read them through the pod, as the
  steps do (`ipt`, `nsipt`), or with `sudo iptables-nft -S`.
- **`More than one SecurityGroup exists with the name 'default'`.** An administrator sees every project's
  `default`; use the group's id, as the steps do.
- **A floating IP answers from this host but not from another LAN machine.** The network node answers ARP for it
  on its uplink; an underlay that delivers only assigned addresses (a cloud provider's network) must be told to
  allow the pool for this host.
- **The pings in 2.3 or 2.9 get no reply, everything else passes.** The LAN's gateway may not answer ping.
  Ping another address on the LAN that does.
- **`helm uninstall lightstack` hangs or fails on its hooks.** `kubectl -n lightstack get jobs` and the
  `…-teardown-quiesce` or `…-teardown-purge` job's log name the cause. If `alpine/k8s:1.31.0` cannot be pulled, give
  another image with `/bin/sh` and `kubectl`: `helm upgrade lightstack charts/lightstack -n lightstack
  --reuse-values --set teardown.kubectlImage=<image>`, then uninstall again.
