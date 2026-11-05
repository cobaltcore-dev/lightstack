# HOST_TESTING — openSUSE Tumbleweed

A single runbook to update Lightstack and test it on **openSUSE Tumbleweed**, covering:

- **Part A** — every check the CI pipeline runs (`.github/workflows/ci.yml`), locally.
- **Part B** — real-mode Lightstack on **docker-compose (KVM)**: create a server, push an SSH
  key, log in over SSH, log in over the console, reach the internet.
- **Part C** — real-mode Lightstack on **k3s via Helm**, image storage on **Ceph RGW (S3)**
  served by **Rook on minikube**, with **Horizon** — the same five checks plus "Horizon works".

> This host is **openSUSE Tumbleweed**, not Ubuntu. The in-repo `docs/specs/*/HOST_TESTING*.md`
> runbooks assume Ubuntu (apt-get, monolithic `libvirtd`, AppArmor). This file is the Tumbleweed
> adaptation. The container engine here is **Podman** (with the `docker` shim), libvirt is
> **modular** (`virtqemud`, no `libvirt-sock`), SELinux is **enforcing**, and QEMU is **11.x**.
>
> **Shell blocks are zsh-copy-paste-safe**: no `\` line continuations, no `!` inside double
> quotes, no inline `#` comments. Run every block from the repo root
> (`/home/fnk/code/lightstack/feature/host-os`) unless a block says otherwise.
>
> Two source-level bugs block real mode on this host's QEMU/libvirt. §0.4 lists them and the
> local workarounds Parts B and C depend on. Part A (stub/CI) is unaffected.

---

## §0 — Prerequisites

### 0.0 Post-reboot preflight (openSUSE — run first, every boot)

openSUSE resets several things on each boot that real mode (Parts B/C) and k3s depend on, and this
host kept failing after reboots until they were all set. Run this block **once after every boot**,
before Part B or Part C. It is idempotent — safe to re-run — and it persists firewalld/SELinux so a
future boot starts clean too.

Why each line:
- **firewalld** drops guest DHCP on Lightstack's bridges and blocks egress NAT (`disable --now` also
  stops it coming back next boot).
- **SELinux permissive** — enforcing blocks k3s from exec'ing its binary (`status=203/EXEC`, no
  k3s-selinux policy on openSUSE) and svirt blocks QEMU on the `var_lib_t` state; the `sed` persists
  it, `setenforce 0` applies it now.
- **modular libvirt sockets** must be up (openSUSE has no `libvirt-sock`).
- **`/run/libvirt/libvirt-sock` symlink** — `/run` is tmpfs, so it vanishes on reboot; the Helm chart
  hard-codes that path (Part C).
- **default libvirt network** must be active for minikube's kvm2 driver (Part C).
- **warm libvirt** so the 1 s console-dir capability read at real-mode startup doesn't time out.

**Host (sudo):**
```
sudo systemctl disable --now firewalld
sudo sed -i 's/^SELINUX=enforcing/SELINUX=permissive/' /etc/selinux/config
sudo setenforce 0
sudo systemctl enable --now virtqemud.socket virtnetworkd.socket virtstoraged.socket
sudo ln -sf virtqemud-sock /run/libvirt/libvirt-sock
sudo virsh -c qemu:///system net-start default 2>/dev/null || true
sudo virsh -c qemu:///system capabilities >/dev/null && echo "libvirt warm"
```

Verify (want `firewalld=inactive`, `selinux_enforce=0`, the symlink resolving to `virtqemud-sock`, and
`default` active):
```
echo "firewalld=$(systemctl is-active firewalld)  selinux_enforce=$(cat /sys/fs/selinux/enforce)"
ls -l /run/libvirt/libvirt-sock
sudo virsh -c qemu:///system net-list
```

> **Starting from a clean slate.** To wipe a previous run first: tear down the stacks (Part B → B8,
> Part C → C8), then remove node-local state with `sudo rm -rf /var/lib/lightstack`. The libvirt
> `default`-network renumber to `192.168.151.0/24` (C1) is persistent; if a boot ever shows it back on
> `192.168.122.0/24`, re-run the renumber from C1.

### 0.1 Update the working tree and rebuild

```bash
git fetch
git status -sb
make build
echo "built: $(git describe --tags --always --dirty 2>/dev/null)"
```

Desired outcome: a clean (or expected) status on `feature/host-os`, and a `bin/lightstack` is
produced. The binary has **no `--version` flag**; `make build` embeds the version in its
`-ldflags` line (it also prints at startup), and `git describe` above shows the same string. The
binary's only subcommands are `cleanup`, `healthcheck`, and `console-tokens`.

### 0.2 Detect tooling and print versions

Paste this whole block. It prints each tool's version or `MISSING`, grouped by which Part needs
it. Nothing here installs anything.

```bash
probe() {
  local c="$1"
  if command -v "$c" >/dev/null 2>&1; then
    local v
    v=$("$c" --version 2>/dev/null | head -1)
    test -z "$v" && v=$("$c" version --short 2>/dev/null | head -1)
    test -z "$v" && v=$("$c" version 2>/dev/null | head -1)
    printf '  %-22s %s\n' "$c" "${v:-present}"
  else
    printf '  %-22s %s\n' "$c" "MISSING"
  fi
}
echo "== core (all parts) =="
for c in go make git curl jq; do probe "$c"; done
echo "== container engine + compose (Parts A, B, C) =="
for c in podman docker; do probe "$c"; done
docker compose version >/dev/null 2>&1 && echo "  docker compose          $(docker compose version 2>/dev/null | head -1)" || echo "  docker compose          MISSING (install a compose provider)"
echo "== OpenStack / S3 clients (A seeding, B, C) =="
for c in openstack aws; do probe "$c"; done
echo "== real mode: KVM/libvirt (Parts B, C) =="
for c in virsh qemu-system-x86_64 qemu-img mkisofs dnsmasq virt-viewer; do probe "$c"; done
echo "== kubernetes (Part C) =="
for c in helm kubectl minikube k3s; do probe "$c"; done
echo "== libvirt sockets present =="
ls /run/libvirt/ 2>/dev/null | grep -E 'sock$' | sed 's/^/  /' || echo "  (none)"
echo "== SELinux =="
echo "  enforce=$(cat /sys/fs/selinux/enforce 2>/dev/null || echo n/a)"
```

### 0.3 Install what is missing (openSUSE Tumbleweed)

Install only the rows that printed `MISSING`.

**Core + clients + KVM tooling (zypper):**

```bash
sudo zypper install -y git jq curl make podman podman-docker iptables dnsmasq mkisofs qemu-kvm libvirt libvirt-client virt-viewer
sudo systemctl enable --now virtqemud.socket virtnetworkd.socket virtstoraged.socket
```

Notes:
- The `mkisofs` package provides `mkisofs` (openSUSE's equivalent of Debian's `genisoimage`, which is
  the `cdrkit` fork). Lightstack's config-drive code tries `genisoimage` then **falls back to `mkisofs`
  with identical flags**, so you do **not** need `genisoimage` — a `genisoimage MISSING` line in the
  detector is expected. In real-mode Parts B/C the ISO is built inside the container, which bundles
  `cdrkit`, so host ISO tooling does not matter there at all.
- `podman-docker` provides the `docker` shim so `docker compose` (used by the Makefile, the test
  scripts, and this runbook) resolves to Podman.

**Compose provider** (if `docker compose version` printed MISSING):

```bash
sudo zypper install -y python3-podman-compose || sudo zypper install -y docker-compose
docker compose version
```

**virt-viewer** (Part B console):

```bash
sudo zypper install -y virt-viewer
```

**boto3** (Part A6 only) is **not** installed here — A6 runs its scripts under an ephemeral uv
environment (`uv run --no-project --with boto3 …`, see A6), so a `boto3 MISSING` line in the
detector is expected and fine. If you prefer a persistent install: `sudo zypper install -y
python3-boto3`.

**AWS CLI** (Part C only — the Rook step creates the Glance bucket with `aws s3 mb`; you already
have `openstack` via `uv`, add `aws` the same way or from zypper):

```bash
uv tool install awscli || sudo zypper install -y aws-cli
aws --version
```

**k3s** — do **not** pre-install it here. Part C installs a **fresh k3s at setup (C0)** and removes it
completely at **teardown (C8)**, so every Part C run starts from a clean cluster. A `k3s MISSING` line
in the detector is therefore expected.

`helm`, `kubectl`, `minikube` are already present on this host. The chart needs Helm 3.12+; Helm 4
renders and installs these charts fine.

### 0.4 Known blockers on this host (real mode only)

Parts B and C boot real VMs. One hard-coded value in the current tree still stops that on this
host's modular libvirt. Part A (stub/CI) is **not** affected.

**Fixed in this tree — domain machine type.** `pkg/hypervisor/xml_template.go` previously pinned
`machine='pc-i440fx-2.12'`, which QEMU 11 rejects at define. It now renders the portable `pc` alias
(the running QEMU's current i440fx machine), so real-mode `server create` defines on QEMU 11 with no
workaround. Nothing to do; this note stays so an older checkout's define failure is recognisable.

**Blocker — libvirt socket path.** The binary dials `/var/run/libvirt/libvirt-sock` whenever
`nova.libvirt_uri` is `qemu:///system`. Modular libvirt on this host exposes only
`/run/libvirt/virtqemud-sock`.
- **Part B (compose-kvm)** avoids it cleanly: the config file this runbook writes sets
  `nova.libvirt_uri: "/run/libvirt/virtqemud-sock"`, which the binary uses verbatim. No symlink.
- **Part C (Helm chart)** can't set `libvirt_uri` (the chart hard-codes `qemu:///system` in
  `_helpers.tpl`), so create a compatibility symlink on the host before installing:

  ```bash
  sudo ln -sf virtqemud-sock /run/libvirt/libvirt-sock
  ls -l /run/libvirt/libvirt-sock
  ```

  `/run` is tmpfs — re-create the symlink after a reboot.

**Watch-point — cold libvirt (SPEC-021).** Real-mode startup reads libvirt capabilities once with a
1 s deadline; a freshly socket-activated `virtqemud` can answer slower, after which **every** guest
start is refused. Warm it before starting Lightstack, and grep the log if starts are refused:

```bash
sudo virsh -c qemu:///system capabilities >/dev/null && echo "libvirt warm"
```

**Watch-point — SELinux (svirt).** SELinux is enforcing here. If a guest fails to start or the
console socket can't be created, check for denials:

```bash
sudo dmesg 2>/dev/null | grep -iE 'avc: .*denied' | tail -20
```

For a lab, `sudo setenforce 0` (temporary) rules SELinux in or out; set it back with
`sudo setenforce 1`.

**Real-mode host prep on this openSUSE host (Parts B and C).** Two host security layers must be relaxed
for real-mode guests, both validated 2026-11-02:
- **firewalld** is active and drops guest **DHCP** on Lightstack's custom bridges (the guest boots but
  falls back to an IPv4LL `169.254.x` address, so SSH gives "No route to host") and also blocks the
  **FORWARD/NAT** a guest needs for internet egress. Lightstack programs iptables directly
  (`networking_mode: iptables`), which does not cooperate with firewalld/nftables. Lab fix:
  `sudo systemctl stop firewalld` (targeted alternative: `firewall-cmd --zone=trusted
  --change-interface=br-<netid>` per bridge). Re-enable with `systemctl start firewalld` when done.
- **SELinux** is enforcing; svirt confines QEMU to `svirt_image_t` and blocks it from the
  `var_lib_t`-labelled state under `/var/lib/lightstack` (disk, console.log). Lab fix: `sudo setenforce 0`
  for the duration of real-mode testing (targeted alternative: relabel the state tree
  `svirt_image_t` or set `security_driver="none"` in the host qemu.conf). `setenforce 1` when done.
  (The per-instance-dir **ownership** gap is a separate DAC layer, fixed in code — see §0.4's note.)

The machine-type fix landed in this tree (`machine='pc'`). The remaining libvirt-socket item is left
as the symlink workaround above; I can turn it into a proper commit (a chart-settable `libvirt_uri`
plus a correct default on modular libvirt) if you want it fixed rather than worked around.

---

## Part A — The CI test suite, locally (goal 1)

Mirrors the eight jobs in `.github/workflows/ci.yml`. Pure stub mode — no KVM, no Ceph, no
Kubernetes. Only `dco-check` (the SAP sign-off gate) is omitted; it is a PR-only Git check, not a
test. Run the steps in order; each notes the CI job it reproduces.

Make sure nothing else is bound on `:5432` or the API ports (`35357/8774/9696/8776/9292/8775`)
before starting — `make db-down` and `docker compose ... down -v` from a previous run clear them.

### A1 — Build & lint (`build` job)

```bash
make build
SIZE=$(stat -c %s bin/lightstack)
echo "binary $((SIZE/1048576)) MB"
test "$SIZE" -le 104857600 && echo "size OK" || echo "TOO LARGE"
make lint
```

Desired outcome: binary under 100 MB, `golangci-lint` exits clean.

### A2 — Unit tests on PostgreSQL, then SQLite (`unit-tests` job)

A throwaway Postgres for the DB-backed unit tests (matches the CI service, DB `lightstack_test`):

```bash
podman rm -f ls-test-pg 2>/dev/null || true
podman run -d --name ls-test-pg -e POSTGRES_USER=lightstack -e POSTGRES_PASSWORD=secret -e POSTGRES_DB=lightstack_test -p 5432:5432 postgres:18.4-alpine
sleep 4
LIGHTSTACK_TEST_DB_URL="postgres://lightstack:secret@localhost:5432/lightstack_test?sslmode=disable" go test -race -coverprofile=coverage.txt -covermode=atomic $(go list ./... | grep -v '/test/contract')
```

Then the same suite on SQLite (each package gets its own file beside this path):

```bash
rm -rf /tmp/ls-test && mkdir -p /tmp/ls-test
LIGHTSTACK_TEST_DB_URL="sqlite:///tmp/ls-test/t.db" go test -race -count=1 $(go list ./... | grep -v '/test/contract')
podman rm -f ls-test-pg
```

Desired outcome: both runs green. (If a seeded-project test flakes under parallelism, re-run that
package with `go test -p 1`.)

### A3 — Contract tests on PostgreSQL (`contract-tests` job)

```bash
docker compose -f deployments/docker-compose.yml up -d --build
timeout 120 bash -c 'until curl -sf http://localhost:35357/v3 >/dev/null && curl -sf http://localhost:8774/v2.1 >/dev/null && curl -sf http://localhost:9696/v2.0 >/dev/null && curl -sf http://localhost:8776/v3 >/dev/null && curl -sf http://localhost:9292/v2 >/dev/null; do sleep 3; done'
echo "services ready"
```

```bash
cd test/contract && LIGHTSTACK_DB_URL="postgres://lightstack:secret@localhost:5432/lightstack?sslmode=disable" OS_AUTH_URL="http://localhost:35357/v3" OS_USERNAME=admin OS_PASSWORD=secret OS_PROJECT_NAME=default OS_USER_DOMAIN_NAME=Default OS_PROJECT_DOMAIN_NAME=Default go test ./... -count=1 -timeout=10m 2>&1 | tee /tmp/contract_pg.txt ; cd ../..
```

Desired outcome: the CI gate is a **≥85% pass rate** (milestone 90%, 258/286). Count with
`grep -c '^--- PASS:' /tmp/contract_pg.txt` and `grep -c '^--- FAIL:' /tmp/contract_pg.txt`.

```bash
docker compose -f deployments/docker-compose.yml down -v
```

### A4 — Contract + quick tests on SQLite (`contract-tests-sqlite` job)

The raw binary on a fresh SQLite file (no database container). `qemu-img` and `mkisofs` are present,
so volume creates and cloud-init ISOs work:

```bash
rm -rf /tmp/ls-sqlite && mkdir -p /tmp/ls-sqlite/state /tmp/ls-sqlite/db
sed -e "s|^# state_path: .*|state_path: /tmp/ls-sqlite/state|" config/lightstack.yaml > /tmp/ls-sqlite/lightstack.yaml
LIGHTSTACK_DB_URL="sqlite:///tmp/ls-sqlite/db/lightstack.db" LIGHTSTACK_JWT_SECRET=ci-only LIGHTSTACK_ENDPOINT_HOST=localhost LIGHTSTACK_METADATA_TRUST_INSTANCE_HEADER=true nohup ./bin/lightstack --config /tmp/ls-sqlite/lightstack.yaml --migrations migrations > /tmp/ls-sqlite/server.log 2>&1 &
timeout 60 bash -c 'until ./bin/lightstack healthcheck; do sleep 2; done'
echo "lightstack up on sqlite"
```

```bash
cd test/contract && LIGHTSTACK_DB_URL="sqlite:///tmp/ls-sqlite/db/lightstack.db" OS_AUTH_URL="http://localhost:35357/v3" OS_USERNAME=admin OS_PASSWORD=secret OS_PROJECT_NAME=default OS_USER_DOMAIN_NAME=Default OS_PROJECT_DOMAIN_NAME=Default go test ./... -count=1 -timeout=10m ; cd ../..
OS_AUTH_URL=http://localhost:35357/v3 OS_USERNAME=admin OS_PASSWORD=secret OS_PROJECT_NAME=default OS_USER_DOMAIN_NAME=Default OS_PROJECT_DOMAIN_NAME=Default ./test/quick_test.sh
```

Desired outcome: contract tests pass and `quick_test.sh` passes. Stop the server:

```bash
pkill -f 'bin/lightstack --config /tmp/ls-sqlite' || true
```

> Note: do not `pkill -f bin/lightstack` broadly — on this machine that can also kill an agent
> shell. The pattern above is scoped to this run's config path.

### A5 — Integration tests + Horizon create-UI probe (`integration-tests` job)

```bash
docker compose -f deployments/docker-compose.yml up -d --build
timeout 60 bash -c 'until curl -sf http://localhost:35357/v3 >/dev/null; do sleep 2; done'
OS_AUTH_URL=http://localhost:35357/v3 OS_USERNAME=admin OS_PASSWORD=secret OS_PROJECT_NAME=default OS_USER_DOMAIN_NAME=Default OS_PROJECT_DOMAIN_NAME=Default ./test/quick_test.sh
make test-horizon-ui
docker compose -f deployments/docker-compose.yml down -v
```

Desired outcome: `quick_test.sh` green; `make test-horizon-ui` (SPEC-020 probe) green.

### A6 — Ceph RGW (S3/Swift) integration tests (`rgw-integration-tests` job)

Uses the all-in-one `ceph/demo` fixture (real RGW in a container), **not** Rook — that is Part C.

```bash
docker compose -p lightstack-rgwtest -f deployments/docker-compose.rgw-test.yml up -d --build
timeout 120 bash -c 'until curl -sf http://localhost:45357/v3 >/dev/null; do sleep 3; done'
timeout 240 bash -c 'until docker compose -p lightstack-rgwtest -f deployments/docker-compose.rgw-test.yml logs rgw 2>/dev/null | grep -q "wireup complete"; do sleep 3; done'
echo "rgw wired to keystone"
```

`tenant_s3_rgw_test.sh` needs a `python3` that can `import boto3` (its SigV4 S3 client). Rather than
installing boto3 persistently, run the three scripts under one ephemeral uv environment — `uv run`
prepends the env's `python3` to `PATH`, which the scripts' bare `python3` then picks up (the other two
scripts don't need boto3, but running them under the same env is harmless):

```bash
LIGHTSTACK_TEST_IMAGE_SIZE=256M uv run --no-project --with boto3 -- bash -c './test/rgw_integration_test.sh && ./test/tenant_s3_rgw_test.sh && ./test/swift_rgw_test.sh'
docker compose -p lightstack-rgwtest -f deployments/docker-compose.rgw-test.yml down -v
```

Desired outcome: all three capability scripts pass (Glance-on-RGW, tenant S3 via `/v3/s3tokens`,
Swift via Keystone tokens). (Alternatively install boto3 once with `sudo zypper install -y
python3-boto3` and drop the `uv run` wrapper.)

> **SELinux note (openSUSE/Fedora/RHEL):** the `rgw` container bind-mounts `rgw-keystone-entrypoint.sh`,
> and under enforcing SELinux a non-privileged container can't read a host file labelled `user_home_t`
> — the entrypoint dies `Permission denied` (exit 126), `lightstack` never starts (it waits on `rgw`),
> and `podman compose up -d` hangs. The compose file carries `:ro,z` on that mount so podman relabels it
> automatically; if you ever see the hang, confirm with `podman logs lightstack-rgwtest_rgw_1`, then
> `… down -v` and bring it back up.

### A7 — Helm chart lint / render / unit-test (`helm-check` job)

```bash
helm plugin list 2>/dev/null | grep -q '^unittest' || helm plugin install https://github.com/helm-unittest/helm-unittest --verify=false
make chart-test
```

Desired outcome: both charts (`charts/lightstack`, `charts/horizon`) lint, render, and pass
helm-unittest, and the Horizon bundled-config sync check passes.

---

## Part B — Real mode on docker-compose (KVM) (goal 2)

Boots a **real libvirt/KVM VM** under `deployments/docker-compose-kvm.yml` and checks: server
create, SSH-key injection, SSH login, console login, internet egress. SQLite backend (one file,
no Postgres container).

The machine-type fix is already in this tree (§0.4), so the VM defines on QEMU 11 unchanged. The
libvirt-socket item is handled by the config's `libvirt_uri` override below — no symlink needed here.

> **Run the KVM stack with `sudo` (rootful podman).** Real mode needs real root: neutron `iptables`
> mode creates bridges/TAPs in the **host** network namespace and Lightstack talks to the host libvirt
> socket — rootless podman can do neither. Rootless also maps the container's root to your host user,
> so it cannot write the root-owned `/var/lib/lightstack` state (`permission denied` creating the
> SQLite file). Every `docker compose -f …kvm.yml …` command in this section therefore runs under
> `sudo`. (The rootful image store is separate, so the first `sudo … up --build` re-pulls the base
> layers.)

### B1 — Host prep and config

```bash
sudo virsh -c qemu:///system capabilities >/dev/null && echo "libvirt warm"
sudo mkdir -p /var/lib/lightstack/images /var/lib/lightstack/volumes /var/lib/lightstack/instances /var/lib/lightstack/cloud-init /var/lib/lightstack/dhcp /var/lib/lightstack/db /var/lib/lightstack/console-sockets
sudo mkdir -p /opt/lightstack/config
```

Write the real-mode config (SQLite, real libvirt via the modular socket, iptables neutron, local
storage):

```bash
sudo tee /opt/lightstack/config/lightstack.yaml >/dev/null <<'EOF'
state_path: /var/lib/lightstack
database:
  url: "sqlite:///var/lib/lightstack/db/lightstack.db"
keystone:
  port: 35357
  jwt_secret: "hostos-lab-change-me"
  token_ttl: 24h
  admin_user: admin
  admin_password: secret
nova:
  port: 8774
  libvirt_uri: "/run/libvirt/virtqemud-sock"
  libvirt_mode: real
  default_flavor: m1.small
neutron:
  port: 9696
  networking_mode: iptables
  security_group_mode: iptables
  reconcile_interval: 30s
cinder:
  port: 8776
  storage_mode: local
glance:
  port: 9292
  storage_mode: local
metadata:
  bind_address: "0.0.0.0:8775"
  trust_instance_header: false
logging:
  level: info
  format: json
EOF
echo "config written"
```

> `keystone.token_ttl: 24h` is **required** in a hand-written config. When the key is absent it
> defaults to `0`, which mints tokens whose `exp` equals their `iat` — every request then fails `401
> invalid or expired token` even though `openstack token issue` succeeds (issuing does not validate).
> The shipped `config/lightstack.yaml` sets it; a stripped-down config must keep it.

### B2 — Start the stack

The stack uses a fixed `container_name: lightstack`, so remove any leftover container of that name
first (e.g. from an earlier run or a different spec's testing) — otherwise podman refuses the name and
may try to start the stale one with an out-of-date mount:

```bash
sudo podman rm -f lightstack 2>/dev/null || true
sudo docker compose -f deployments/docker-compose-kvm.yml up -d --build
timeout 120 bash -c 'until curl -sf http://localhost:35357/v3 >/dev/null; do sleep 3; done'
sudo docker compose -f deployments/docker-compose-kvm.yml logs --tail=40 lightstack
sudo docker compose -f deployments/docker-compose-kvm.yml logs lightstack 2>/dev/null | grep -i "console socket directory not ready" && echo "WARN cold-libvirt: restart lightstack after warming" || echo "console dir OK"
```

Desired outcome: Keystone answers; the log shows migrations applied, real hypervisor + iptables neutron
+ local cinder/glance, and **no** "console socket directory not ready". If that warning appears, warm
libvirt (§0.4) and `sudo docker compose -f deployments/docker-compose-kvm.yml restart lightstack`.

### B3 — Authenticate and seed image + keypair + network

Host networking means the API is on loopback.

```bash
export OS_AUTH_URL=http://127.0.0.1:35357/v3
export OS_USERNAME=admin OS_PASSWORD=secret OS_PROJECT_NAME=default
export OS_USER_DOMAIN_NAME=Default OS_PROJECT_DOMAIN_NAME=Default OS_IDENTITY_API_VERSION=3
openstack token issue -f value -c id >/dev/null && echo "auth OK"
```

```bash
CIRROS_URL=https://download.cirros-cloud.net/0.6.2/cirros-0.6.2-x86_64-disk.img
CIRROS_BYTES=21430272
if [ "$(stat -c %s /tmp/cirros.img 2>/dev/null || echo 0)" -eq "$CIRROS_BYTES" ]; then echo "cirros complete"; else curl -fsSL -o /tmp/cirros.img "$CIRROS_URL"; fi
stat -c %s /tmp/cirros.img
openstack image show cirros >/dev/null 2>&1 || openstack image create cirros --disk-format qcow2 --container-format bare --public --file /tmp/cirros.img
openstack image show cirros -c status -f value
```

Push an SSH key (this is the "push ssh keys" check):

```bash
test -f /tmp/hostos_key || ssh-keygen -t ed25519 -f /tmp/hostos_key -N ""
openstack keypair show hostos-key >/dev/null 2>&1 || openstack keypair create --public-key /tmp/hostos_key.pub hostos-key
openstack keypair list -f value -c Name
```

Project network plus an external network and router for internet egress:

```bash
openstack network show int-net >/dev/null 2>&1 || openstack network create int-net
openstack subnet show int-sub >/dev/null 2>&1 || openstack subnet create --network int-net --subnet-range 10.0.0.0/24 int-sub
openstack network show ext-net >/dev/null 2>&1 || openstack network create ext-net --external
openstack subnet show ext-sub >/dev/null 2>&1 || openstack subnet create --network ext-net --subnet-range 172.24.4.0/24 --no-dhcp ext-sub
openstack router show r1 >/dev/null 2>&1 || openstack router create r1
openstack router set r1 --external-gateway ext-net
openstack router add subnet r1 int-sub 2>/dev/null || echo "subnet already attached"
```

Desired outcome: image `active`, keypair `hostos-key` listed, and the networks/router created.

### B4 — Create the server (check: create server)

```bash
openstack server create --flavor m1.small --image cirros --key-name hostos-key --network int-net vm1 --wait
openstack server show vm1 -c status -c power_state -c addresses -c key_name
```

Desired outcome: `status ACTIVE`, `power_state Running`, an address on `10.0.0.0/24`, `key_name`
= `hostos-key`. Confirm a real domain exists:

```bash
sudo virsh -c qemu:///system list --all
```

### B5 — SSH login (check: login via ssh)

The guest's bridge lives in the host network namespace (host networking), so the host reaches it
directly.

```bash
VMIP=$(openstack server show vm1 -f value -c addresses | grep -oE '10\.0\.0\.[0-9]+' | head -1)
echo "vm1 at $VMIP"
ssh -i /tmp/hostos_key -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null cirros@"$VMIP" id
```

Desired outcome: the `id` command runs on the guest — the injected key works.

### B6 — Internet egress (check: access internet)

```bash
ssh -i /tmp/hostos_key -o StrictHostKeyChecking=no cirros@"$VMIP" 'ping -c2 1.1.1.1'
```

Desired outcome: replies. If it fails while routing looks right, add the host NAT allow rules (two
libvirt networks on one host):

```bash
sudo iptables -t nat -A POSTROUTING -s 10.0.0.0/24 ! -d 10.0.0.0/24 -j MASQUERADE
sudo iptables -I FORWARD 1 -s 10.0.0.0/24 -j ACCEPT
```

Re-run the `ping`. (Remove these at teardown.)

### B7 — Console login (check: login via console)

The compose-kvm stack ships **no noVNC proxy** (that lives in the chart — Part C). QEMU still
creates the guest's VNC on a unix socket, so log in with a local VNC viewer against the live
domain:

```bash
sudo virsh -c qemu:///system domdisplay vm1 2>/dev/null || true
sudo virsh -c qemu:///system dumpxml $(openstack server show vm1 -f value -c id) | grep -A2 '<graphics'
virt-viewer -c qemu:///system $(openstack server show vm1 -f value -c id) &
```

Desired outcome: a VNC window opens on the guest framebuffer; log in at the cirros prompt
(user `cirros`, password `gocubsgo` for cirros 0.6.2). Close the viewer window when done. The
full **browser** noVNC console is exercised in Part C via Horizon.

### B8 — Teardown Part B

```bash
openstack server delete vm1 2>/dev/null || true
sudo docker compose -f deployments/docker-compose-kvm.yml down -v
sudo iptables -t nat -D POSTROUTING -s 10.0.0.0/24 ! -d 10.0.0.0/24 -j MASQUERADE 2>/dev/null || true
sudo iptables -D FORWARD -s 10.0.0.0/24 -j ACCEPT 2>/dev/null || true
```

Leave `/var/lib/lightstack` if you want to reuse the SQLite DB; `sudo rm -rf /var/lib/lightstack/*`
for a clean slate.

---

## Part C — k3s + Helm + Ceph RGW (Rook/minikube) + Horizon (goal 3)

Two clusters on one host: **k3s** runs Lightstack (real libvirt/KVM) and Horizon; **minikube**
runs Rook/Ceph with an **RGW (S3)** endpoint that backs Glance. Then the five checks plus "Horizon
works". Builds on the SPEC-018 and SPEC-021 real-mode runbooks, updated for the current chart paths
(`charts/horizon`, `consoleProxy`) and this host.

Three shells, each pinned to a context — each block is labelled with the shell to run it in:
- **T1 — host + k3s**: `export KUBECONFIG=~/.kube/k3s.yaml`; builds images, runs `helm`/`kubectl`
  against k3s, hosts Postgres.
- **T2 — OpenStack CLI**: drives Lightstack over the node IP. No kubeconfig.
- **T3 — Rook/minikube**: the default kubeconfig (`~/.kube/config`, context `rook`).

The machine-type fix is already in this tree (§0.4), so the chart image boots a guest unchanged.
Create the `libvirt-sock` symlink first (the chart can't override `libvirt_uri`) — C0 does this.

### C0 — Fresh k3s, blockers, kubeconfig (T1)

Install a **clean k3s each run**: uninstall any prior k3s first (its uninstaller is idempotent and
only present when k3s is installed), then install. A fresh install wipes k3s's containerd image
store, so the image import in C2 re-runs — that is expected.

**T1 — host:**
```bash
if [ -x /usr/local/bin/k3s-uninstall.sh ]; then sudo /usr/local/bin/k3s-uninstall.sh; fi
curl -sfL https://get.k3s.io | sudo INSTALL_K3S_SELINUX_WARN=true INSTALL_K3S_SKIP_SELINUX_RPM=true sh -
sudo systemctl is-active k3s
```

Then the real-mode blockers (libvirt socket + warm) and a user-readable kubeconfig pinned to T1:

**T1 — host:**
```bash
sudo ln -sf virtqemud-sock /run/libvirt/libvirt-sock
ls -l /run/libvirt/libvirt-sock
sudo virsh -c qemu:///system capabilities >/dev/null && echo "libvirt warm"
mkdir -p ~/.kube
sudo cp /etc/rancher/k3s/k3s.yaml ~/.kube/k3s.yaml
sudo chown "$(id -u):$(id -g)" ~/.kube/k3s.yaml
chmod 600 ~/.kube/k3s.yaml
export KUBECONFIG=~/.kube/k3s.yaml
kubectl get nodes
```

Desired outcome: k3s is `active`, the symlink resolves to `virtqemud-sock`, libvirt is warm, and the
k3s node is `Ready`. Keep `KUBECONFIG` exported in T1 for the rest of Part C.

### C1 — Rook/Ceph + RGW on minikube (T3)

The SPEC-014 fixture stands up Rook Ceph + RGW on a dedicated `rook` minikube profile (kvm2 + raw
OSD disks), exposed on a durable NodePort `30080`. It needs ~8 GB RAM and 4 vCPUs for that profile.

> **If your host LAN is `192.168.122.0/24`** (`ip -br addr show`), the libvirt `default` network can't
> start (`Network is already in use by interface …`) and minikube's kvm2 driver fails. Renumber it
> first, then run the fixture:
> ```
> sudo virsh -c qemu:///system net-dumpxml default > /tmp/defnet.xml
> sed -i 's/192\.168\.122/192.168.151/g' /tmp/defnet.xml
> sudo virsh -c qemu:///system net-destroy default 2>/dev/null || true
> sudo virsh -c qemu:///system net-undefine default
> sudo virsh -c qemu:///system net-define /tmp/defnet.xml
> sudo virsh -c qemu:///system net-start default
> sudo virsh -c qemu:///system net-autostart default
> ```

**T3 — rook (default kubeconfig):**
```bash
unset KUBECONFIG
deployments/rook/setup-rook-rgw.sh --delete 2>/dev/null || true
deployments/rook/setup-rook-rgw.sh
```

Wait for `ROUNDTRIP OK` / `HEALTH_OK` (several minutes on first run). Capture the endpoint and keys
for T1 and create the Glance bucket:

**T3 — rook:**
```bash
ROOK_IP=$(minikube -p rook ip)
RGW_ACCESS=$(kubectl --context rook -n rook-ceph get secret rook-ceph-object-user-my-store-lightstack -o jsonpath='{.data.AccessKey}' | base64 -d)
RGW_SECRET=$(kubectl --context rook -n rook-ceph get secret rook-ceph-object-user-my-store-lightstack -o jsonpath='{.data.SecretKey}' | base64 -d)
install -m 600 /dev/null /tmp/rgw.env
printf 'export RGW_URL=%s\nexport RGW_ACCESS=%s\nexport RGW_SECRET=%s\n' "http://$ROOK_IP:30080" "$RGW_ACCESS" "$RGW_SECRET" > /tmp/rgw.env
source /tmp/rgw.env
echo "RGW_URL=$RGW_URL"
test -n "$RGW_ACCESS" -a -n "$RGW_SECRET" && echo "RGW keys captured" || echo "RGW keys MISSING"
AWS_ACCESS_KEY_ID=$RGW_ACCESS AWS_SECRET_ACCESS_KEY=$RGW_SECRET AWS_DEFAULT_REGION=us-east-1 aws --endpoint-url "$RGW_URL" s3 mb s3://lightstack-images 2>/dev/null || echo "bucket exists"
```

Desired outcome: `/tmp/rgw.env` written (mode 600), keys captured, bucket created or already there.

### C2 — Build the Lightstack image and import into k3s (T1)

k3s uses its own containerd, so hand the image over as a tar. The image is built from the
machine-type-patched tree (C0).

**T1 — host:**
```bash
export KUBECONFIG=~/.kube/k3s.yaml
docker build -t docker.io/library/lightstack:hostos -f build/package/Dockerfile .
rm -f /tmp/lightstack-hostos.tar
docker save docker.io/library/lightstack:hostos -o /tmp/lightstack-hostos.tar
sudo /usr/local/bin/k3s ctr images import /tmp/lightstack-hostos.tar
sudo /usr/local/bin/k3s ctr images ls | grep lightstack:hostos
```

Desired outcome: `docker.io/library/lightstack:hostos` is listed in the k3s image store. (Tag it
fully qualified — Podman prefixes a bare local build with `localhost/`, which the chart's
`image.repository=lightstack` would not resolve.)

### C3 — Postgres + node-local storage (T1)

**T1 — host:**
```bash
make db-up
sudo mkdir -p /var/lib/lightstack/images /var/lib/lightstack/volumes /var/lib/lightstack/instances /var/lib/lightstack/cloud-init /var/lib/lightstack/dhcp /var/lib/lightstack/console-tokens /var/lib/lightstack/console-sockets
```

The real-mode pod is `hostNetwork` with host DNS, so it reaches Postgres at `127.0.0.1:5432`.

### C4 — Install Lightstack (real mode, Glance→RGW, console proxy) (T1)

Create the namespace and the RGW-creds Secret, then render the values file (unquoted heredoc so
`$RGW_URL` and `$NODE_IP` expand). `endpointHost` is the **node IP** so one value works for both the
Horizon pod and the host CLI, and the console URLs carry a reachable origin.

**T1 — host:**
```bash
source /tmp/rgw.env
export NODE_IP=$(hostname -I | awk '{print $1}')
echo "NODE_IP=$NODE_IP  RGW_URL=$RGW_URL"
test -n "$RGW_URL" -a -n "$RGW_ACCESS" || echo "re-run C1 in T3 — RGW values missing"
kubectl create namespace lightstack --dry-run=client -o yaml | kubectl apply -f -
install -m 600 /dev/null /tmp/rgw-creds.env
printf 'accessKey=%s\nsecretKey=%s\n' "$RGW_ACCESS" "$RGW_SECRET" > /tmp/rgw-creds.env
kubectl -n lightstack create secret generic rgw-s3-creds --from-env-file=/tmp/rgw-creds.env --dry-run=client -o yaml | kubectl apply -f -
JWT=$(tr -dc 'a-f0-9' </dev/urandom | head -c 32)
cat > /tmp/lightstack-real-values.yaml <<EOF
image: { repository: lightstack, tag: hostos, pullPolicy: Never }
deploymentMode: single-node
database:
  url: postgres://lightstack:secret@127.0.0.1:5432/lightstack?sslmode=disable
secrets:
  jwtSecret: $JWT
  adminUser: admin
  adminPassword: secret
endpointHost: $NODE_IP
service:
  type: ClusterIP
realMode: { enabled: true, privileged: true, hostNetwork: true }
nova: { libvirtMode: real, consoleProxyBaseUrl: "http://$NODE_IP:30680" }
neutron: { networkingMode: iptables, securityGroupMode: iptables }
cinder: { storageMode: local }
glance:
  storageMode: s3
  s3: { endpoint: "$RGW_URL", bucket: lightstack-images, region: us-east-1 }
consoleProxy: { enabled: true }
extraEnv:
  - { name: AWS_ACCESS_KEY_ID, valueFrom: { secretKeyRef: { name: rgw-s3-creds, key: accessKey } } }
  - { name: AWS_SECRET_ACCESS_KEY, valueFrom: { secretKeyRef: { name: rgw-s3-creds, key: secretKey } } }
EOF
echo "values written"
```

Install and wait:

**T1 — host:**
```bash
helm install lightstack charts/lightstack -n lightstack -f /tmp/lightstack-real-values.yaml
kubectl -n lightstack rollout status deploy/lightstack-lightstack --timeout=300s
kubectl -n lightstack rollout status deploy/lightstack-lightstack-console-proxy --timeout=120s
kubectl -n lightstack get pods
```

Confirm every service is **real** and the RGW endpoint is reachable from the pod:

**T1 — host:**
```bash
kubectl -n lightstack get secret lightstack-lightstack-config -o jsonpath='{.data.lightstack\.yaml}' | base64 -d | grep -E "_mode:|libvirt_mode:"
kubectl -n lightstack logs deploy/lightstack-lightstack | grep -i "console socket directory not ready" && echo "WARN cold-libvirt: kubectl -n lightstack rollout restart deploy/lightstack-lightstack after warming" || echo "console dir OK"
kubectl -n lightstack exec deploy/lightstack-lightstack -- sh -c "wget -qO- $RGW_URL 2>&1 | head -c 120; echo"
```

Desired outcome: `libvirt_mode: "real"`, `networking_mode: "iptables"`,
`security_group_mode: "iptables"`, `storage_mode: "s3"` (glance) and `"local"` (cinder); no
cold-libvirt warning; the `wget` prints S3 XML (`<ListAllMyBucketsResult…>` or
`<Error><Code>AccessDenied`). If the pod can't reach RGW, add the cross-cluster rules:

**T1 — host (only if the wget failed):**
```bash
MK_NET="${ROOK_IP%.*}.0/24"
sudo iptables -I FORWARD 1 -d "$MK_NET" -j ACCEPT
sudo iptables -t nat -A POSTROUTING -d "$MK_NET" ! -s "$MK_NET" -j MASQUERADE
```

### C5 — Install Horizon (T1)

**T1 — host:**
```bash
helm install horizon charts/horizon -n lightstack --set lightstack.host=lightstack-lightstack
kubectl -n lightstack rollout status deploy/horizon-lightstack-horizon --timeout=300s
kubectl -n lightstack get svc horizon-lightstack-horizon
echo "dashboard: http://$NODE_IP:30080/dashboard"
```

Desired outcome: the Horizon pod is Ready and the login page renders at the NodePort.

### C6 — Seed fixtures (T2)

Lightstack is `hostNetwork`, so its API is already on the host. Seed image, keypair, networks,
router, and a second project.

**T2 — openstack:**
```bash
export OS_AUTH_URL=http://$(hostname -I | awk '{print $1}'):35357/v3
export OS_USERNAME=admin OS_PASSWORD=secret OS_PROJECT_NAME=default
export OS_USER_DOMAIN_NAME=Default OS_PROJECT_DOMAIN_NAME=Default OS_IDENTITY_API_VERSION=3
openstack token issue >/dev/null && echo "auth OK"
openstack catalog list -f value -c Name -c Endpoints | grep -E 'compute|network|image|identity|placement'
```

Every catalog URL must carry the node IP (not `localhost`), and placement must appear at `:8778`.

**T2 — openstack:**
```bash
CIRROS_URL=https://download.cirros-cloud.net/0.6.2/cirros-0.6.2-x86_64-disk.img
CIRROS_BYTES=21430272
if [ "$(stat -c %s /tmp/cirros.img 2>/dev/null || echo 0)" -eq "$CIRROS_BYTES" ]; then echo "cirros complete"; else curl -fsSL -o /tmp/cirros.img "$CIRROS_URL"; fi
openstack image show cirros >/dev/null 2>&1 || openstack image create cirros --disk-format qcow2 --container-format bare --public --file /tmp/cirros.img
openstack image show cirros -c status -f value
test -f /tmp/hostos_key || ssh-keygen -t ed25519 -f /tmp/hostos_key -N ""
openstack keypair show hostos-key >/dev/null 2>&1 || openstack keypair create --public-key /tmp/hostos_key.pub hostos-key
openstack network show int-net >/dev/null 2>&1 || openstack network create int-net
openstack subnet show int-sub >/dev/null 2>&1 || openstack subnet create --network int-net --subnet-range 10.0.0.0/24 int-sub
openstack network show ext-net >/dev/null 2>&1 || openstack network create ext-net --external
openstack subnet show ext-sub >/dev/null 2>&1 || openstack subnet create --network ext-net --subnet-range 172.24.4.0/24 --no-dhcp ext-sub
openstack router show r1 >/dev/null 2>&1 || openstack router create r1
openstack router set r1 --external-gateway ext-net
openstack router add subnet r1 int-sub 2>/dev/null || echo "subnet attached"
openstack project show verify-two >/dev/null 2>&1 || openstack project create --domain Default verify-two
openstack role add --project verify-two --user admin admin 2>/dev/null || echo "role present"
openstack project list -f value -c Name
```

Desired outcome: image `active` (proves Glance→RGW works), keypair listed, networks + router
created, and both `default` and `verify-two` projects listed. Optionally confirm the image landed
in RGW (T3): `source /tmp/rgw.env; aws --endpoint-url $RGW_URL s3 ls s3://lightstack-images/images/`.

### C7 — The five checks (T2 + browser)

**Create server (check 1), SSH key already pushed in C6 (check 2):**

**T2 — openstack:**
```bash
openstack server create --flavor m1.small --image cirros --key-name hostos-key --network int-net vm1 --wait
openstack server show vm1 -c status -c power_state -c addresses -c key_name
```

Desired outcome: `ACTIVE`, running, an address on `int-net`, `key_name hostos-key`.

**SSH login (check 3):**

**T2 — openstack:**
```bash
VMIP=$(openstack server show vm1 -f value -c addresses | grep -oE '10\.0\.0\.[0-9]+' | head -1)
ssh -i /tmp/hostos_key -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null cirros@"$VMIP" id
```

**Internet egress (check 4):**

**T2 — openstack:**
```bash
ssh -i /tmp/hostos_key -o StrictHostKeyChecking=no cirros@"$VMIP" 'ping -c2 1.1.1.1'
```

If it fails, add the host NAT rules (same as B6) for `10.0.0.0/24` and re-check.

**Console login (check 5) — the real noVNC path:**

**T2 — openstack:**
```bash
openstack console url show vm1
```

Desired outcome: a URL `http://<NODE_IP>:30680/vnc_lite.html?path=%3Ftoken%3D<32 hex>`. Open it (or
the instance's **Console** tab in Horizon) in a browser — the live guest framebuffer appears; log in
as `cirros` / `gocubsgo`. The serial log also works: `openstack console log show vm1`.

**Horizon works (goal 3's extra check):** open `http://<NODE_IP>:30080/dashboard`, log in
(`admin` / `secret` / `Default`), and walk: Overview renders; the project switcher lists `default`
and `verify-two` and re-scopes; Networks, Volumes, and Instances panels render; the Console tab
shows the guest. Record each verdict.

### C7b — Full lifecycle through the Horizon UI (a second project)

Do the whole workflow in the **browser**, in a fresh project `ui-demo`, to exercise Horizon's create
flows and project isolation (not just the `default` project seeded over the API). Reach the dashboard
as in C5 (node IP, or an SSH tunnel). Log in `admin` / `secret` / `Default`.

Where a known Horizon UI gap blocks a step, a project-scoped CLI fallback is given (run it in the T2
shell with `OS_*` pointed at the node IP); note the finding and carry on.

**1. Create the project (admin).** Identity → Projects → **+ Create Project**: Name `ui-demo`,
Enabled. Create. Then on the `ui-demo` row → **Manage Members** → add `admin` with roles `admin` and
`member` → Save.
Desired: `ui-demo` is listed and `admin` is a member.

> **Fixed defect (2026-11-03):** Create Project used to bounce silently — the button clicked, a brief
> re-render, no modal. Cause: the chart's `local_settings` had `OPENSTACK_KEYSTONE_DEFAULT_ROLE =
> "_member_"`, but Lightstack seeds only `admin`/`member`/`reader`, so adding the admin with the
> default role failed (`GET /dashboard/identity/create` -> 200 + `X-Horizon-Location` redirect, error
> in the `messages` cookie). Fixed to `member` in `deployments/horizon-config/local_settings` +
> `make horizon-chart-sync`. If a Horizon action ever fails silently, decode the `messages` cookie
> (strip leading `.`, urlsafe-base64-decode, zlib-decompress) — the error text is in it.

**2. Switch to it.** Top-left project dropdown → **ui-demo**. Desired: the view re-scopes and the
resource panels are empty (isolation from `default`).

**3. Create a key pair (this is the SSH-key injection).** Project → Compute → **Key Pairs** →
**+ Create Key Pair**: Name `ui-key`, Type `SSH Key`. Create — the browser downloads `ui-key.pem`
(the private key). Keep it; `chmod 600 ui-key.pem` before using it for SSH.
Desired: `ui-key` appears in the list.

**4. Create a network + subnet.** Project → Network → **Networks** → **+ Create Network**:
Network tab Name `ui-net`; Subnet tab Name `ui-sub`, Network Address `10.20.0.0/24`; Subnet Details
leave DHCP enabled. Create.
Desired: `ui-net` with `ui-sub` listed. **If there is no Create Network button** that is finding
**F3** — fall back (T2):
`openstack network create ui-net; openstack subnet create --network ui-net --subnet-range 10.20.0.0/24 ui-sub`
(scope to the project by switching `OS_PROJECT_NAME=ui-demo` and re-issuing a token first).

**5. Create a router for egress.** Project → Network → **Routers** → **+ Create Router**: Name `ui-r`,
External Network `ext-net`. Create. Then open `ui-r` → **Interfaces** → **+ Add Interface** →
Subnet `ui-sub` → Submit.
Desired: `ui-r` has an external gateway (`ext-net`) and an internal interface on `ui-sub`. This is what
gives the guest a route out (step 9). `ext-net` is an external network, so a router in any project may
use it as a gateway.

**6. Upload an image.** Project → Compute → **Images** → **+ Create Image**: Name `cirros-ui`,
Format `QCOW2`, Source `Image File` → upload `/tmp/cirros.img` (copy it to the browser machine first if
the browser is remote), or `URL` →
`https://download.cirros-cloud.net/0.6.2/cirros-0.6.2-x86_64-disk.img`. Visibility Private. Create.
Desired: `cirros-ui` reaches status **Active** (its bytes land in the RGW S3 bucket, same as the CLI
path). A 503 here means the pod lost the RGW endpoint — re-check C4's reachability test.

> **Fixed defect (2026-11-03):** Create Image in the UI threw `TypeError: Cannot read properties of
> undefined (reading 'data')` + "Unable to retrieve namespace". Cause: Glance lacked
> `GET /v2/schemas/metadefs/namespace` (404); the Angular modal read `.data` off the undefined
> response. Fixed by adding the `/v2/schemas/metadefs/*` schema routes (internal/glance); rebuild
> + reimport + `helm upgrade lightstack` to pick it up. CLI `openstack image create` was unaffected.

**7. Launch the instance.** Project → Compute → **Instances** → **Launch Instance**:
Details Name `ui-vm`; Source = Boot from Image, select `cirros-ui`; Flavor `m1.small`; Networks
`ui-net`; Key Pair `ui-key`. Launch.
Desired: `ui-vm` reaches **Active** with a `10.20.0.x` address. **If the Networks step shows no
selectable rows** (findings **F7/F4** — check the browser console for `[ngRepeat:dupes]`), fall back
(T2, project-scoped):
`openstack server create --flavor m1.small --image cirros-ui --key-name ui-key --network ui-net ui-vm --wait`

**8. Console login (noVNC, in the browser).** Instances → `ui-vm` → **Console** (or the Console tab).
The noVNC viewer loads the live framebuffer; click into it, press Enter, log in `cirros` /
`gocubsgo`. Desired: an interactive guest shell in the browser.
**Reachability:** the console URL carries `nova.consoleProxyBaseUrl` (`http://<NODE_IP>:30680`). If
your browser reaches `<NODE_IP>` directly, it works as-is. If you reach Horizon through an SSH tunnel,
also forward the console port and point the origin at localhost — see the note after step 10.

**9. Internet from the guest (in the console).** In the noVNC console shell:
`ping -c3 1.1.1.1`. Desired: replies.

If it's **100% loss** (and `systemctl is-active firewalld` is `inactive`, `ip_forward=1`), the host isn't
masquerading the guest subnet out the uplink: the guest's gateway is the host bridge, so packets leave
`enp1s0` still sourced `10.20.0.x` and the upstream router can't route replies back. Lightstack's router
SNAT lives in a separate `qrouter` netns that isn't in this path, so add a host masquerade (T1, idempotent
— same plumbing as Part B's B6; two libvirt networks on one host, not a Lightstack bug):
```
sudo iptables -t nat -C POSTROUTING -s 10.20.0.0/24 ! -d 10.20.0.0/24 -j MASQUERADE 2>/dev/null || sudo iptables -t nat -A POSTROUTING -s 10.20.0.0/24 ! -d 10.20.0.0/24 -j MASQUERADE
sudo iptables -I FORWARD 1 -s 10.20.0.0/24 -j ACCEPT
sudo iptables -I FORWARD 1 -d 10.20.0.0/24 -j ACCEPT
```
Re-run the guest `ping`. (Remove these at teardown — C8.)

**10. (Optional) SSH with the Horizon-generated key.** From the openSUSE host (which shares the guest
bridge's netns):
`ssh -i /path/to/ui-key.pem -o StrictHostKeyChecking=no cirros@<ui-vm 10.20.0.x>`
Desired: logs in with the key Horizon generated in step 3.

> **Console through an SSH tunnel.** The noVNC URL Lightstack issues is an absolute origin, so a browser
> on a remote workstation must be able to reach it. Forward both ports and set the origin to localhost:
> on the workstation `ssh -L 8088:<NODE_IP>:30080 -L 30680:<NODE_IP>:30680 fnk@<vm>`, then (T1)
> `helm upgrade lightstack charts/lightstack -n lightstack --reuse-values --set nova.consoleProxyBaseUrl=http://localhost:30680`
> and `kubectl -n lightstack rollout status deploy/lightstack-lightstack`. Browse Horizon at
> `http://localhost:8088/dashboard`; the Console tab then targets `localhost:30680`, which the tunnel
> carries to the proxy. (Revert the origin to `http://<NODE_IP>:30680` for node-IP access.)

### C8 — Teardown Part C

Delete the guests **through the API first, while Lightstack is still up** — Lightstack then destroys and
undefines its own libvirt domains and tears down the port/bridge/NAT it created. Uninstalling k3s or
the chart does **not** do this (the chart's default `dataRetentionPolicy: retain` leaves host state
untouched), so skipping this step orphans the VM on the host's libvirt.

**T2 — openstack:**
```bash
openstack server delete vm1 --wait 2>/dev/null || true
```

Then remove the chart release, host Postgres, the cross-cluster NAT rules, and **uninstall k3s
completely** so the next run starts clean:

**T1 — host:**
```bash
export KUBECONFIG=~/.kube/k3s.yaml
helm uninstall horizon lightstack -n lightstack 2>/dev/null || true
kubectl delete namespace lightstack --ignore-not-found
make db-down
MK_NET="${ROOK_IP%.*}.0/24"
sudo iptables -t nat -D POSTROUTING -d "$MK_NET" ! -s "$MK_NET" -j MASQUERADE 2>/dev/null || true
sudo iptables -D FORWARD -d "$MK_NET" -j ACCEPT 2>/dev/null || true
sudo iptables -t nat -D POSTROUTING -s 10.0.0.0/24 ! -d 10.0.0.0/24 -j MASQUERADE 2>/dev/null || true
sudo iptables -D FORWARD -s 10.0.0.0/24 -j ACCEPT 2>/dev/null || true
sudo iptables -t nat -D POSTROUTING -s 10.20.0.0/24 ! -d 10.20.0.0/24 -j MASQUERADE 2>/dev/null || true
sudo iptables -D FORWARD -s 10.20.0.0/24 -j ACCEPT 2>/dev/null || true
sudo iptables -D FORWARD -d 10.20.0.0/24 -j ACCEPT 2>/dev/null || true
sudo /usr/local/bin/k3s-uninstall.sh
```

**T3 — rook:**
```bash
deployments/rook/setup-rook-rgw.sh --delete
```

**Remove the compatibility symlink, and (for a truly fresh run) the node-local state** that lives
outside k3s and survives its uninstall:
```bash
sudo rm -f /run/libvirt/libvirt-sock
sudo rm -rf /var/lib/lightstack
```

If a guest was left behind (Lightstack was already down when you deleted it), check and clean host
libvirt by hand — list with `sudo virsh -c qemu:///system list --all`, then `destroy`/`undefine` the
Lightstack ones; residual bridges/dnsmasq/iptables follow the SPEC-010/SPEC-009 host reset.

---

## Appendix — mapping to CI jobs and source runbooks

| This runbook | CI job / source |
| --- | --- |
| A1 | `build` |
| A2 | `unit-tests` (Postgres + SQLite) |
| A3 | `contract-tests` |
| A4 | `contract-tests-sqlite` |
| A5 | `integration-tests` (+ `make test-horizon-ui`) |
| A6 | `rgw-integration-tests` (ceph/demo fixture) |
| A7 | `helm-check` (`make chart-test`) |
| Part B | `deployments/docker-compose-kvm.yml`, SPEC-009/021 real mode |
| Part C | SPEC-018 `HOST_TESTING_REAL_MODE.md` + SPEC-021 console, updated for `charts/horizon` |

`dco-check` (SAP sign-off) is a PR-only Git check, not reproduced here.
