# Configuration Guide

**📚 Complete Documentation**: See **[INDEX.md](INDEX.md)** for full documentation index with learning paths.


Complete reference for configuring lightstack.

---

## Table of Contents

- [Overview](#overview)
- [Configuration File](#configuration-file)
- [Environment Variables](#environment-variables)
- [Database Configuration](#database-configuration)
- [Service Configuration](#service-configuration)
- [Security Configuration](#security-configuration)
- [Storage Configuration](#storage-configuration)
- [Networking Configuration](#networking-configuration)
- [Logging Configuration](#logging-configuration)
- [Advanced Configuration](#advanced-configuration)

---

## Overview

Lightstack can be configured through:

1. **Configuration file** (`config/lightstack.yaml`) - Primary configuration method
2. **Environment variables** - Override specific settings
3. **Command-line flags** - Runtime overrides

**Priority order:** Command-line flags > Environment variables > Configuration file > Defaults

---

## Configuration File

### Location

**Default locations** (checked in order):
1. `./lightstack.yaml` (current directory)
2. `/etc/lightstack/lightstack.yaml` (system-wide)
3. `~/.lightstack/lightstack.yaml` (user-specific)

**Specify custom location:**
```bash
lightstack --config /path/to/custom.yaml
```

### Complete Example

**File:** `config/lightstack.yaml`

```yaml
# Node-local state root (real mode): image bases, volumes, instance overlays,
# cloud-init ISOs, DHCP state, console tokens. Absolute; the same path the host
# sees, since libvirtd opens the paths written into the domain XML.
state_path: /var/lib/lightstack

# Database connection
database:
  url: "postgres://lightstack:secret@localhost/lightstack?sslmode=disable"
  max_connections: 20
  max_idle: 5
  conn_max_lifetime: 1h

# Keystone (Identity Service)
keystone:
  port: 35357
  jwt_secret: "change-me-in-production"
  token_ttl: 24h
  admin_user: admin
  admin_password: secret

# Compute node configuration
compute:
  node_id: auto              # Node UUID (auto-generates if "auto")
  tunnel_ip: auto            # VXLAN tunnel IP (auto-detects if "auto")
  vxlan_port: 4789           # VXLAN UDP port
  heartbeat_interval: 30s    # Node heartbeat interval

# Nova (Compute Service)
nova:
  port: 8774
  libvirt_uri: "qemu:///system"
  default_flavor: m1.small
  libvirt_mode: stub         # "stub" or "real"

# Neutron (Network Service)
neutron:
  port: 9696
  dhcp_lease_time: 24h
  iptables_enabled: true
  networking_mode: stub      # "stub", "iptables", or "ebpf"

  # VXLAN overlay networking (multi-node)
  vxlan_enabled: false
  vni_range_start: 1000
  vni_range_end: 10000
  coordination_poll_interval: 1s
  vxlan_mtu: 1450

# Cinder (Block Storage Service)
cinder:
  port: 8776
  ceph_pool: volumes
  ceph_conf: /etc/ceph/ceph.conf
  storage_mode: local        # "stub", "local", "rbd", or "local,rbd"

# Glance (Image Service)
glance:
  port: 9292
  ceph_pool: images
  ceph_conf: /etc/ceph/ceph.conf
  storage_mode: local        # "stub", "local", "rbd", "s3", etc.
  s3_bucket: ""              # S3 bucket name (for S3 storage)
  s3_region: us-east-1       # S3 region
  s3_endpoint: ""            # Custom S3 endpoint (MinIO, Ceph RGW)

# Logging
logging:
  level: info                # debug, info, warn, error
  format: json               # json, text
```

---

## Environment Variables

> **Correction (SPEC-008).** Earlier versions of this page documented a generic
> `lightstack_<SECTION>_<KEY>` convention (e.g. `LIGHTSTACK_DATABASE_URL`, `LIGHTSTACK_KEYSTONE_ADMIN_PASSWORD`,
> `LIGHTSTACK_NOVA_LIBVIRT_MODE`). **The binary does not implement that layer** — those variables
> are silently ignored. Everything else (admin password, service modes, VXLAN, cache,
> logging, ports, …) takes effect **only from the YAML config file** passed via `--config`.

The binary honors exactly these environment variables:

| Variable | Overrides / controls | Where it is read |
|----------|----------------------|------------------|
| `LIGHTSTACK_DB_URL` | `database.url`: `postgres://…` or `sqlite:///absolute/path` (see [Database Configuration](#database-configuration)) | `internal/common/config.go` (`LoadConfig`) |
| `LIGHTSTACK_DB_BACKEND` | the backend the deployment expects, `postgres` or `sqlite`. When set, a `database.url` that selects the other one is refused at startup | startup (`cmd/lightstack/main.go`, `cmd/lightstack-migrate`) |
| `LIGHTSTACK_NODE_NAME` | the name this node reserves volumes and snapshots under while it creates them; the startup sweep resolves only its own. Defaults to the host name. Set it where the host name changes across restarts — a pod without host networking; the Helm chart sets it to the pod's node name | startup (`internal/common/nodename.go`) |
| `LIGHTSTACK_JWT_SECRET` | `keystone.jwt_secret` | `internal/common/config.go` (`LoadConfig`) |
| `LIGHTSTACK_KEYSTONE_RGW_COMPAT_TENANT_IDS` | `keystone.rgw_compat_tenant_ids`. Two-way boolean: `true`/`1` turns it **on** over a config file that shipped it off; `false`/`0` turns it **off** against a config file that shipped it on. Renders `token.project.id` in OpenStack's dash-free form in the `/v3/auth/tokens` responses; `/v3/s3tokens` already uses that historical form. The EC2 mint accepts dash-free or alternate UUID encodings as input and stores the canonical database id. Enabling the flag therefore unifies Swift with the existing S3 tenant namespace without re-keying S3 data. | `internal/common/config.go` (`LoadConfig`) |
| `LIGHTSTACK_ENDPOINT_HOST` | advertised catalog / self-link host — a **fallback**, see below | request time (`internal/keystone/auth.go`, `internal/common/baseurl.go`) |
| `LIGHTSTACK_ENDPOINT_SCHEME` | advertised URL scheme: `http` (default) or `https` | request time (`internal/common/scheme.go`) |
| `LIGHTSTACK_DRAIN_SECONDS` | graceful-shutdown drain window | shutdown (`cmd/lightstack/main.go`) |
| `LIGHTSTACK_GLANCE_STORAGE_MODE` | `glance.storage_mode` | `internal/common/config.go` (`LoadConfig`) |
| `LIGHTSTACK_GLANCE_S3_ENDPOINT` | `glance.s3_endpoint` | `internal/common/config.go` (`LoadConfig`) |
| `LIGHTSTACK_GLANCE_S3_BUCKET` | `glance.s3_bucket` | `internal/common/config.go` (`LoadConfig`) |
| `LIGHTSTACK_GLANCE_S3_REGION` | `glance.s3_region` | `internal/common/config.go` (`LoadConfig`) |
| `LIGHTSTACK_GLANCE_S3_CA_BUNDLE` | `glance.s3_ca_bundle` | `internal/common/config.go` (`LoadConfig`) |
| `LIGHTSTACK_GLANCE_S3_INSECURE_SKIP_VERIFY` | `glance.s3_insecure_skip_verify`. Two-way: `false`/`0` turns it **off** against a config file that shipped it on | `internal/common/config.go` (`LoadConfig`) |
| `LIGHTSTACK_GLANCE_S3_CONNECT_TIMEOUT` | `glance.s3_connect_timeout`. A Go duration; an unparseable value is ignored with a warning and the file value stands | `internal/common/config.go` (`LoadConfig`) |
| `LIGHTSTACK_METADATA_TRUST_INSTANCE_HEADER` | `metadata.trust_instance_header`. Two-way, like the other booleans. **Test stacks only**: with it on, the metadata service believes `X-Instance-ID` from any caller, so any guest can read any instance's metadata and user-data | `internal/common/config.go` (`LoadConfig`) |

```bash
export LIGHTSTACK_DB_URL="postgres://user:pass@host/db?sslmode=disable"
export LIGHTSTACK_JWT_SECRET="your-secret-here"
export LIGHTSTACK_ENDPOINT_HOST="lightstack.example.com"
export LIGHTSTACK_ENDPOINT_SCHEME="https"   # only when TLS is terminated in front of lightstack
```

> Upgrading from a release that used the previous product name? These variables were renamed
> with it and the old spellings are **not** read — see [UPGRADING](UPGRADING.md).

To change anything else, edit (or mount) the config file. The Helm chart, for example,
renders the full `lightstack.yaml` — including `keystone.admin_password` — into a Kubernetes
**Secret** mounted at the `--config` path, precisely because no per-key env override exists.

#### `LIGHTSTACK_ENDPOINT_HOST` is a fallback, not an override

**Behavioural change (SPEC-025).** Self-links now name **the host the client actually
addressed**, and `LIGHTSTACK_ENDPOINT_HOST` is used only when a request carries no usable host.
It previously won unconditionally.

Why it changed: the old precedence made a per-command endpoint override unusable. A
client that set `OS_COMPUTE_ENDPOINT_OVERRIDE` (or any `OS_*_ENDPOINT_OVERRIDE`) would
fetch the service's version document, be handed back the deployment's own hostname in
place of the one it had explicitly named, and then fail against a host the caller never
chose.

What to expect after upgrading:

- Behind a proxy, ingress or load balancer, **nothing changes in practice** — the request
  already carries the hostname you want advertised, so that is what gets advertised.
- Reached directly by address, self-links now name that address rather than the
  configured name. If clients relied on being redirected to the configured name, they
  will now stay where they are.
- **There is deliberately no setting to restore the old order.** Two rules for the same
  question is what produced the defect.

#### `LIGHTSTACK_ENDPOINT_HOST` also decides what the service catalog caches

A second, narrower role. The catalog cache key includes the host a request addressed —
it has to, or one caller's `Host` header would decide which endpoints another caller in
the same project is handed, and therefore where that caller sends its token. But the host
comes from a client-supplied header, so keying on it lets callers choose the key space:
with `cache.enabled: true`, a tenant varying that one header writes a Redis entry per
value, each held 24 hours in a store shared across the deployment.

So **only the host named by `LIGHTSTACK_ENDPOINT_HOST` is cached.** A request addressing any
other name is answered correctly, from the database, uncached.

What that means in practice:

- `cache.enabled` defaults to **false**, so most deployments are unaffected either way.
- If you enable caching and want catalog lookups served from it, **set
  `LIGHTSTACK_ENDPOINT_HOST` to the name your clients use.** Left unset, the advertised host is
  `localhost` and a deployment addressed by any other name caches no catalogs.
- The endpoints themselves are unaffected: precedence is still request host first, as
  above. This setting governs caching only.

**The service catalog now follows the same rule** (SPEC-025-followup). It previously did not:
catalog endpoint URLs were resolved from `LIGHTSTACK_ENDPOINT_HOST` alone, so pointing it at a name
clients could not resolve made *every* command fail at catalog lookup — one step **before** any
version document, and therefore before the self-link fix above could ever be reached. A client that
can reach the deployment at all is now handed a catalog naming the address it used.

`LIGHTSTACK_ENDPOINT_HOST` remains the fallback for both, used when a request carries no usable host, and
`localhost` is the last resort so an advertised URL is always absolute and parseable.

Two consequences worth knowing:

- **A forwarded-host header is deliberately not trusted.** Lightstack reads `Host`, never
  `X-Forwarded-Host`. On a direct connection a forwarding header is chosen by the client, and the
  advertised endpoints are exactly where a client will send its token — so trusting one would hand
  an attacker the choice. **A deployment behind a proxy or ingress must configure the proxy to
  preserve the original `Host`** (nginx: `proxy_set_header Host $host;`); otherwise Lightstack advertises
  whatever name the proxy forwarded, typically the internal service name. That is no worse than the
  old `localhost` default, but it is not automatically better either.
- **The catalog cache is keyed on the host as well as the project.** Enabling `cache.enabled`
  (Redis, off by default) with a project-only key would let one caller's `Host` decide the endpoints
  another caller in the same project receives. Nothing to configure — noted because the cache's
  24-hour TTL previously rested on the catalog being identical for every caller, and it no longer is.

The advertised **port** is applied consistently now too. It names the port the target
service is reachable on, and replaces whatever port the request carried — which is what
lets compute advertise an image location on 9292 and a console origin on 6080 while
itself being addressed on 8774. Lightstack is port-per-service, so there is no path-routed
topology for this to disturb.

### Docker Compose Example

```yaml
services:
  lightstack:
    image: lightstack:latest
    environment:
      LIGHTSTACK_DB_URL: "postgres://lightstack:secret@postgres:5432/lightstack?sslmode=disable"
      LIGHTSTACK_JWT_SECRET: "${JWT_SECRET}"
    volumes:
      # everything else (service modes, admin password, logging, ...) comes from the file:
      - ./config/lightstack.yaml:/app/config/lightstack.yaml:ro
```

---

## Database Configuration

Lightstack runs on PostgreSQL or, for a single node, on an embedded SQLite file. The scheme of
`database.url` (or `LIGHTSTACK_DB_URL`) chooses the backend once, at startup:

| `database.url` | Backend |
|---|---|
| `postgres://…`, `postgresql://…` — every form pgx accepts | PostgreSQL |
| `sqlite:///absolute/path/lightstack.db` — an absolute path, no host, no query parameters | SQLite, single node |

Anything else — an empty URL, a keyword/value string (`host=db user=…`), another scheme, a relative
SQLite path — is refused at startup with a message naming what is accepted, before any file is
created. `LIGHTSTACK_DB_BACKEND=postgres|sqlite`, when set, must name the backend the URL selects; a
mismatch is refused too, the guard for a deployment configured for the wrong one.

### PostgreSQL Connection

```yaml
database:
  url: "postgres://user:password@host:port/database?sslmode=disable"
```

```
postgres://[user]:[password]@[host]:[port]/[database]?[parameters]
```

**Parameters:**
- `sslmode` - SSL mode (disable, require, verify-ca, verify-full)
- `connect_timeout` - Connection timeout in seconds
- `application_name` - Application name for logging

**Examples:**

```bash
# Local development (no SSL)
postgres://lightstack:secret@localhost:5432/lightstack?sslmode=disable

# Production (SSL required)
postgres://lightstack:secret@db.example.com:5432/lightstack?sslmode=require

# With timeout
postgres://lightstack:secret@localhost:5432/lightstack?sslmode=disable&connect_timeout=10
```

### Pool tuning (PostgreSQL)

These keys size the PostgreSQL connection pool. Each is optional and falls back to the default below
when unset or non-positive. They have no effect on SQLite, whose connection settings are fixed (see
the SQLite section).

```yaml
database:
  url: "postgres://…"
  max_connections: 20        # maximum open connections (default 20)
  min_connections: 2         # idle connections kept warm (default 2)
  max_conn_lifetime: 1h      # recycle a connection after this age (default 1h)
  max_conn_idle_time: 15m    # close a connection idle this long (default 15m)
  health_check_period: 1m    # how often the pool checks connection health (default 1m)
```

### SQLite

```yaml
database:
  url: "sqlite:///var/lib/lightstack/db/lightstack.db"
```

- **Path**: `<state_path>/db/lightstack.db` is the recommended place — node-local, beside the other
  node-local state. The server creates the directory (`0750`) and the file (`0600`) on first start
  and migrates it. Missing directories above it are created `0755`: QEMU must traverse the state
  root. A database file, `-wal` or `-shm` file with group or other permissions is tightened to
  `0600` at startup, with a `database file mode tightened` log line.
- **Single node**: with `neutron.vxlan_enabled: true` the server refuses to start. Multi-node
  deployments use PostgreSQL.
- **Settings**: write-ahead logging, `synchronous=FULL`, a 5 s busy wait, foreign keys on, and
  transactions that begin `IMMEDIATE`. They are fixed, not configurable, and read back at startup;
  the `database ready` log line reports them.
- **Entry points**: `lightstack cleanup` and `lightstack console-tokens` never create the database.
  For a missing file, `console-tokens` exits 1 naming the path; `cleanup` warns naming it and does
  its host-only cleanup, as for an unreachable PostgreSQL.
- **Files**: `lightstack.db`, and beside it `lightstack.db-wal`, `lightstack.db-shm` and
  `lightstack.db.migrate-lock`. Committed data can sit in the `-wal` file until a checkpoint, so
  never copy `lightstack.db` alone. Back up with `lightstack-migrate backup` (a consistent copy while
  Lightstack runs, to `<state_path>/backups`) and verify with `lightstack-migrate check`; restoring
  means deleting `-wal` and `-shm` first ([KUBERNETES_DEPLOYMENT.md](KUBERNETES_DEPLOYMENT.md#backup-integrity-check-and-restore),
  [DOCKER_DEPLOYMENT.md](DOCKER_DEPLOYMENT.md#backup-and-restore-on-sqlite)).

The startup log line names the backend, where the database is (never with credentials), the schema
version and the node name:

```json
{"level":"info","backend":"sqlite","location":"/var/lib/lightstack/db/lightstack.db","schema_version":74,"node":"node-a","journal_mode":"wal","synchronous":"full","foreign_keys":true,"busy_timeout_ms":5000,"txlock":"immediate","time":"2026-10-12T10:00:00Z","message":"database ready"}
```

### Connection Pool Tuning

```yaml
database:
  max_connections: 20        # Default: 20
  min_connections: 2         # Default: 2
  max_conn_lifetime: 1h      # Default: 1 hour
  max_conn_idle_time: 15m    # Default: 15 minutes
  health_check_period: 1m    # Default: 1 minute; PostgreSQL only
```

| Setting | PostgreSQL | SQLite |
|---|---|---|
| `max_connections` | pool size | open connections; SQLite still writes one transaction at a time |
| `min_connections` | connections kept open | idle connections kept |
| `max_conn_lifetime`, `max_conn_idle_time` | as named | as named |
| `health_check_period` | pool health checks | not used |

**Guidelines (PostgreSQL):**
- **Small deployments:** 10-20 connections
- **Medium deployments:** 20-50 connections
- **Large deployments:** 50-100 connections

**Formula:** `max_connections = (expected_concurrent_requests * 1.5)`

---

## Service Configuration

### Keystone (Identity Service)

```yaml
keystone:
  port: 35357                        # API port
  jwt_secret: "change-me-in-production"  # JWT signing secret
  token_ttl: 24h                     # Token lifetime
  admin_user: admin                  # Admin username (first-boot seed)
  admin_password: secret             # Admin password (first-boot seed) — CHANGE for exposed deployments
```

**Admin bootstrap (first-boot seed):** `admin_password` is applied at startup only
when the admin account is absent or still carries the well-known default `secret`;
once changed it is never overwritten on later starts (rotate via the Identity API,
e.g. `openstack user password set`, not config). Setting `admin_user` to a name
other than `admin` provisions that user with the admin role and **disables** the
seeded `admin` account. When `admin_password` is unset entirely, Lightstack boots with the
default credential and logs a `WARN` — the Helm chart is responsible for requiring a
password at install time.

`admin_user` is a **first-boot identity**, not a managed one:
- If `admin_user` names an account that **already exists**, that account is granted
  the admin role but its password is **left unchanged** (`admin_password` is not
  applied; a `WARN` is logged). Change an existing user's password via the Identity
  API, not config.
- **Renaming** `admin_user` on a later start provisions the new name but does **not**
  remove the previously-configured one — only the seeded `admin` is disabled. Remove
  a stale admin account via the Identity API.

**Advertised endpoint scheme:** the service catalog and resource self-links use the
scheme from the `LIGHTSTACK_ENDPOINT_SCHEME` environment variable (`http` default, or
`https`), alongside `LIGHTSTACK_ENDPOINT_HOST`. See [Environment Variables](#environment-variables).

**Security Notes:**
- ⚠️ **Always change `jwt_secret` in production!**
- ⚠️ **Change default admin credentials!**
- Use strong random string for `jwt_secret` (32+ characters)

**Generate secure JWT secret:**
```bash
openssl rand -hex 32
```

### Nova (Compute Service)

```yaml
nova:
  port: 8774                         # API port
  libvirt_uri: "qemu:///system"      # libvirt connection URI
  default_flavor: m1.small           # Default flavor for VMs
  libvirt_mode: stub                 # "stub" or "real"
```

**libvirt_uri** names the local libvirt socket Lightstack dials. Read in real mode only:
- `qemu:///system` (the default; empty and `qemu+unix:///system` mean the same) - Lightstack chooses
  the socket on every connection attempt: `/var/run/libvirt/virtqemud-sock` where the modular
  daemons run, else `/var/run/libvirt/libvirt-sock` (the monolithic `libvirtd`, or `virtproxyd`).
- `qemu:///system?socket=/path/to/sock` or `qemu+unix:///system?socket=/path/to/sock` - that
  socket, exactly.
- `/path/to/sock` - an absolute socket path: that socket, exactly.

Anything else - a session URI, another driver, a remote host or transport (`qemu+tcp://`,
`qemu+ssh://`, ...), a relative path, any other parameter - stops real-mode startup, and the
`cleanup` and `console-tokens` commands, with an error naming the value and these forms.
Lightstack only ever dials the libvirt on its own node.

**libvirt_mode:**
- `stub` - Mock libvirt (for testing, no actual VMs)
- `real` - Real libvirt integration (creates actual VMs)

#### Run state, shutdown grace and audit retention (SPEC-025)

```yaml
nova:
  state_reconcile_interval: 30s      # how often recorded run state is checked
  shutdown_timeout: 30s              # grace given to a guest OS before a stop forces it off
  instance_action_retention: 2160h   # 90 days — how long a deleted server's actions are kept
```

**`state_reconcile_interval`** (default `30s`) — how often the recorded run state of
real-mode servers is compared against the hypervisor and corrected.

Only drift Lightstack did *not* cause needs this: every lifecycle action persists its own
result, so a stop or start is recorded immediately and needs no sweep. What this catches
is a guest that crashed, or one removed out of band. Reading a server never contacts the
hypervisor — deliberately, so a listing does not cost one round trip per server — which
means this interval is how stale a server's reported state can be for drift of that kind.
A non-positive value is clamped to the default rather than honoured, since `0` would mean
a busy loop.

**`shutdown_timeout`** (default `30s`, maximum `50s`) — how long a guest OS is given to
shut down cleanly before a stop powers it off the hard way.

libvirt's shutdown is a *request* to the guest, not a state transition: the guest's own
`on_poweroff` handling decides what to do with it, it commonly takes several seconds, and
a guest that ignores ACPI never honours it at all. A stop that returned as soon as libvirt
accepted the request therefore reported success over a guest that was still running, and
recorded `SHUTOFF` for it. So a stop now waits for the guest to actually stop, and forces
it off once this period expires.

**A stop is synchronous, so this is also how long an `os-stop` request can take.** That is
what bounds it, and it is why the default is *below* upstream nova's `shutdown_timeout` of
60s — upstream's stop is asynchronous and holds no connection open, so it can afford a
minute where Lightstack cannot:

- **30s default** — the default read timeout of gunicorn (which fronts Horizon) and of
  most ingresses. A longer default fails in a deployment rather than in a test.
- **50s ceiling** — `HTTPWriteTimeout` is 60s, and Go resets that deadline when a
  request's header is read, so it covers *handler execution plus the response write*. A
  handler that runs past it has its connection closed with **no response written**: the
  stop succeeds and reports nothing, and a client that retries then gets a truthful `409`
  for a stop that already happened. A configured value above the ceiling is clamped down,
  with a warning on stderr.

An ordinary Linux guest stops in a few seconds; the full period is only reached by a guest
that is ignoring the request — a Windows guest at a "some apps are still running" prompt,
or any guest stopped before its ACPI handler is up. A non-positive value is clamped to the
default: unset YAML is indistinguishable from a deliberate `0`, and the safe reading of
"not configured" is not "hard-kill every guest".

**`instance_action_retention`** (default `2160h`, i.e. 90 days) — how long a server's
action records are kept **after the server itself is deleted**.

Deleting a server does not delete its action history; upstream retains it and so does
Lightstack. Without a retention age the table would only grow. Records belonging to a server
that still exists are never reclaimed, whatever their age. A non-positive value is
clamped to the default, since `0` would mean reclaiming everything immediately.

All three are **config-file only**. The binary honours exactly four environment variables
(see [Environment Variables](#environment-variables)), so these must arrive through the
`lightstack.yaml` you pass to `--config` — in Helm, through the rendered Secret. The chart
exposes them as `nova.stateReconcileInterval`, `nova.shutdownTimeout` and
`nova.instanceActionRetention`.

### Neutron (Network Service)

```yaml
neutron:
  port: 9696                         # API port
  dhcp_lease_time: 24h               # DHCP lease duration
  iptables_enabled: true             # Enable iptables security groups
  networking_mode: stub              # "stub", "iptables", or "ebpf"

  # VXLAN overlay (multi-node)
  vxlan_enabled: false               # Enable VXLAN overlay
  vni_range_start: 1000              # VNI range start
  vni_range_end: 10000               # VNI range end
  coordination_poll_interval: 1s     # Coordination check interval
  vxlan_mtu: 1450                    # VXLAN MTU
```

**networking_mode:**
- `stub` - Mock networking (no actual networks)
- `iptables` - iptables-based security groups (production)
- `ebpf` - east-west XDP filtering on top of the iptables north-south filter — partial, and it does not
  load in a stock build (see [EBPF_STATUS.md](EBPF_STATUS.md))

**VXLAN configuration** (multi-node only):
- `vxlan_enabled: true` - Enable VXLAN overlay networking
- `vni_range` - VXLAN Network Identifier range (1000-10000)
- `vxlan_mtu: 1450` - Accounts for VXLAN overhead (50 bytes)

### Cinder (Block Storage Service)

```yaml
cinder:
  port: 8776                         # API port
  ceph_pool: volumes                 # Ceph pool for volumes
  ceph_conf: /etc/ceph/ceph.conf     # Ceph config file
  storage_mode: local                # "stub", "local", "rbd", "local,rbd"
```

**storage_mode options:**
- `stub` - Mock storage (no actual volumes)
- `local` - Local filesystem storage (for testing)
- `rbd` - Ceph RBD storage (production)
- `local,rbd` - Hybrid (local + Ceph fallback)

### Glance (Image Service)

```yaml
glance:
  port: 9292                         # API port
  ceph_pool: images                  # Ceph pool for images
  ceph_conf: /etc/ceph/ceph.conf     # Ceph config file
  storage_mode: local                # Storage backend
  s3_bucket: ""                      # S3 bucket (for S3 storage)
  s3_region: us-east-1               # S3 region
  s3_endpoint: ""                    # Custom S3 endpoint
```

**storage_mode options:**
- `stub` - Mock storage (no actual images)
- `local` - Local filesystem
- `rbd` - Ceph RBD
- `s3` - S3-compatible storage
- `local,rbd` - Hybrid (local + Ceph)
- `local,s3` - Hybrid (local + S3)

> `rbd,s3` is **not supported** for Glance and is rejected at startup: the Glance RBD
> backend is not implemented, so the S3 replica would never be written. Use `local,s3`.

**S3 configuration:**
```yaml
glance:
  storage_mode: s3
  s3_bucket: "my-images-bucket"
  s3_region: "us-west-2"
  # For MinIO/Ceph RGW:
  s3_endpoint: "https://s3.example.com"
```

**Environment variables for S3 credentials** (read implicitly by the AWS SDK's
standard config chain — they are not `LIGHTSTACK_`-prefixed and are not part of
the thirteen Lightstack configuration variables):
```bash
export AWS_ACCESS_KEY_ID="your-access-key"
export AWS_SECRET_ACCESS_KEY="your-secret-key"
```

---

## Security Configuration

### JWT Token Security

```yaml
keystone:
  jwt_secret: "change-me-in-production"  # CRITICAL: Change this!
  token_ttl: 24h                         # Token lifetime
```

**Best practices:**
1. **Use strong secret:** 32+ random characters
2. **Rotate regularly:** Change every 90 days
3. **Store securely:** Use secrets management (Vault, AWS Secrets Manager)
4. **Never commit:** Add to `.gitignore`

**Generate secure secret:**
```bash
# Option 1: OpenSSL
openssl rand -hex 32

# Option 2: /dev/urandom
head -c 32 /dev/urandom | base64

# Option 3: UUID
uuidgen | sha256sum | cut -d' ' -f1
```

### Default Credentials

```yaml
keystone:
  admin_user: admin
  admin_password: secret
```

**⚠️ CRITICAL: Change these in production!**

**Change via the config file** (there is no env override for these — see
[Environment Variables](#environment-variables)):
```yaml
keystone:
  admin_user: myadmin            # optional; disables the seeded `admin` when changed
  admin_password: "<generate a strong password>"
```
The configured password is a **first-boot seed** (SPEC-007): applied only while the
account still carries the default; rotate afterwards via the Identity API
(`openstack user password set`), not by changing config.

### Database Security

```yaml
database:
  url: "postgres://user:password@host/db?sslmode=require"
```

**Production settings:**
- Use `sslmode=require` or `sslmode=verify-full`
- Use strong database password
- Restrict database access by IP
- Use connection pooling

---

## Storage Configuration

### Node-local state root

```yaml
state_path: /var/lib/lightstack
```

Top-level, not per service: image bases, volumes, instance overlays, cloud-init ISOs, DHCP
state and console tokens all live under it. It must be absolute and not `/`, and Lightstack
refuses to start otherwise. It must also be the **same path on the host** as Lightstack sees:
libvirtd opens the paths Lightstack writes into the domain XML, so a container has to mount
the root at the path it has on the host. The Helm chart sets it from
`realMode.hostStoragePath` and mounts it that way.

### Local Storage

```yaml
cinder:
  storage_mode: local

glance:
  storage_mode: local
```

**Storage locations** (under `state_path`, default shown):
- Volumes: `/var/lib/lightstack/volumes/`
- Images: `/var/lib/lightstack/images/`

**Requirements:**
- Sufficient disk space
- Fast I/O (SSD recommended)
- Regular backups

### Ceph RBD Storage

```yaml
cinder:
  storage_mode: rbd
  ceph_pool: volumes
  ceph_conf: /etc/ceph/ceph.conf

glance:
  storage_mode: rbd
  ceph_pool: images
  ceph_conf: /etc/ceph/ceph.conf
```

**Prerequisites:**
```bash
# Install Ceph client
apt-get install ceph-common

# Verify Ceph access
rbd ls -p volumes
```

**Ceph pools must exist:**
```bash
ceph osd pool create volumes 128
ceph osd pool create images 128
```

### S3 Storage (Images Only)

```yaml
glance:
  storage_mode: s3
  s3_bucket: "my-images-bucket"
  s3_region: "us-west-2"
  s3_endpoint: ""  # Optional: for MinIO, Ceph RGW
```

**AWS S3:**
```bash
export AWS_ACCESS_KEY_ID="AKIAIOSFODNN7EXAMPLE"
export AWS_SECRET_ACCESS_KEY="wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
```

**MinIO:**
```yaml
glance:
  storage_mode: s3
  s3_bucket: "images"
  s3_region: "us-east-1"
  s3_endpoint: "https://minio.example.com"
```

### Hybrid Storage

```yaml
glance:
  storage_mode: local,rbd  # Try local first, fallback to Ceph
```

**Use cases:**
- Fast local cache + reliable Ceph backup
- Gradual migration between storage backends
- Cost optimization (local SSD + slower network storage)

---

## Networking Configuration

### Single-Node Setup

```yaml
neutron:
  networking_mode: iptables
  iptables_enabled: true
  vxlan_enabled: false
```

**Features:**
- Bridge-based networking
- iptables security groups
- No VXLAN overhead

### Multi-Node Setup

```yaml
compute:
  tunnel_ip: 192.168.1.10    # This node's tunnel IP
  vxlan_port: 4789           # VXLAN UDP port

neutron:
  networking_mode: iptables
  vxlan_enabled: true
  vni_range_start: 1000
  vni_range_end: 10000
  vxlan_mtu: 1450
```

**Requirements:**
- All nodes must reach each other on `tunnel_ip`
- UDP port 4789 open between nodes
- MTU adjusted for VXLAN overhead

**Auto-detection:**
```yaml
compute:
  tunnel_ip: auto  # Auto-detects primary IP
```

---

## Logging Configuration

### Log Levels

```yaml
logging:
  level: info    # debug, info, warn, error
  format: json   # json, text
```

**Levels:**
- `debug` - Verbose logging (development)
- `info` - Normal operations (production)
- `warn` - Warnings only
- `error` - Errors only

### Log Formats

**JSON format** (production):
```json
{"time":"2026-03-07T21:45:12Z","level":"info","msg":"Server started","port":8774}
```

**Text format** (development):
```
2026-03-07T21:45:12Z [INFO] Server started port=8774
```

### Log Output

**Stdout** (default):
```yaml
logging:
  level: info
  format: json
```

**File** (via systemd):
```ini
[Service]
StandardOutput=append:/var/log/lightstack/lightstack.log
StandardError=append:/var/log/lightstack/lightstack-error.log
```

**Syslog** (via command):
```bash
lightstack 2>&1 | logger -t lightstack
```

---

## Advanced Configuration

### Compute Node Registration

```yaml
compute:
  node_id: auto                # Auto-generate UUID
  tunnel_ip: auto              # Auto-detect IP
  heartbeat_interval: 30s      # Heartbeat frequency
```

**Manual node ID:**
```yaml
compute:
  node_id: "550e8400-e29b-41d4-a716-446655440000"
  tunnel_ip: "192.168.1.10"
```

### Performance Tuning

**Database connections:**
```yaml
database:
  max_connections: 50          # Increase for high load
  max_idle: 10
  conn_max_lifetime: 30m       # Shorter for frequent schema changes
```

**DHCP lease time:**
```yaml
neutron:
  dhcp_lease_time: 12h         # Shorter for dynamic environments
```

### Resource Limits

**Default quotas** (set in database):
```sql
UPDATE projects SET quota_instances = 100 WHERE name = 'production';
UPDATE projects SET quota_cores = 200 WHERE name = 'production';
UPDATE projects SET quota_ram = 204800 WHERE name = 'production';
```

### Multi-Tenant Isolation

**Network isolation:**
```yaml
neutron:
  vxlan_enabled: true          # Required for tenant isolation
  vni_range_start: 1000
  vni_range_end: 10000         # 9000 networks max
```

---

## Rate Limiting Configuration

Lightstack supports two-tier rate limiting to protect against brute-force attacks and resource exhaustion.
See [ADR-002](adr/ADR-002-rate-limiting-architecture.md) for architectural details.

### Configuration

```yaml
# Rate limiting (v0.7.0+)
rate_limiting:
  enabled: false              # Set to true to enable rate limiting
  auth_rate: 20               # Requests per minute per IP (unauthenticated endpoints)
  api_rate: 100               # Requests per minute per project (authenticated endpoints)
  eviction_interval: 10m      # How often to clean up stale rate limit entries
  max_entries: 10000           # Maximum tracked IPs/projects (degrade open on overflow)
  overflow_policy: allow       # "allow" (let traffic through) or "deny" (block new entries)
```

### Settings Reference

| Setting | Type | Default | Description |
|---------|------|---------|-------------|
| `enabled` | bool | `false` | Enable/disable rate limiting globally |
| `auth_rate` | int | `20` | Max requests/minute per IP for auth endpoints |
| `api_rate` | int | `100` | Max requests/minute per project for API endpoints |
| `eviction_interval` | duration | `10m` | Interval for cleaning stale entries |
| `max_entries` | int | `10000` | Max concurrent tracked IPs/projects |
| `overflow_policy` | string | `allow` | Behavior when max_entries exceeded |

### How It Works

1. **Unauthenticated requests** (e.g., `POST /v3/auth/tokens`): Limited by the client's address,
   meaning the TCP peer. Forwarding headers such as `X-Forwarded-For` are not believed: every
   server's engine trusts no proxy, since a caller could otherwise send a fresh address with each
   request and never be limited.
2. **Authenticated requests**: Limited by `project_id` extracted from the JWT token.
3. **When limit exceeded**: Returns HTTP 429 with `Retry-After` header and OpenStack-compatible error body.
4. **Overflow behavior**: When 10,000+ unique IPs/projects are tracked, new entries are allowed
   through without limiting. A warning is logged.

### Changing Limits

Rate limiting is configured **only via the config file** (no env override — see
[Environment Variables](#environment-variables)):

```yaml
rate_limiting:
  enabled: true
  auth_rate: 50
  api_rate: 200
```

### Monitoring

When rate limiting triggers, Lightstack logs at WARN level:
```json
{"level":"warn","project_id":"abc123","rate":"100/m","msg":"rate limit exceeded"}
```

---

## Configuration Validation

### Validate Configuration

```bash
# Check config syntax
lightstack --config /path/to/lightstack.yaml --validate

# Dry-run (validate without starting)
lightstack --config /path/to/lightstack.yaml --dry-run
```

### Common Issues

**Missing required fields:**
```
Error: database.url is required
```

**Invalid values:**
```
Error: logging.level must be one of: debug, info, warn, error
```

**Port conflicts:**
```
Error: port 8774 already in use
```

---

## Examples

### Development Setup

```yaml
database:
  url: "postgres://lightstack:secret@localhost/lightstack?sslmode=disable"

keystone:
  jwt_secret: "dev-secret-do-not-use-in-prod"
  token_ttl: 1h

nova:
  libvirt_mode: stub

neutron:
  networking_mode: stub

cinder:
  storage_mode: local

glance:
  storage_mode: local

logging:
  level: debug
  format: text
```

### Production Setup (Single-Node)

```yaml
database:
  url: "postgres://lightstack:${DB_PASSWORD}@db.internal:5432/lightstack?sslmode=require"
  max_connections: 50

keystone:
  jwt_secret: "${JWT_SECRET}"  # From environment
  token_ttl: 24h
  admin_password: "${ADMIN_PASSWORD}"

nova:
  libvirt_mode: real
  libvirt_uri: "qemu:///system"

neutron:
  networking_mode: iptables
  vxlan_enabled: false

cinder:
  storage_mode: rbd
  ceph_pool: volumes

glance:
  storage_mode: rbd
  ceph_pool: images

logging:
  level: info
  format: json
```

### Production Setup (Multi-Node)

```yaml
database:
  url: "postgres://lightstack:${DB_PASSWORD}@db.internal:5432/lightstack?sslmode=require"

compute:
  tunnel_ip: auto
  vxlan_port: 4789

keystone:
  jwt_secret: "${JWT_SECRET}"

nova:
  libvirt_mode: real

neutron:
  networking_mode: iptables
  vxlan_enabled: true
  vni_range_start: 1000
  vni_range_end: 10000

cinder:
  storage_mode: rbd

glance:
  storage_mode: rbd

logging:
  level: info
  format: json
```

---

## Summary

**Key configuration points:**
- ✅ Change `jwt_secret` in production
- ✅ Change default admin credentials
- ✅ Use SSL for database connections
- ✅ Choose appropriate storage backends
- ✅ Enable VXLAN for multi-node
- ✅ Tune database connection pool
- ✅ Set appropriate log levels

**Next steps:**
- [INSTALLATION.md](INSTALLATION.md) - Install Lightstack
- [OPERATIONS.md](OPERATIONS.md) - Day-to-day operations
- [DOCKER_DEPLOYMENT.md](DOCKER_DEPLOYMENT.md) - Docker-specific config
