# Host testing guide — real mode: cluster DNS, the libvirt socket and the console directory retry (SPEC-031)

Validates cloud-storage#630 (SPEC-031) by hand: the five host checks FR-021 (a)–(e) asks for, with the chart in
real mode on single-node k3s. It proves SC-001, SC-002, SC-003, SC-004, SC-004a and SC-007 on a real host; the
unit, chart and contract suites cover the rest (`baseline-gates.md`, T041 and T042).

Written for Ubuntu 24.04, whose libvirt is the monolithic `libvirtd` only: its packages ship no `virtqemud`. An
Ubuntu host therefore proves four of the five checks. (b), a host running only the modular daemons, needs a
libvirt that ships them, such as openSUSE Tumbleweed or Fedora. §1 records which daemon the host runs and the
steps use it, so on such a host the same blocks prove (b) in 2.1 and (e) in 2.4. Only §0's package commands
are Ubuntu's.

**Run all commands from the repo root**, in one shell. Paths below are repo-relative (`charts/lightstack`,
`build/package/Dockerfile`); host paths (`/run/libvirt`, `/var/lib/…`, `/tmp/…`, `~/.kube`) are not. `sudo`
is needed throughout: k3s, libvirt's units and the node-local state are root's. Each section is one block.
Every block from §2 on starts by sourcing the helpers §1 writes, so any of them can be re-run on its own.

Assumptions:
- One Ubuntu 24.04 host with KVM (nested is fine), libvirt (`qemu-system-x86` and `libvirt-daemon-system`;
  Ubuntu socket-activates `libvirtd`) and Docker or Podman.
- The chart runs real mode with `privileged` and `hostNetwork`, PostgreSQL inside the cluster behind a
  Service, and `dataRetentionPolicy: delete`. Node-local state goes to a dedicated root,
  `/var/lib/lightstack-spec031`, never `/var/lib/lightstack`.
- §0 installs k3s if it is missing; §3 removes it only then.

**Validates:**

| FR-021 | What the host must show | Proves | Ubuntu 24.04 | Modular host |
|---|---|---|---|---|
| (a) | a database URL naming a Service resolves under host networking: the pod becomes Ready, and the delete-policy uninstall's purge reaches the database | SC-001 | 2.1 | 2.1 |
| (b) | modular daemons only, default values: dials `virtqemud-sock`, a VM boots `ACTIVE`, the uninstall destroys it | SC-002 | — | 2.1 |
| (c) | cold libvirt: a failed first attempt, a later success, Ready, the first VM `ACTIVE`, no restart | SC-004, SC-004a | 2.2 | 2.2 |
| (d) | libvirt unreachable at start: not Ready, a create ends `ERROR` with a fault; sockets back → connects, Ready, VM `ACTIVE`, no restart | SC-004a, SC-007 | 2.3 | 2.3 |
| (e) | monolithic `libvirtd`: dials `libvirt-sock`, a VM boots `ACTIVE` | SC-003 | 2.1 | 2.4 |

On Ubuntu, (c) and (d) run against `libvirtd`, where the ticket's (c) names `virtqemud`. For Lightstack it is
the same path: it dials a socket and does not know which daemon answers.

Record each step's outcome in §4.

---

## 0. Prerequisites

Installs what the steps call, each only when missing: `jq`, `curl` and `virsh` (`libvirt-clients`), the
OpenStack client (`python3-openstackclient`), Helm 3 from its snap, and k3s. Helm comes from the snap's `3`
track: the chart wants Helm 3.12 or later, and the snap's default track is Helm 4.

Before k3s, it turns off an active ufw. On other hosts it also stops an active firewalld and makes an enforcing
SELinux permissive: there is no k3s SELinux policy for openSUSE, and its k3s service fails with
`status=203/EXEC`. Each change leaves a marker, so §3 undoes exactly what this block did and removes k3s only
if this block installed it. Then it gives your user a private (0600) kubeconfig copy, and waits for the node and
for CoreDNS, which step (a) resolves through.

It installs no hypervisor and no container engine: KVM, libvirt and Docker or Podman must already be there.
The block only checks them.

```bash
sudo apt-get update -qq
sudo apt-get install -y jq curl libvirt-clients
command -v openstack >/dev/null || sudo apt-get install -y python3-openstackclient
command -v helm >/dev/null || sudo snap install helm --classic --channel=3/stable
command -v ufw >/dev/null && sudo ufw status | grep -q 'Status: active' && sudo ufw disable && touch /tmp/ls031-ufw-disabled
systemctl is-active --quiet firewalld && sudo systemctl stop firewalld && touch /tmp/ls031-firewalld-stopped
grep -q 1 /sys/fs/selinux/enforce 2>/dev/null && sudo setenforce 0 && touch /tmp/ls031-selinux-relaxed
command -v k3s >/dev/null || { curl -sfL https://get.k3s.io | sudo INSTALL_K3S_SELINUX_WARN=true INSTALL_K3S_SKIP_SELINUX_RPM=true sh - && touch /tmp/ls031-k3s-installed; }
for i in $(seq 1 60); do sudo /usr/local/bin/k3s kubectl get node >/dev/null 2>&1 && break; sleep 2; done
sudo /usr/local/bin/k3s kubectl wait --for=condition=Ready node --all --timeout=180s
mkdir -p "$HOME/.kube"
sudo install -m 600 -o "$(id -un)" -g "$(id -gn)" /etc/rancher/k3s/k3s.yaml "$HOME/.kube/lightstack-spec031.yaml"
KUBECONFIG="$HOME/.kube/lightstack-spec031.yaml" kubectl -n kube-system rollout status deploy/coredns --timeout=180s
echo "== container engine" && { docker --version 2>/dev/null || podman --version 2>/dev/null || echo "MISSING: install docker or podman first"; }
echo "== clients" && helm version --short && kubectl version --client 2>/dev/null | head -1 && jq --version && openstack --version && virsh --version
echo "== KVM and QEMU" && ls -l /dev/kvm; command -v qemu-system-x86_64 || echo "MISSING: qemu-system-x86_64"
echo "== libvirtd.socket virtqemud.socket" && systemctl is-active libvirtd.socket virtqemud.socket | paste -sd' '
echo "== libvirt sockets" && ls -l /run/libvirt/libvirt-sock /run/libvirt/virtqemud-sock 2>&1
echo "== firewall for the run: ufw=$(command -v ufw >/dev/null && sudo ufw status | head -1) firewalld=$(systemctl is-active firewalld) selinux_enforce=$(cat /sys/fs/selinux/enforce 2>/dev/null || echo none)"
echo "== Lightstack ports in use, want none" && ss -ltnH '( sport = :35357 or sport = :8774 or sport = :9696 or sport = :8776 or sport = :9292 or sport = :8775 or sport = :8778 )'
```

**Pass =**
- The node reports `condition met`, and `deployment "coredns" successfully rolled out`.
- A container engine version, and versions for all five clients — Helm `v3.…`.
- `/dev/kvm` and the `qemu-system-x86_64` path are listed.
- On Ubuntu the libvirt line reads `active inactive`, `libvirt-sock` is listed and `virtqemud-sock` says
  `No such file or directory`. A modular host reads `inactive active`, the other way round.
- The firewall line reads `ufw=Status: inactive firewalld=inactive selinux_enforce=none` (`0` on an SELinux host).
- Nothing is listed under the ports line.

If libvirt is missing (both units `inactive`, both sockets absent), run
`sudo apt-get install -y qemu-system-x86 libvirt-daemon-system`, then this block again.

On a modular host, a `libvirt-sock` beside `virtqemud-sock` is the symlink workaround or a running
`virtproxyd` — §1 clears both. A running `libvirtd.socket` there must be stopped by hand first
(`sudo systemctl stop libvirtd.service libvirtd.socket libvirtd-ro.socket libvirtd-admin.socket`), or 2.1 tests
the monolithic daemon instead of the modular one.

---

## 1. Setup steps

One block, which does this in order:
- **Records** which libvirt socket units are active, so §3 restores exactly those. From that record it also takes
  the daemon that serves QEMU: `libvirtd` on Ubuntu, `virtqemud` on a modular host. A re-run keeps the first
  record.
- **Modular hosts only** (both lines do nothing on Ubuntu):
  - stops an active `virtproxyd`, which would provide `libvirt-sock`. §3 starts it again, since it is in the
    record.
  - removes a `libvirt-sock` symlink workaround, for good: the fix retires it, and `/run` is tmpfs anyway.
- **Builds the image** from the working tree, so the change under test is in the image whether or not it is
  committed. It streams the image into k3s's own containerd. The tag is fully qualified, since Podman would
  otherwise prefix `localhost/`, which the chart's `image.repository: lightstack` does not resolve to. Uses
  `sudo docker` if your user cannot reach the Docker daemon, and Podman if there is no Docker. This part takes
  a few minutes.
- **Writes three files:**
  - `/tmp/ls031-host.sh` — the client environment, `NODE_IP`, `LIBVIRT_DAEMON` and the functions below.
    `NODE_IP` is the address of the default route's interface, computed once now, before any Lightstack bridge
    exists. The catalog advertises it (`endpointHost`), and the pod is host-networked, so the API answers on it
    even while the pod is not Ready.
  - `/tmp/ls031-values.yaml` — names the database by Service name only, `pg.ls031.svc`, which resolves only
    through the cluster DNS (a). It keeps `realMode.libvirtSocketPath` at its default, so the binary chooses the
    socket (b–e). `dataRetentionPolicy: delete` makes each uninstall purge the release.
  - `/tmp/ls031-pg.yaml` — a PostgreSQL Deployment and Service. Its readiness probe checks TCP, so it turns
    Ready only once the final server listens, not during the image's init-time restart.

The functions:

| Function | Does |
|---|---|
| `ls_fresh` | recreates namespace `ls031` with a new PostgreSQL, so each step starts on an empty database |
| `ls_install [helm args]` | installs release `ls`, waits until Keystone answers on `NODE_IP`, and prints its HTTP status (`000`: no answer) |
| `ls_readyz` | prints `/readyz`'s body and status |
| `ls_pods` | the Lightstack pod's READY and RESTARTS |
| `ls_log` | the log lines about the libvirt connection and the console socket directory |
| `ls_seed` | the cirros image (downloaded once), network `net031` and its subnet |
| `ls_boot NAME` | creates a server, waits, prints its status and fault |
| `ls_domains` | the host's Lightstack domains (`instance-…`) |
| `ls_uninstall` | the delete-policy uninstall, then `ls_domains` |
| `ls_sockets` | lists `libvirt-sock` and `virtqemud-sock` |
| `ls_cold` | stops `LIBVIRT_DAEMON`'s service, leaving its sockets, and empties the QEMU capabilities cache (2.2) |
| `ls_libvirt_off`, `ls_libvirt_on` | stop `LIBVIRT_DAEMON` with its socket units, and start the socket units again (2.3) |
| `ls_to_monolithic`, `ls_to_modular` | modular hosts: switch libvirt to `libvirtd` and back (2.4) |

```bash
sudo -v
test -f /tmp/ls031-sockets-before || systemctl list-units --type=socket --state=active --plain --no-legend 'virt*' 'libvirtd*' | awk '{print $1}' > /tmp/ls031-sockets-before
LIBVIRT_DAEMON=libvirtd; grep -qx virtqemud.socket /tmp/ls031-sockets-before && LIBVIRT_DAEMON=virtqemud
systemctl is-active --quiet virtproxyd.socket && sudo systemctl stop virtproxyd.service virtproxyd.socket virtproxyd-ro.socket virtproxyd-admin.socket
test -L /run/libvirt/libvirt-sock && sudo rm -f /run/libvirt/libvirt-sock && echo "removed the libvirt-sock symlink workaround"
NODE_IP=$(ip -4 route get 1.1.1.1 | awk '{for (i = 1; i < NF; i++) if ($i == "src") print $(i + 1)}')
if command -v docker >/dev/null; then CE=(docker); docker info >/dev/null 2>&1 || CE=(sudo docker); else CE=(podman); fi
"${CE[@]}" build -t docker.io/library/lightstack:spec031 -f build/package/Dockerfile .
sudo -v
"${CE[@]}" save docker.io/library/lightstack:spec031 | sudo /usr/local/bin/k3s ctr images import -
printf 'export NODE_IP=%s LIBVIRT_DAEMON=%s\n' "$NODE_IP" "$LIBVIRT_DAEMON" > /tmp/ls031-host.sh
cat >> /tmp/ls031-host.sh <<'EOF'
export NS=ls031
export KUBECONFIG=$HOME/.kube/lightstack-spec031.yaml
export OS_AUTH_URL=http://$NODE_IP:35357/v3
export OS_USERNAME=admin OS_PASSWORD=secret OS_PROJECT_NAME=default OS_IDENTITY_API_VERSION=3
export OS_USER_DOMAIN_NAME=Default OS_PROJECT_DOMAIN_NAME=Default
LS_SEL=app.kubernetes.io/name=lightstack,app.kubernetes.io/instance=ls
ls_fresh() {
  kubectl delete namespace "$NS" --ignore-not-found --wait=true
  kubectl create namespace "$NS"
  kubectl apply -f /tmp/ls031-pg.yaml
  kubectl -n "$NS" rollout status deploy/pg --timeout=300s
}
ls_install() {
  helm install ls charts/lightstack -n "$NS" -f /tmp/ls031-values.yaml "$@"
  for i in $(seq 1 150); do curl -s -o /dev/null "http://$NODE_IP:35357/v3" && break; sleep 2; done
  curl -s -o /dev/null -w "Keystone on $NODE_IP, HTTP %{http_code}\n" "http://$NODE_IP:35357/v3"
}
ls_readyz() {
  curl -s -w ' (HTTP %{http_code})\n' "http://$NODE_IP:35357/readyz"
}
ls_pods() {
  kubectl -n "$NS" get pods -l "$LS_SEL" -o 'custom-columns=POD:.metadata.name,READY:.status.containerStatuses[0].ready,RESTARTS:.status.containerStatuses[0].restartCount'
}
ls_log() {
  kubectl -n "$NS" logs deploy/ls-lightstack -c lightstack | grep -E 'Hypervisor started|connected to libvirt|connection attempt failed|connection lost|console socket directory'
}
ls_seed() {
  stat -c %s /tmp/cirros-0.6.2.img 2>/dev/null | grep -qx 21430272 || curl -fsSL -o /tmp/cirros-0.6.2.img https://download.cirros-cloud.net/0.6.2/cirros-0.6.2-x86_64-disk.img
  openstack image create cirros --disk-format qcow2 --container-format bare --public --file /tmp/cirros-0.6.2.img -f value -c status
  openstack network create net031 -f value -c status
  openstack subnet create net031-sub --network net031 --subnet-range 10.31.0.0/24 -f value -c cidr
}
ls_boot() {
  openstack server create "$1" --flavor m1.tiny --image cirros --network net031 --wait -f value -c status
  openstack server show "$1" -f json | jq -r '"status=\(.status) fault=\(.fault.message // "none")"'
}
ls_domains() {
  sudo virsh -c qemu:///system list --all | grep instance- || echo "no Lightstack domains"
}
ls_uninstall() {
  helm uninstall ls -n "$NS" --timeout 10m
  ls_domains
}
ls_sockets() {
  ls -l /run/libvirt/libvirt-sock /run/libvirt/virtqemud-sock 2>&1
}
ls_cold() {
  sudo systemctl stop "$LIBVIRT_DAEMON.service"
  sudo find /var/cache/libvirt/qemu/capabilities -name '*.xml' -delete
  echo "cached capability files left: $(sudo find /var/cache/libvirt/qemu/capabilities -name '*.xml' | wc -l)"
  echo "$LIBVIRT_DAEMON.service=$(systemctl is-active "$LIBVIRT_DAEMON.service") $LIBVIRT_DAEMON.socket=$(systemctl is-active "$LIBVIRT_DAEMON.socket")"
}
ls_libvirt_off() {
  sudo systemctl stop "$LIBVIRT_DAEMON.service" "$LIBVIRT_DAEMON.socket" "$LIBVIRT_DAEMON-ro.socket" "$LIBVIRT_DAEMON-admin.socket"
  ls_sockets
}
ls_libvirt_on() {
  sudo systemctl start "$LIBVIRT_DAEMON.socket" "$LIBVIRT_DAEMON-ro.socket" "$LIBVIRT_DAEMON-admin.socket"
  ls_sockets
}
ls_to_monolithic() {
  : > /tmp/ls031-modular-daemons
  for d in virtqemud virtnetworkd virtnodedevd virtsecretd virtstoraged virtinterfaced virtnwfilterd; do
    systemctl is-active --quiet "$d.socket" && echo "$d" >> /tmp/ls031-modular-daemons
  done
  for d in $(cat /tmp/ls031-modular-daemons); do
    sudo systemctl stop "$d.service" "$d.socket" "$d-ro.socket" "$d-admin.socket"
  done
  sudo systemctl start libvirtd.socket libvirtd-ro.socket libvirtd-admin.socket
  ls_sockets
}
ls_to_modular() {
  sudo systemctl stop libvirtd.service libvirtd.socket libvirtd-ro.socket libvirtd-admin.socket
  for d in $(cat /tmp/ls031-modular-daemons 2>/dev/null); do
    sudo systemctl start "$d.socket" "$d-ro.socket" "$d-admin.socket"
  done
  rm -f /tmp/ls031-modular-daemons
  ls_sockets
}
EOF
cat > /tmp/ls031-values.yaml <<EOF
image:
  repository: lightstack
  tag: spec031
  pullPolicy: Never
deploymentMode: single-node
endpointHost: $NODE_IP
database:
  url: postgres://lightstack:secret@pg.ls031.svc:5432/lightstack
secrets:
  jwtSecret: spec031-hosttest-jwt-secret-0123456789
  adminUser: admin
  adminPassword: secret
realMode:
  enabled: true
  privileged: true
  hostNetwork: true
  hostStoragePath: /var/lib/lightstack-spec031
nova:
  libvirtMode: real
neutron:
  networkingMode: iptables
  securityGroupMode: iptables
cinder:
  storageMode: local
glance:
  storageMode: local
consoleProxy:
  enabled: false
dataRetentionPolicy: delete
teardown:
  kubectlImage: alpine/k8s:1.31.0
EOF
cat > /tmp/ls031-pg.yaml <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: pg
  namespace: ls031
spec:
  selector:
    matchLabels:
      app: pg
  template:
    metadata:
      labels:
        app: pg
    spec:
      containers:
        - name: pg
          image: docker.io/library/postgres:18.3
          env:
            - name: POSTGRES_USER
              value: lightstack
            - name: POSTGRES_PASSWORD
              value: secret
            - name: POSTGRES_DB
              value: lightstack
          ports:
            - containerPort: 5432
          readinessProbe:
            exec:
              command: ["pg_isready", "-h", "127.0.0.1", "-U", "lightstack", "-d", "lightstack"]
            periodSeconds: 2
---
apiVersion: v1
kind: Service
metadata:
  name: pg
  namespace: ls031
spec:
  selector:
    app: pg
  ports:
    - port: 5432
      targetPort: 5432
EOF
source /tmp/ls031-host.sh
echo "== NODE_IP=$NODE_IP LIBVIRT_DAEMON=$LIBVIRT_DAEMON" && ls_sockets
echo "== recorded libvirt sockets:" $(cat /tmp/ls031-sockets-before)
echo "== image" && sudo /usr/local/bin/k3s ctr images ls -q | grep lightstack:spec031
echo "== helpers" && type ls_fresh ls_boot ls_cold ls_libvirt_off ls_to_monolithic
grep -nE 'endpointHost|url:' /tmp/ls031-values.yaml
```

**Pass =**
- `NODE_IP=<the host's address> LIBVIRT_DAEMON=libvirtd` (`virtqemud` on a modular host).
- That daemon's socket is listed, and the other says `No such file or directory`.
- The recorded sockets include `libvirtd.socket`; on a modular host, `virtqemud.socket`.
- `docker.io/library/lightstack:spec031` is listed.
- The five functions are reported as shell functions.
- The values file shows `endpointHost: <that address>` and
  `url: postgres://lightstack:secret@pg.ls031.svc:5432/lightstack`.

---

## 2. Test steps

Run the steps in order. Each starts from a fresh namespace and database and ends with a delete-policy
uninstall, which purges the release from the host, so the next step starts clean. On Ubuntu, 2.4 only prints
that it does not apply.

### 2.1 Cluster DNS and default values on the host's daemon — FR-021 (a), with (e) on Ubuntu and (b) on a modular host

Installs with default values on the host as §1 found it.

The pod reaching Ready proves the first half of (a): the database check passes only once `pg.ls031.svc`
resolves, which takes the cluster DNS. Before the fix a host-networked pod used the node's resolver and never
got there.

The uninstall proves the second half, together with the daemon check: the purge Job finds the guests to destroy
in the database, through the same Service name. With no database it logs `database unreachable` and destroys
nothing, so a VM left behind means the purge could not resolve the name.

```bash
source /tmp/ls031-host.sh
ls_fresh
ls_install
kubectl -n "$NS" rollout status deploy/ls-lightstack --timeout=300s
kubectl -n "$NS" get pods -l "$LS_SEL" -o jsonpath='{.items[0].spec.hostNetwork} {.items[0].spec.dnsPolicy}{"\n"}'
kubectl -n "$NS" exec deploy/ls-lightstack -c lightstack -- cat /etc/resolv.conf
ls_readyz
ls_pods
ls_log
ls_seed
ls_boot vm-default
ls_domains
sudo ls -ln /var/lib/lightstack-spec031/instances
sudo stat -c '%u:%g %a %n' /var/lib/lightstack-spec031/console-sockets
ls_uninstall
```

**Expect:**
- `deployment "ls-lightstack" successfully rolled out` — Ready (a).
- `true ClusterFirstWithHostNet`, and `/etc/resolv.conf` with `nameserver 10.43.0.10` and
  `search ls031.svc.cluster.local svc.cluster.local cluster.local`.
- `ok (HTTP 200)`; the pod `READY true`, `RESTARTS 0`.
- The log has two lines:
  - `connected to libvirt`, with the host daemon's socket: `"socket":"/var/run/libvirt/libvirt-sock"` on
    Ubuntu (e), `"/var/run/libvirt/virtqemud-sock"` on a modular host (b).
  - Then `console socket directory ready; guests can start`, naming
    `"dir":"/var/lib/lightstack-spec031/console-sockets"` and a `qemu_user` uid:gid. On Ubuntu that is
    `libvirt-qemu`'s uid and the `kvm` group's gid (`id libvirt-qemu`).

  A warm daemon shows no failure line here.
- `ls_seed`: `active`, `ACTIVE`, `10.31.0.0/24`. `ls_boot`: `ACTIVE`, then `status=ACTIVE fault=none`. One
  `instance-…  running` domain.
- The instance's directory and `console-sockets` both belong to the `qemu_user` uid:gid from the log, and
  `console-sockets` has mode `750`.
- `release "ls" uninstalled`, then `no Lightstack domains`. The purge reached the database by Service name
  (a) and destroyed the VM: (e) on Ubuntu, (b) on a modular host.

### 2.2 Cold libvirt, forced — FR-021 (c)

`ls_cold` stops the daemon's service — `libvirtd` on Ubuntu, `virtqemud` on a modular host. Its sockets stay,
so the next connection starts it. It also empties the daemon's QEMU capabilities cache, so its first answers
come slowly, as after a fresh boot. Nothing may touch libvirt between this and the install: no `virsh`.

```bash
source /tmp/ls031-host.sh
ls_fresh
ls_cold
ls_install
kubectl -n "$NS" rollout status deploy/ls-lightstack --timeout=300s
ls_log
ls_pods
ls_seed
ls_boot vm-cold
ls_domains
ls_uninstall
```

**Expect:**
- `cached capability files left: 0`; `libvirtd.service=inactive libvirtd.socket=active` on Ubuntu,
  `virtqemud…` on a modular host. systemd may warn that the stopped service can still be activated by its
  sockets — that is the point.
- Rolled out — Ready, with no restart in between.
- The log shows a failed first attempt, then success:
  - a failure — `libvirt connection attempt failed; retrying in the background` (`did not answer within
    1s`), or `console socket directory not ready; guest starts are refused until it is` with
    `reading libvirt capabilities: libvirt did not answer within 1s`, or both;
  - then `connected to libvirt` with the daemon's socket, and `console socket directory ready; guests can start`.
- `READY true`, `RESTARTS 0`; `vm-cold` `ACTIVE` with `fault=none` — the first guest after a cold start, with
  no restart and no warm-up (SC-004); one `instance-…` domain; the uninstall ends with
  `no Lightstack domains`.
- If the log has no failure line, this host answered even the cold read within 1 s: record
  **retry not exercised** in §4. `TestPreparationRetriesUntilLibvirtAnswers` remains the proof (FR-021 c).

### 2.3 libvirt unreachable at start, forced — FR-021 (d)

`ls_libvirt_off` stops the daemon's socket units, which remove the socket (`RemoveOnStop=yes`), and then the
block installs. The pod stays up but not Ready, so the Service has no endpoint. The client talks to the pod on
the node's own address instead, which host networking allows. `ls_libvirt_on` then starts the socket units
again, and the block watches Lightstack connect by itself.

The block runs straight through. The install raises the startup probe's budget from 30 to 120 tries, 5 s apart.
The step keeps the pod unready on purpose, and a kubelet restart in the middle would hide whether Lightstack
recovers on its own.

```bash
source /tmp/ls031-host.sh
ls_fresh
ls_libvirt_off
ls_install --set probes.startup.failureThreshold=120
sleep 15
ls_readyz
ls_pods
ls_seed
ls_boot vm-unreachable
ls_log
echo "== starting the $LIBVIRT_DAEMON sockets again"
ls_libvirt_on
kubectl -n "$NS" wait --for=condition=Ready pod -l "$LS_SEL" --timeout=180s
ls_log
ls_readyz
ls_pods
ls_boot vm-reconnected
ls_domains
ls_uninstall
```

**Expect** (before the sockets come back):
- Both sockets: `No such file or directory`.
- `hypervisor unavailable (HTTP 503)`; the pod `READY false`, `RESTARTS 0`.
- `ls_seed` works — the API needs no libvirt. In `ls_boot vm-unreachable` the create is accepted and `--wait`
  reports the server in error. Then `status=ERROR fault=the hypervisor is not ready to start guests` — never
  `ACTIVE` (SC-007).
- The log, in order:
  - `Hypervisor started in real mode; connecting to libvirt in the background`;
  - one WARN `libvirt connection attempt failed; retrying in the background` with
    `no libvirt socket: neither /var/run/libvirt/virtqemud-sock nor /var/run/libvirt/libvirt-sock exists`
    (later attempts log at DEBUG only);
  - one ERROR `console socket directory not ready; guest starts are refused until it is`
    (`not connected to libvirt`), from `vm-unreachable`'s start.

**Expect** (after):
- The daemon's socket is listed again: `libvirt-sock` on Ubuntu.
- `condition met` within a few seconds — the loop retries every 1–5 s.
- The log adds `connected to libvirt` with that socket, and `console socket directory ready; guests can start`.
- `ok (HTTP 200)`; `READY true`, `RESTARTS 0` — no restart (SC-004a).
- `vm-reconnected`: `ACTIVE`, `status=ACTIVE fault=none`. One `instance-…` domain, `vm-reconnected`'s;
  `vm-unreachable` never had one. The uninstall ends with `no Lightstack domains`.

### 2.4 Monolithic libvirtd on a modular host — FR-021 (e) there

**On a modular host only.** The block stops every active modular driver daemon with its sockets and starts
`libvirtd.socket`. It installs with default values, then switches back. On Ubuntu, 2.1 has already tested
`libvirtd`, and the block only prints that it skipped.

```bash
source /tmp/ls031-host.sh
if [ "$LIBVIRT_DAEMON" = virtqemud ]; then
  ls_fresh
  ls_to_monolithic
  ls_install
  kubectl -n "$NS" rollout status deploy/ls-lightstack --timeout=300s
  ls_log
  ls_pods
  ls_seed
  ls_boot vm-monolithic
  ls_domains
  ls_uninstall
  ls_to_modular
else
  echo "skipped: this host runs libvirtd, which 2.1 tested; (b) needs a host with the modular daemons"
fi
```

**Expect** on a modular host:
- After the switch, `libvirt-sock` is listed and `virtqemud-sock` says `No such file or directory`.
- Rolled out — Ready. The log: `connected to libvirt` with `"socket":"/var/run/libvirt/libvirt-sock"`, then
  `console socket directory ready; guests can start` (SC-003).
- `READY true`, `RESTARTS 0`; `vm-monolithic` `ACTIVE`, `fault=none`; one `instance-…` domain, listed through
  `libvirtd`; the uninstall ends with `no Lightstack domains`.
- After the switch back, `virtqemud-sock` is listed again and `libvirt-sock` says
  `No such file or directory`.

On Ubuntu: `skipped: …`. Record (b) as not possible on this host in §4.

---

## 3. Teardown steps

Removes what this run created and puts back what it changed. It goes in this order:
1. **libvirt first**, so the release's purge can reach it after an interrupted 2.3 or 2.4: any leftover switch
   to `libvirtd`, then the socket units §1 recorded.
2. The release and its namespace.
3. k3s, only if §0 installed it.
4. ufw, firewalld and SELinux, only if §0 changed them.
5. The test state root, the image and the temporary files.

It reads the kubeconfig copy directly and touches the cluster only while that copy exists, so it cannot act on
another cluster. Safe to re-run.

```bash
test -f /tmp/ls031-modular-daemons && sudo systemctl stop libvirtd.service libvirtd.socket libvirtd-ro.socket libvirtd-admin.socket
test -f /tmp/ls031-sockets-before && sudo systemctl start $(cat /tmp/ls031-sockets-before)
export KUBECONFIG=$HOME/.kube/lightstack-spec031.yaml
test -f "$KUBECONFIG" && helm uninstall ls -n ls031 --timeout 10m
test -f "$KUBECONFIG" && kubectl delete namespace ls031 --ignore-not-found --wait=true
sudo virsh -c qemu:///system list --all | grep instance- || echo "no Lightstack domains"
test -f /tmp/ls031-k3s-installed && sudo /usr/local/bin/k3s-uninstall.sh
test -f /tmp/ls031-ufw-disabled && sudo ufw --force enable
test -f /tmp/ls031-firewalld-stopped && sudo systemctl start firewalld
test -f /tmp/ls031-selinux-relaxed && sudo setenforce 1
sudo rm -rf /var/lib/lightstack-spec031
if command -v docker >/dev/null; then CE=(docker); docker info >/dev/null 2>&1 || CE=(sudo docker); else CE=(podman); fi
"${CE[@]}" rmi docker.io/library/lightstack:spec031 2>/dev/null || true
rm -f /tmp/ls031-host.sh /tmp/ls031-values.yaml /tmp/ls031-pg.yaml /tmp/ls031-sockets-before /tmp/ls031-modular-daemons /tmp/ls031-ufw-disabled /tmp/ls031-firewalld-stopped /tmp/ls031-selinux-relaxed /tmp/ls031-k3s-installed /tmp/cirros-0.6.2.img "$HOME/.kube/lightstack-spec031.yaml"
echo "ufw=$(command -v ufw >/dev/null && sudo ufw status | head -1) firewalld=$(systemctl is-active firewalld) selinux_enforce=$(cat /sys/fs/selinux/enforce 2>/dev/null || echo none)"
ls -l /run/libvirt/libvirt-sock /run/libvirt/virtqemud-sock 2>&1
echo "teardown done"
```

**Pass =**
- `teardown done`, and `no Lightstack domains`.
- ufw, firewalld and SELinux are as they were before §0.
- The daemon's socket is listed — `libvirt-sock` on Ubuntu, `virtqemud-sock` alone on a modular host.
- `/var/lib/lightstack-spec031` is gone.
- If §0 installed k3s, `command -v k3s` now finds nothing. Its uninstaller also removes the imported image.

Not put back:
- On a modular host, the `libvirt-sock` symlink, if §1 removed one. The fix makes it unnecessary, and a reboot
  clears `/run` anyway.
- On a modular host where `virtproxyd.socket` was enabled only to provide `libvirt-sock`, it is no longer
  needed either: `sudo systemctl disable --now virtproxyd.socket`. This runbook restarts it as it found it.
- The clients §0 installed, if you want them gone: `sudo snap remove helm`,
  `sudo apt-get remove python3-openstackclient`, and `jq` if it was not there before.

---

## 4. Results

Note the host and the `LIBVIRT_DAEMON` §1 printed. Then fill in one row per check: **pass**, **fail** (with what
was seen), **retry not exercised** for (c), or **not possible on this host** for (b) on Ubuntu.

Host: ______ `LIBVIRT_DAEMON`: ______

| FR-021 | Section | Outcome | Evidence: the log lines, statuses and domains seen |
|---|---|---|---|
| (a) cluster DNS | 2.1 | | |
| (b) modular host, defaults | 2.1 on a modular host | | |
| (c) cold libvirt | 2.2 | | |
| (d) unreachable libvirt | 2.3 | | |
| (e) monolithic libvirtd | 2.1 on Ubuntu, 2.4 on a modular host | | |

---

## Troubleshooting

- **k3s does not start** — `sudo journalctl -u k3s -n 50`. On an SELinux host, `status=203/EXEC` means
  SELinux is enforcing. §0 makes it permissive for the run; check with `cat /sys/fs/selinux/enforce`.
- **`ls_install` prints `HTTP 000`, or the pod restarts with `Failed to connect to database`** — in 2.1 this is
  the failure step (a) tests. Check that the pod's `/etc/resolv.conf` names `10.43.0.10` and the
  `ls031.svc.cluster.local` search domain, and that `kubectl -n ls031 get pods -l app=pg` is Ready. The log is
  in `kubectl -n ls031 logs deploy/ls-lightstack -c lightstack --previous`.
- **`ImagePullBackOff`** — for `lightstack`, §1 did not import the image, or the tag differs: run
  `sudo /usr/local/bin/k3s ctr images ls -q | grep lightstack`. For `postgres` or `alpine/k8s`, the host
  cannot reach Docker Hub.
- **`helm uninstall` times out** — look at the purge Job: `kubectl -n ls031 get jobs` and
  `kubectl -n ls031 logs job/ls-lightstack-teardown-purge`. The purge makes one 1-second attempt to reach
  libvirt and fails closed (FR-020a). If 2.3 was interrupted with the sockets stopped, start them and uninstall
  again: `source /tmp/ls031-host.sh && ls_libvirt_on && ls_uninstall`.
- **A server ends `ERROR` while libvirt is up** — its fault says which step failed:
  - `the image could not be made available on this host` or `the root disk could not be staged`: disk staging
    failed before libvirt was asked.
  - `the hypervisor could not create the guest`: libvirt refused the domain. Look at
    `sudo journalctl -u libvirtd -n 50` (`-u virtqemud` on a modular host), and at `sudo dmesg | grep -i denied`
    for an AppArmor (Ubuntu) or SELinux denial naming the path QEMU could not open.
- **2.3 shows `RESTARTS 1`** — the startup probe ran out of tries: the block was held up for longer than
  120 × 5 s. Run 2.3 again.
- **2.2 shows no failure line** — the host answered the cold read within 1 s. Record
  **retry not exercised** (FR-021 c).
- **Domains left behind after an interrupted run** — list them with
  `sudo virsh -c qemu:///system list --all`, then for each `instance-…`:
  `sudo virsh -c qemu:///system destroy <name>` and `sudo virsh -c qemu:///system undefine <name>`.
