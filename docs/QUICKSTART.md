# Quickstart — Installing lightstack

Get a working OpenStack-compatible cloud (Keystone, Nova, Neutron, Cinder, Glance —
plus a Placement stub and the EC2 metadata service) in one binary or one container.

**Time to first VM:** ~10 minutes with Docker.

---

## Choose your install path

| Path | Use case | Time | Guide |
|------|----------|------|-------|
| **A. Docker Compose** (stub mode) | Evaluation, API/CLI/SDK testing, CI | ~5 min | this page |
| **B. Docker Compose + Horizon** | Browser dashboard demo | ~10 min | this page |
| **C. Binary from source** | Development, macOS | ~10 min | this page |
| **D. Helm chart (Kubernetes)** | Cluster deployment | — | [KUBERNETES_DEPLOYMENT.md](KUBERNETES_DEPLOYMENT.md) |
| **E. Single node with real KVM** | Production demo / POC with real VMs | 15–20 min | [SINGLE_NODE_DEPLOYMENT.md](SINGLE_NODE_DEPLOYMENT.md) |

All paths default to **stub mode** (unless stated otherwise): resources are tracked in
PostgreSQL, no real VMs/networks are created. Stub mode is safe on any OS, including
macOS and non-Linux hosts. See [NETWORKING_MODES.md](NETWORKING_MODES.md) and
[STORAGE_MODES.md](STORAGE_MODES.md) for real-mode configuration.

---

## Prerequisites

- **Docker** (with Compose v2) — for paths A and B
- **Go 1.27+** — for path C only
- **OpenStack CLI** (optional but recommended):

```bash
# macOS / Linux
brew install pipx && pipx install python-openstackclient
# or with pip
pip install python-openstackclient
```

---

## Path A — Docker Compose (recommended for evaluation)

### 1. Clone and start

```bash
git clone https://github.com/cobaltcore-dev/lightstack.git
cd lightstack
docker compose -f deployments/docker-compose.yml up -d
```

> **Note:** the compose file lives in `deployments/` — run the command from the repo
> root with `-f deployments/docker-compose.yml` (or `cd deployments` first).

> **Security note:** this evaluation stack also publishes port **8775** — the
> EC2-style metadata service, which serves **unauthenticated** responses and
> trusts an instance-identifying header (a test-only setting). On a shared
> host, do not expose 8775 beyond localhost.

Three containers start:

```
$ docker compose -f deployments/docker-compose.yml ps
NAME                 STATUS
lightstack           Up (healthy)
lightstack-postgres  Up (healthy)
lightstack-redis     Up (healthy)
```

- `postgres:18.4-alpine` — state database
- `redis:7-alpine` — optional cache
- `lightstack` — all services in one container (built from `build/package/Dockerfile`)

### 2. Configure your OpenStack client

```bash
export OS_AUTH_URL=http://localhost:35357/v3
export OS_PROJECT_NAME=default
export OS_USERNAME=admin
export OS_PASSWORD=secret  # Path A dev stack public default (local testing only)
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default
```

> The domain names are **case-sensitive**: `Default`, not `default`.

### 3. Verify

```bash
openstack token issue
openstack service list
```

You should get a token and a catalog listing the five core services plus Placement.

### 4. Launch your first (stub) VM

```bash
# Create a network with a subnet
openstack network create my-net
openstack subnet create --network my-net --subnet-range 10.0.0.0/24 my-subnet

# Download and upload a test image
curl -LO https://download.cirros-cloud.net/0.6.2/cirros-0.6.2-x86_64-disk.img
openstack image create --file cirros-0.6.2-x86_64-disk.img --disk-format qcow2 cirros

# Boot
openstack server create --flavor m1.small --image cirros --network my-net test-vm
openstack server show test-vm -f json | jq -r '.status'
```

### 5. Stop / clean up

```bash
docker compose -f deployments/docker-compose.yml down          # keep data
docker compose -f deployments/docker-compose.yml down -v       # wipe data too
```

---

## Path B — Docker Compose with the Horizon dashboard

Adds the OpenStack Horizon web UI (Flamingo 2025.2). This compose file **requires
both** a JWT secret and a bootstrap admin password before it will start:

```bash
cd lightstack
export LIGHTSTACK_JWT_SECRET="$(openssl rand -base64 32)"
export LIGHTSTACK_ADMIN_PASSWORD="$(openssl rand -base64 24)"
docker compose -f deployments/docker-compose-horizon.yml up -d
```

Open **http://localhost/dashboard** and log in with:

- **Domain:** `Default`
- **User:** `admin`
- **Password:** `$LIGHTSTACK_ADMIN_PASSWORD` (the value you generated above)

Creating networks, volumes and instances from the dashboard works (verified in stub
and real mode). Known open gaps are tracked in
[compatibility/HORIZON_COMPATIBILITY_REPORT.md](compatibility/HORIZON_COMPATIBILITY_REPORT.md).

Details: [UNIFIED_DEPLOYMENT.md](UNIFIED_DEPLOYMENT.md) ·
[HORIZON_DEPLOYMENT.md](HORIZON_DEPLOYMENT.md)

---

## Path C — Binary from source

```bash
git clone https://github.com/cobaltcore-dev/lightstack.git
cd lightstack

# Start PostgreSQL (18) and apply migrations
make db-up
make migrate   # shells out to the golang-migrate CLI — make install-tools once
               # if it is missing; make run also auto-applies at startup

# Build and run (config/lightstack.yaml is used by default)
make run
```

The binary is `bin/lightstack`. It starts seven HTTP servers on localhost:

| Port | Service |
|------|---------|
| 35357 | Keystone (Identity) |
| 8774 | Nova (Compute) |
| 9696 | Neutron (Network) |
| 8776 | Cinder (Block Storage) |
| 9292 | Glance (Image) |
| 8778 | Placement (stub) |
| 8775 | Metadata (EC2-style, no auth) |

Then configure the OpenStack client as in Path A step 2 and verify with
`openstack token issue`.

For development with hot reload: `make dev`. Full build/test/lint commands:
[CONTRIBUTING.md](CONTRIBUTING.md).

---

## Path D — Helm chart (Kubernetes)

The chart lives in [`charts/lightstack`](../charts/lightstack/README.md). It requires a
container image you supply (there is no default by design):

```bash
helm install lightstack ./charts/lightstack \
  --set image.repository=<your-registry>/lightstack \
  --set image.tag=<tag> \
  --set database.url="postgres://user:pass@host:5432/lightstack?sslmode=disable" \
  --set secrets.adminPassword=$(openssl rand -base64 16) \
  --set secrets.jwtSecret=$(openssl rand -base64 32)
```

> The chart fails fast at install time if `database.url`, `secrets.jwtSecret` or
> `secrets.adminPassword` are missing — there are no insecure defaults. This stub-mode
> stack has no console proxy; the noVNC proxy is deployed by the chart in real mode.

The chart runs readiness/liveness probes, drain handling and (optional) host-state
teardown jobs. See [KUBERNETES_DEPLOYMENT.md](KUBERNETES_DEPLOYMENT.md) and the
[chart README](../charts/lightstack/README.md).

---

## Path E — Single node with real KVM (real VMs)

For a production-style demo with actual virtual machines on one Linux host
(Ubuntu 24.04/22.04 or Debian 12, 16GB+ RAM, VT-x/AMD-V):

```bash
wget https://raw.githubusercontent.com/cobaltcore-dev/lightstack/main/scripts/deploy-single-node.sh
chmod +x deploy-single-node.sh
sudo ./deploy-single-node.sh
```

Installs lightstack with real libvirt/KVM, PostgreSQL, a network bridge, the Horizon
dashboard and noVNC. Guide: [SINGLE_NODE_DEPLOYMENT.md](SINGLE_NODE_DEPLOYMENT.md) ·
script reference: [scripts/README.md](../scripts/README.md).

---

## Default credentials

All install paths seed the same admin identity (`admin`, project `default`, domain
`Default`), but the password depends on the path:

- **Path A (plain Docker dev stack):** `admin` / `secret` — the public default, opted
  into by `LIGHTSTACK_ALLOW_INSECURE_DEFAULT_SECRET` for local testing only.
- **Path B (Horizon stack):** `admin` / `$LIGHTSTACK_ADMIN_PASSWORD` — required, no default.
- **Helm:** `admin` / `secrets.adminPassword` — required, no default.
- **Single-node script:** generated and saved in `/root/.lightstack-env`.

> **⚠️ Change any seeded password before exposing a deployment.** Also replace the JWT
> secret — anyone who knows it can forge admin tokens. In Docker, set
> `LIGHTSTACK_JWT_SECRET` and `LIGHTSTACK_ADMIN_PASSWORD` in your environment or in
> `deployments/.env` (Compose V2 resolves `.env` from the compose file's directory):
> `cp .env.example deployments/.env`. In the Helm chart, set `secrets.jwtSecret` and
> `secrets.adminPassword` (both required, no defaults). The plain dev stack may keep its
> public defaults only because it sets the opt-out explicitly.

## Troubleshooting quick hits

| Symptom | Fix |
|---|---|
| `openstack token issue` → connection refused | Services not up yet; wait for `docker compose ps` to show `(healthy)`. Check you're using port **35357**, not 5000/5001. |
| `The resource could not be found` (404) on auth | Wrong `OS_AUTH_URL` path — it must end in `/v3`. |
| Authentication fails | Domain names are case-sensitive: `OS_USER_DOMAIN_NAME=Default`. |
| Compose build fails | Run from the repo root with `-f deployments/docker-compose.yml`. |
| Port already in use | Something else holds 35357/8774/9696/8776/9292/8775 (plus 5432/6379 for the stack's postgres/redis) — stop it or edit the port mapping. |

More: [TROUBLESHOOTING.md](TROUBLESHOOTING.md)

## Where to go next

- **Command cheat sheet** — [QUICK_REFERENCE.md](QUICK_REFERENCE.md)
- **All configuration options** — [CONFIGURATION.md](CONFIGURATION.md)
- **Real mode (KVM, networking, storage)** — [REAL_LIBVIRT_MODE.md](REAL_LIBVIRT_MODE.md),
  [NETWORKING_MODES.md](NETWORKING_MODES.md), [STORAGE_MODES.md](STORAGE_MODES.md)
- **Upgrading an existing install** — [UPGRADING.md](UPGRADING.md)
- **Day-to-day operations** — [OPERATIONS.md](OPERATIONS.md)
- **Full documentation index** — [INDEX.md](INDEX.md)
