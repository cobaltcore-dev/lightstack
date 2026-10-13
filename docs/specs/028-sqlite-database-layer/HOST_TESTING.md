# Host testing guide — SQLite database layer (real mode)

This runbook is the real-mode gate of SPEC-028 (T105, quickstart §9): Lightstack in real mode — libvirt
compute, iptables networking, node-local Glance and Cinder — once on a SQLite file and once on
PostgreSQL, with the same steps, and once more on a SQLite file with Lightstack installed by the Helm
chart into a one-node k3s cluster. The stub-mode gates ran on both backends while implementing
(`baseline-gates.md`, *Final gates*): unit tests, contract suite, `quick_test.sh`, the Horizon probe and
the migration round trip.

§2 and §3 run Lightstack from `deployments/docker-compose-kvm.yml`, the repository's real-mode Compose stack,
built from this branch and started as root: privileged, on the host's network, with the host's libvirt
socket directory and the whole state root bind-mounted at their own paths. libvirtd on the host opens
the paths Lightstack writes into each domain's XML, so a path must mean the same inside the container
and outside, and real-mode networking needs the host's network namespace. The stack passes
`LIGHTSTACK_DB_URL` through, so the database URL alone picks the backend (FR-042).

The runbook gives the stack a state root of its own, `/var/lib/lightstack-spec028`, and a config
directory of its own, `/tmp/spec028/config`, through the stack's `LIGHTSTACK_STATE_PATH` and
`LIGHTSTACK_CONFIG_DIR`, and runs it under the Compose project name `spec028`, so nothing mixes with
another installation and `down` touches no other stack. Podman works through `podman-docker` and a
compose provider (`sudo docker compose` runs `podman compose`), and it must run as root: rootless
Podman cannot reach the host's network namespace.

**Run all commands from the repo root, in one shell.** Each section is one zsh-copy-paste-safe block.
§1.1 exports the OpenStack credentials; a new shell needs that block again.

**Validates**

| Criterion | Section |
|---|---|
| A fresh SQLite file is created with its directory `0750` and its files `0600`, and migrated to 074 (FR-008, FR-011, FR-011a) | 2.1 |
| The `database ready` line reports the backend, the settings read back and the node name (FR-013a) | 2.1 |
| The real-mode Compose stack runs either backend, the database URL alone choosing (FR-042) | 2.1, 3 |
| A server boots on a network as a real libvirt domain | 2.2 |
| An image-backed volume is created, bootable, and attaches and detaches | 2.3 |
| A server create over quota answers `403` (FR-035a) | 2.4 |
| A volume create killed mid-copy turns `error` about a minute after the restart, and deleting it removes its partial copy (FR-032, FR-033) | 2.5 |
| The same on PostgreSQL | 3 |
| The same on SQLite with Lightstack installed by the Helm chart on k3s; a container killed mid-copy is restarted by the kubelet and sweeps its reservation under the node's name (FR-043) | 4 |

## 0. Prerequisites

This block installs the two tools the steps call on the host — `curl`, and `sqlite3`, which reads the
database file through the bind mount — and only checks the rest: the OpenStack client, Docker or Podman
with a compose provider, KVM and libvirt. Go, `qemu-img` and `genisoimage` are not needed on the host:
the image builds the binary and carries the tools. Ubuntu socket-activates `libvirtd`, so
`systemctl is-active libvirtd` reading `inactive` is not a fault; the block connects to it instead.

With Podman, every `docker` call prints `Emulate Docker CLI using podman…`, and `docker compose` names
the provider it runs. Both are expected; `sudo touch /etc/containers/nodocker` silences the first.

Stop anything else that listens on Lightstack's ports (a compose stack, another real-mode run).

```bash
sudo apt-get update
sudo apt-get install -y curl sqlite3
command -v openstack >/dev/null || echo "install python-openstackclient first"
command -v docker >/dev/null || echo "install docker, or podman with podman-docker, first"
command -v helm >/dev/null || echo "install helm 3 first, section 4 needs it"
sudo docker compose version >/dev/null 2>&1 || echo "docker compose does not run as root, install a compose provider first"
test -c /dev/kvm || echo "no /dev/kvm, real mode needs KVM"
sudo virsh -c qemu:///system uri >/dev/null || echo "libvirt is not reachable, install libvirt-daemon-system and qemu-system-x86 first"
sudo docker ps -a --format '{{.Names}}' | grep -qx lightstack && echo "a leftover 'lightstack' container exists, run 'sudo docker rm -f lightstack' first"
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:35357/v3
ip -br addr | grep -c '10\.28\.0\.1/'
echo "versions:"; sudo docker --version; sudo docker compose version; openstack --version; sqlite3 --version; sudo virsh -c qemu:///system version
```

**Pass =** every tool prints a version, nothing prints `install … first` (helm only matters for §4),
`does not run as root`, `no /dev/kvm`, `not reachable` or the leftover-`lightstack`-container warning,
the `curl` line prints `000` (nothing listens on the identity port yet), and the count is `0`: no bridge
left by an earlier run holds `10.28.0.1`, the gateway of the test network §2.6 checks is gone. If it
prints `1`, `ip -br addr | grep '10\.28\.0\.1/'` names the bridge, and `sudo ip link del <bridge>` removes it.

## 1. Setup steps

### 1.1 Credentials

`OS_COMPUTE_API_VERSION=2.1` is pinned because the OpenStack client's microversion negotiation fails
on Lightstack's version document otherwise.

```bash
export OS_AUTH_URL=http://127.0.0.1:35357/v3
export OS_USERNAME=admin
export OS_PASSWORD=secret
export OS_PROJECT_NAME=default
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
export OS_IDENTITY_API_VERSION=3
export OS_COMPUTE_API_VERSION=2.1
```

**Pass =** nothing is printed.

### 1.2 Config, image and test data

The block writes the stack's config, `/tmp/spec028/config/lightstack.yaml`: the reference config with
every real mode spelled out, the runbook's state root and a JWT secret of its own. Its `database.url`
is never used — every start passes the URL. Then it builds the stack's image from this branch — the
working tree, uncommitted changes included — and prepares two images: `big.raw`, 2 GiB of random data
whose copy §2.5 interrupts (random, because `qemu-img` copies zeros in no time), and cirros.

The image build takes a few minutes the first time: a rootful Podman keeps its own image store, so
nothing a rootless build pulled is reused.

```bash
mkdir -p /tmp/spec028/config
sed -e 's|^# state_path: .*|state_path: /var/lib/lightstack-spec028|' -e 's|jwt_secret: "change-me-in-production"|jwt_secret: "spec028-host-test"|' -e 's|libvirt_mode: stub|libvirt_mode: real|' -e 's|networking_mode: stub|networking_mode: iptables|' -e 's|security_group_mode: stub|security_group_mode: iptables|' config/lightstack.yaml > /tmp/spec028/config/lightstack.yaml
grep -nE '^state_path|jwt_secret|libvirt_mode|networking_mode|security_group_mode|storage_mode' /tmp/spec028/config/lightstack.yaml
sudo docker compose -p spec028 -f deployments/docker-compose-kvm.yml build
sudo docker images | grep spec028
test -s /tmp/spec028/big.raw || head -c 2G /dev/urandom > /tmp/spec028/big.raw
ls -l /tmp/spec028/big.raw
test -s /tmp/spec028/cirros.img || curl -fsSL -o /tmp/spec028/cirros.img http://download.cirros-cloud.net/0.6.2/cirros-0.6.2-x86_64-disk.img
ls -l /tmp/spec028/cirros.img
```

**Pass =** the `grep` shows `state_path: /var/lib/lightstack-spec028`, `jwt_secret: "spec028-host-test"`,
`libvirt_mode: real`, `networking_mode: iptables`, `security_group_mode: iptables` and
`storage_mode: local` twice; the build ends by tagging the image, and the image list shows
`spec028_lightstack` (`spec028-lightstack` with Compose v2); `big.raw` is 2147483648 bytes; the cirros
image exists.

## 2. Test steps — SQLite

### 2.1 Start on a fresh SQLite file

The block removes the previous container and the whole state root, then creates the root again,
empty: a bind mount needs its directory to exist. The database directory is then the first thing
Lightstack creates in it. A state root missing altogether, whose directories Lightstack must create
`0755` for QEMU, cannot occur under a bind mount; `TestSQLiteOpenCreatesDirectoryAndFile` covers that
case.

```bash
sudo docker compose -p spec028 -f deployments/docker-compose-kvm.yml down
sudo rm -rf /var/lib/lightstack-spec028
sudo install -d -m 0755 /var/lib/lightstack-spec028
sudo env LIGHTSTACK_STATE_PATH=/var/lib/lightstack-spec028 LIGHTSTACK_CONFIG_DIR=/tmp/spec028/config LIGHTSTACK_DB_URL=sqlite:///var/lib/lightstack-spec028/db/lightstack.db docker compose -p spec028 -f deployments/docker-compose-kvm.yml up -d
for i in $(seq 1 60); do curl -sf http://127.0.0.1:35357/readyz >/dev/null && break; sleep 1; done
curl -s -w '\n' http://127.0.0.1:35357/readyz
sudo docker logs lightstack 2>&1 | grep -E '"database ready"|database file mode tightened'
sudo stat -c '%a %n' /var/lib/lightstack-spec028 /var/lib/lightstack-spec028/db /var/lib/lightstack-spec028/db/lightstack.db /var/lib/lightstack-spec028/db/lightstack.db-wal /var/lib/lightstack-spec028/db/lightstack.db-shm
sudo sqlite3 /var/lib/lightstack-spec028/db/lightstack.db 'SELECT version, dirty FROM schema_migrations'
sudo docker logs lightstack 2>&1 | grep -E 'Hypervisor initialized|Neutron initialized|Cinder initialized|Glance initialized|Lightstack started'
```

**Expect:**
- `/readyz` answers `ok`;
- one `database ready` line with `"backend":"sqlite"`, `"location":"/var/lib/lightstack-spec028/db/lightstack.db"`,
  `"schema_version":74`, `"node":"<this host's name>"`, `"journal_mode":"wal"`, `"synchronous":"full"`,
  `"foreign_keys":true`, `"busy_timeout_ms":5000` and `"txlock":"immediate"`; no `tightened` line on a
  fresh file. With host networking the container carries the host's name, so the node name stays the
  same across restarts and recreated containers; the sweep in §2.5 finds its reservation by it;
- `755` for the state root, `750` for the database directory and `600` for the database, `-wal` and
  `-shm` files;
- `74|0` from `schema_migrations`;
- the hypervisor in `real` mode, Neutron in `iptables` mode, Cinder and Glance in `local` mode, and
  `Lightstack started successfully`.

### 2.2 Boot a server on a network

```bash
openstack image show spec028-cirros >/dev/null 2>&1 || openstack image create --disk-format qcow2 --container-format bare --public --file /tmp/spec028/cirros.img spec028-cirros
openstack network show spec028-net >/dev/null 2>&1 || openstack network create spec028-net
openstack subnet show spec028-sub >/dev/null 2>&1 || openstack subnet create --network spec028-net --subnet-range 10.28.0.0/24 spec028-sub
openstack server create --flavor m1.tiny --image spec028-cirros --network spec028-net --wait spec028-vm
openstack server show spec028-vm -f value -c status -c addresses
sudo virsh -c qemu:///system list --name | grep '^instance-'
```

**Expect:** the server is `ACTIVE` with an address in `10.28.0.0/24`, and one `instance-…` libvirt
domain is running.

### 2.3 An image-backed volume, attached and detached

The volume's row is reserved `creating` before the copy and finalised `available` after it, so the
request answers only once the volume is usable, as before.

```bash
openstack volume create --size 1 --image spec028-cirros spec028-vol
openstack volume show spec028-vol -f value -c status -c bootable
sudo ls -l /var/lib/lightstack-spec028/volumes/
openstack server add volume spec028-vm spec028-vol
sleep 3
openstack volume show spec028-vol -f value -c status
openstack server remove volume spec028-vm spec028-vol
sleep 3
openstack volume show spec028-vol -f value -c status
if sudo test -f /var/lib/lightstack-spec028/db/lightstack.db; then sudo sqlite3 /var/lib/lightstack-spec028/db/lightstack.db "SELECT status, reserved_by FROM volumes WHERE name = 'spec028-vol'"; else sudo docker exec spec028-postgres psql -At -U lightstack -d lightstack_spec028 -c "SELECT status, reserved_by FROM volumes WHERE name = 'spec028-vol'"; fi
```

**Expect:** `available` and `true` after the create, a `volume-<id>.qcow2` file under `volumes/`,
`in-use` after the attach, `available` after the detach, and `available|<this host's name>`: the row
was reserved under this host's name. The last line reads it with `sqlite3`, or with `psql` when §3 runs
on PostgreSQL.

### 2.4 A server create over quota answers 403

```bash
PROJECT=$(openstack token issue -f value -c project_id)
openstack quota set --instances "$(openstack server list -f value -c ID | wc -l)" "$PROJECT"
openstack server create --flavor m1.tiny --image spec028-cirros --network spec028-net spec028-over-quota
openstack quota set --instances 10 "$PROJECT"
openstack server list -f value -c Name
```

**Expect:** the create fails with `ForbiddenException: 403: Client Error for url: http://127.0.0.1:8774/v2.1/servers,
Quota exceeded for instances: Requested 1, but already used 1 of 1 instances`
— upstream Nova's message — and the server list names `spec028-vm` only.

### 2.5 Kill Lightstack while a volume is copied from an image, then restart

The block uploads the 2 GiB image, starts the volume create in the background, waits until the copy has
begun — the volume file appears — and kills the container with `SIGKILL`. Lightstack is the
container's first process, so the kernel takes its `qemu-img` and dnsmasq children down with it, as a
crashed node would lose them. The stack's `unless-stopped` restart policy does not restart a killed
container, so the block starts the same container again itself, with the same database URL.

The killed process's last heartbeat is still fresh when the container comes back, and a process
restarted that quickly cannot be told from a live one sharing the node, so the startup sweep leaves
the volume `creating`. The periodic sweep sets it to `error` once that heartbeat is more than 60 s old
— at its first pass, about a minute after the restart — and the block waits for that. As upstream
Cinder's cleanup does, the sweep leaves the partial copy in place; deleting the errored volume
removes it.

An upload that takes longer than 5 s prints `Retriable status code 503`, an effect older than this
feature: the local image store bounds a whole upload by its 5 s metadata timeout, and the client
retries with the rest of the file, so `spec028-big` keeps only what arrived after the first 5 s, which
is still enough for the kill. A faster upload stores the whole file (`size` 2147483648).

```bash
openstack image show spec028-big >/dev/null 2>&1 || openstack image create --disk-format raw --container-format bare --file /tmp/spec028/big.raw spec028-big
sudo ls /var/lib/lightstack-spec028/volumes/ > /tmp/spec028/volumes.before
openstack volume create --size 3 --image spec028-big spec028-killed > /tmp/spec028/killed.out 2>&1 &
for i in $(seq 1 300); do sudo ls /var/lib/lightstack-spec028/volumes/ | grep -vxFf /tmp/spec028/volumes.before | grep -q . && break; sleep 0.1; done
sudo docker kill -s KILL lightstack
sleep 2
NEWFILE=$(sudo ls /var/lib/lightstack-spec028/volumes/ | grep -vxFf /tmp/spec028/volumes.before)
echo "interrupted: $NEWFILE"
pgrep -x qemu-img || echo "no qemu-img left"
sudo docker start lightstack
for i in $(seq 1 60); do curl -sf http://127.0.0.1:35357/readyz >/dev/null && break; sleep 1; done
curl -s -w '\n' http://127.0.0.1:35357/readyz
openstack volume show spec028-killed -f value -c status
for i in $(seq 1 150); do test "$(openstack volume show spec028-killed -f value -c status)" = error && break; sleep 1; done
openstack volume show spec028-killed -f value -c status
sudo docker logs lightstack 2>&1 | grep 'interrupted block-storage creates'
sudo ls /var/lib/lightstack-spec028/volumes/ | grep -c "$NEWFILE"
openstack volume delete spec028-killed
sleep 2
openstack volume list -f value -c Name | grep -c spec028-killed
sudo ls /var/lib/lightstack-spec028/volumes/ | grep -c "$NEWFILE"
```

**Expect:**
- `interrupted:` names one `volume-<id>.qcow2` file — the copy had begun — and `no qemu-img left`;
- after the restart, `/readyz` answers `ok`, and the first status is `creating`: the startup sweep
  left the volume alone (`error` already only if the restart took more than about 30 s);
- the second status, after the wait, is `error`, and the log shows
  `interrupted block-storage creates set to error` with `"volumes":1`;
- the file count is `1`: the errored volume keeps its partial copy;
- deleting it succeeds, and the last two counts are `0`: the volume and its partial copy are gone.

If `interrupted:` is empty the copy finished before the kill: delete `spec028-killed` and rerun the
block. A rerun restarts the same container, whose log keeps the sweep lines of earlier runs; the last
one is this run's.

### 2.6 Clean up the SQLite pass

```bash
openstack server delete --wait spec028-vm
openstack network delete spec028-net
openstack volume delete spec028-vol
openstack image delete spec028-big
ip -br addr | grep -c '10\.28\.0\.1/'
```

**Expect:** each command returns without error, and the count is `0` — the network's bridge and DHCP
server are gone, so the next pass starts on a clean host. Lightstack keeps running; the next pass's
first block stops it.

## 3. Test steps — PostgreSQL

The same steps against a fresh PostgreSQL database. The block removes the SQLite pass's container and
state root and starts PostgreSQL 18.3 in a container of its own: on the host's network but listening on
loopback port 55028 only, so it collides with no PostgreSQL another checkout uses, and started with
`--rm`, so stopping it removes it together with its data. Then it starts the stack on that database's
URL, so §2.2–§2.6 run unchanged. The readiness check goes over TCP: during its first start the image
runs a temporary server on the socket only, and Lightstack must not catch that one. The URL sets
`sslmode=disable`: the migrations connect through `lib/pq`, which insists on TLS unless the URL says
otherwise, and this server has none.

```bash
sudo docker compose -p spec028 -f deployments/docker-compose-kvm.yml down
sudo docker rm -f -v spec028-postgres >/dev/null 2>&1
sudo rm -rf /var/lib/lightstack-spec028
sudo install -d -m 0755 /var/lib/lightstack-spec028
sudo docker run -d --rm --name spec028-postgres --network host -e POSTGRES_DB=lightstack_spec028 -e POSTGRES_USER=lightstack -e POSTGRES_PASSWORD=secret -e PGPORT=55028 docker.io/library/postgres:18.3 postgres -c listen_addresses=127.0.0.1
for i in $(seq 1 60); do sudo docker exec spec028-postgres pg_isready -h 127.0.0.1 -U lightstack -d lightstack_spec028 >/dev/null 2>&1 && break; sleep 1; done
sudo docker exec spec028-postgres pg_isready -h 127.0.0.1 -U lightstack -d lightstack_spec028
sudo env LIGHTSTACK_STATE_PATH=/var/lib/lightstack-spec028 LIGHTSTACK_CONFIG_DIR=/tmp/spec028/config LIGHTSTACK_DB_URL='postgres://lightstack:secret@127.0.0.1:55028/lightstack_spec028?sslmode=disable' docker compose -p spec028 -f deployments/docker-compose-kvm.yml up -d
for i in $(seq 1 60); do curl -sf http://127.0.0.1:35357/readyz >/dev/null && break; sleep 1; done
curl -s -w '\n' http://127.0.0.1:35357/readyz
sudo docker logs lightstack 2>&1 | grep '"database ready"'
```

**Expect:** `pg_isready` reports `accepting connections`, `/readyz` answers `ok`, and the
`database ready` line reports `"backend":"postgres"`, `"location":"127.0.0.1:55028/lightstack_spec028"`
(no user name or password), `"schema_version":74` and the node name, without the SQLite settings.

Then run **§2.2, §2.3, §2.4, §2.5 and §2.6** as written; skip §2.1, which starts the SQLite file. §2.3
finds no SQLite file and reads the row through `psql`, and §2.5 starts the same container again, so
it keeps the PostgreSQL URL.

**Expect:** every **Expect** of §2.2–§2.6 holds on PostgreSQL too.

## 4. Test steps — SQLite on k3s

The same steps once more on a SQLite file, with Lightstack installed by the Helm chart into a one-node
k3s cluster instead of started by Compose. The chart runs it in real mode — privileged, on the host's
network, with the host's libvirt socket directory and the state root (`realMode.hostStoragePath`, here
`/var/lib/lightstack-spec028`) mounted at their own paths — so the database in the state root's `db/`
directory lives on the node. The chart has no SQLite support of its own yet: pinning the pod to its
node, `LIGHTSTACK_DB_BACKEND` and removing `db/` on uninstall are part 2 (cloud-storage#599). A
one-node cluster keeps the pod on the node that holds the file. The chart sets `LIGHTSTACK_NODE_NAME`
to the node's name (FR-043), and §4.3's sweep finds its reservation by it.

§4 uses the files and the image §1.2 makes. The teardown in §5 removes both, so after it, run §1.1 and
§1.2 again first.

### 4.1 Install k3s and import the image

The block stops the Compose stack, whose ports the pod takes over, and leaves §3's PostgreSQL running
for the teardown. It installs k3s unless it is already there and copies the image §1.2 built into
k3s's own containerd, under a fully qualified name — the name the pod requests. k3s's kubeconfig stays
readable by root only, so `kubectl` runs through `sudo`.

```bash
sudo docker compose -p spec028 -f deployments/docker-compose-kvm.yml down
command -v k3s >/dev/null || curl -sfL https://get.k3s.io | sudo sh -
for i in $(seq 1 90); do sudo k3s kubectl get nodes 2>/dev/null | grep -qw Ready && break; sleep 2; done
sudo k3s kubectl get nodes
IMG=$(sudo docker images --format '{{.Repository}}:{{.Tag}}' | grep -m1 -E '(^|/)spec028[_-]lightstack:')
echo "stack image: ${IMG:-none, run section 1.2 first}"
sudo docker tag "$IMG" docker.io/library/lightstack:spec028
sudo rm -f /tmp/spec028/lightstack-image.tar
sudo docker save -o /tmp/spec028/lightstack-image.tar docker.io/library/lightstack:spec028
sudo k3s ctr -n k8s.io images import /tmp/spec028/lightstack-image.tar
sudo k3s ctr -n k8s.io images ls -q | grep 'lightstack:spec028'
```

**Pass =** the node is `Ready`, `stack image:` names the image §1.2 built (`localhost/spec028_lightstack:latest`,
`spec028-lightstack` with Compose v2), and the last line names `docker.io/library/lightstack:spec028`.
If `stack image:` reads `none`, the tag, save and import fail after it: run §1.2, then this block again.

### 4.2 Install Lightstack with a SQLite URL

The block removes a release and a state root an earlier run left, then installs the chart with a
SQLite URL. `helm` runs through `sudo` for the root-only kubeconfig. The chart wants the admin
password set explicitly; the block sets the `secret` §1.1 uses, which only a test cluster may.

```bash
sudo helm --kubeconfig /etc/rancher/k3s/k3s.yaml uninstall spec028 -n spec028 --wait >/dev/null 2>&1
sudo rm -rf /var/lib/lightstack-spec028
sudo helm --kubeconfig /etc/rancher/k3s/k3s.yaml install spec028 charts/lightstack -n spec028 --create-namespace --set image.repository=docker.io/library/lightstack --set image.tag=spec028 --set image.pullPolicy=Never --set database.url=sqlite:///var/lib/lightstack-spec028/db/lightstack.db --set secrets.jwtSecret=spec028-host-test --set secrets.adminPassword=secret --set nova.libvirtMode=real --set neutron.networkingMode=iptables --set neutron.securityGroupMode=iptables --set cinder.storageMode=local --set glance.storageMode=local --set realMode.enabled=true --set realMode.privileged=true --set realMode.hostNetwork=true --set realMode.hostStoragePath=/var/lib/lightstack-spec028 --set consoleProxy.enabled=false
sudo k3s kubectl -n spec028 rollout status deploy/spec028-lightstack --timeout=300s
for i in $(seq 1 60); do curl -sf http://127.0.0.1:35357/readyz >/dev/null && break; sleep 1; done
curl -s -w '\n' http://127.0.0.1:35357/readyz
sudo k3s kubectl -n spec028 logs deploy/spec028-lightstack | grep -E '"database ready"|database file mode tightened'
sudo stat -c '%a %n' /var/lib/lightstack-spec028 /var/lib/lightstack-spec028/db /var/lib/lightstack-spec028/db/lightstack.db /var/lib/lightstack-spec028/db/lightstack.db-wal /var/lib/lightstack-spec028/db/lightstack.db-shm
sudo sqlite3 /var/lib/lightstack-spec028/db/lightstack.db 'SELECT version, dirty FROM schema_migrations'
sudo k3s kubectl -n spec028 logs deploy/spec028-lightstack | grep -E 'Hypervisor initialized|Neutron initialized|Cinder initialized|Glance initialized|Lightstack started'
```

**Expect:**
- the rollout completes and `/readyz` answers `ok`;
- one `database ready` line with `"backend":"sqlite"`, `"location":"/var/lib/lightstack-spec028/db/lightstack.db"`,
  `"schema_version":74`, `"node":"<this node's name>"` and the SQLite settings of §2.1; no `tightened`
  line;
- `755` for the state root, which the kubelet created, `750` for the database directory and `600` for
  the database, `-wal` and `-shm` files;
- `74|0` from `schema_migrations`;
- the hypervisor in `real` mode, Neutron in `iptables` mode, Cinder and Glance in `local` mode, and
  `Lightstack started successfully`.

Then run **§2.2, §2.3 and §2.4** as written, **§4.3** in place of §2.5, and **§2.6**.

### 4.3 Kill Lightstack while a volume is copied from an image, in the cluster

The kubelet restarts a killed container itself, within seconds, so the block notes the interrupted
file before the kill rather than after it, and kills the Lightstack process on the host with
`SIGKILL`. As in §2.5, its `qemu-img` and dnsmasq children die with it. The restarted container runs
in the same pod on the same node. As in §2.5, its startup sweep leaves the volume `creating`, its
periodic sweep sets it to `error` about a minute later, and deleting the volume removes its partial
copy.

```bash
openstack image show spec028-big >/dev/null 2>&1 || openstack image create --disk-format raw --container-format bare --file /tmp/spec028/big.raw spec028-big
sudo ls /var/lib/lightstack-spec028/volumes/ > /tmp/spec028/volumes.before
openstack volume create --size 3 --image spec028-big spec028-killed > /tmp/spec028/killed.out 2>&1 &
for i in $(seq 1 300); do NEWFILE=$(sudo ls /var/lib/lightstack-spec028/volumes/ | grep -vxFf /tmp/spec028/volumes.before); test -n "$NEWFILE" && break; sleep 0.1; done
sudo pkill -KILL -x lightstack
echo "interrupted: $NEWFILE"
sleep 1
pgrep -x qemu-img || echo "no qemu-img left"
for i in $(seq 1 120); do curl -sf http://127.0.0.1:35357/readyz >/dev/null && break; sleep 1; done
curl -s -w '\n' http://127.0.0.1:35357/readyz
sudo k3s kubectl -n spec028 get pods -l app.kubernetes.io/name=lightstack -o jsonpath='{.items[0].status.containerStatuses[0].restartCount}{"\n"}'
openstack volume show spec028-killed -f value -c status
for i in $(seq 1 150); do test "$(openstack volume show spec028-killed -f value -c status)" = error && break; sleep 1; done
openstack volume show spec028-killed -f value -c status
sudo k3s kubectl -n spec028 logs deploy/spec028-lightstack | grep 'interrupted block-storage creates'
sudo ls /var/lib/lightstack-spec028/volumes/ | grep -c "$NEWFILE"
openstack volume delete spec028-killed
sleep 2
openstack volume list -f value -c Name | grep -c spec028-killed
sudo ls /var/lib/lightstack-spec028/volumes/ | grep -c "$NEWFILE"
```

**Expect:**
- `interrupted:` names one `volume-<id>.qcow2` file — the copy had begun — and `no qemu-img left`;
- `/readyz` answers `ok` again, and the restart count is `1`; each rerun adds one;
- the first status is `creating` (`error` already only if the restart took more than about 30 s);
  the second, after the wait, is `error`, and the restarted container's log shows
  `interrupted block-storage creates set to error` with `"volumes":1`;
- the file count is `1`: the errored volume keeps its partial copy;
- deleting it succeeds, and the last two counts are `0`: the volume and its partial copy are gone.

If `interrupted:` is empty the copy finished before the kill: delete `spec028-killed` and rerun the
block.

### 4.4 Clean up the k3s pass

The release keeps `dataRetentionPolicy: retain`, so uninstalling it purges nothing on the node. The
block uninstalls it and deletes its namespace, then runs the cleanup subcommand on this pass's SQLite
database in a one-off container of the Compose stack, as §5 does on PostgreSQL. It removes the image
from k3s and its fully qualified tag from Podman; the state root goes in §5, and k3s stays installed.

```bash
sudo helm --kubeconfig /etc/rancher/k3s/k3s.yaml uninstall spec028 -n spec028 --wait
sudo k3s kubectl delete namespace spec028 --wait
sudo env LIGHTSTACK_STATE_PATH=/var/lib/lightstack-spec028 LIGHTSTACK_CONFIG_DIR=/tmp/spec028/config LIGHTSTACK_DB_URL=sqlite:///var/lib/lightstack-spec028/db/lightstack.db docker compose -p spec028 -f deployments/docker-compose-kvm.yml run --rm -T lightstack /app/lightstack cleanup -config /opt/lightstack/config/lightstack.yaml
sudo k3s ctr -n k8s.io images rm docker.io/library/lightstack:spec028
sudo docker rmi docker.io/library/lightstack:spec028
sudo rm -f /tmp/spec028/lightstack-image.tar
sudo virsh -c qemu:///system list --all --name | grep -c '^instance-'
```

**Pass =** the uninstall and the namespace deletion succeed, the cleanup logs `teardown: complete` and
exits without error, and no `instance-…` domain is left (`0`).

## 5. Teardown

The cleanup subcommand runs in a one-off container of the stack (`compose run`), with the same mounts
and the PostgreSQL URL; §4.4 cleaned up the k3s pass already. Reading the database to enumerate them,
it destroys and undefines the VMs Lightstack created, purges the host network state (bridges, dnsmasq,
iptables rules and chains) and removes the node-local trees under `/var/lib/lightstack-spec028`. It
removes no TAP devices: each deleted server leaves its `tap-…` behind, a defect older than this
feature. `down --rmi local` then removes the stack's container and image, and stopping PostgreSQL
removes its container and data. The image and `/tmp/spec028` are gone afterwards, so a later pass
starts again at §1.2.

```bash
sudo docker compose -p spec028 -f deployments/docker-compose-kvm.yml stop
sudo env LIGHTSTACK_STATE_PATH=/var/lib/lightstack-spec028 LIGHTSTACK_CONFIG_DIR=/tmp/spec028/config LIGHTSTACK_DB_URL='postgres://lightstack:secret@127.0.0.1:55028/lightstack_spec028?sslmode=disable' docker compose -p spec028 -f deployments/docker-compose-kvm.yml run --rm -T lightstack /app/lightstack cleanup -config /opt/lightstack/config/lightstack.yaml
sudo docker compose -p spec028 -f deployments/docker-compose-kvm.yml down --rmi local
sudo docker stop spec028-postgres
sudo rm -rf /var/lib/lightstack-spec028 /tmp/spec028
sudo virsh -c qemu:///system list --all --name | grep -c '^instance-'
```

**Pass =** the cleanup logs `teardown: complete` and exits without error, `down` removes the container
and the image, and no `instance-…` domain is left (`0`).

k3s stays installed. If §4 installed it and nothing else uses it, `sudo /usr/local/bin/k3s-uninstall.sh`
removes it with everything it runs.
