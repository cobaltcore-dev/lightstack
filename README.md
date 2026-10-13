# Lightstack - OpenStack Lightweight Cloud Platform

[![CI Pipeline](https://github.com/cobaltcore-dev/lightstack/actions/workflows/ci.yml/badge.svg)](https://github.com/cobaltcore-dev/lightstack/actions/workflows/ci.yml)

**Status**: Unreleased (post-v0.6.1) | ~335 functional operations vs the 330-endpoint baseline | 100% Terraform Compatible
**Last Updated**: October 12, 2026

**lightstack** (OpenStack 3 Kubernetes-style) is a lightweight, high-performance implementation of OpenStack APIs in pure Go, inspired by how K3s simplified Kubernetes.

---

## 🎯 Coverage & Terraform Compatibility

lightstack implements **~335 functional operations** across the five core OpenStack services against the 330-endpoint baseline — coverage is substantial and Terraform-compatible, though not literally "104%"; the earlier "342/330 (104%)" figure counted raw routes (URL aliases + version discovery), see [docs/blueprint.md](docs/blueprint.md) for the reconciled analysis. Existing Terraform scripts and OpenStack CLI commands run against lightstack without any modifications. The Horizon **dashboard is usable for resource management** — networks, volumes and instances can be created from it, while the noVNC console is still open ([report](docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md)).

## 🎯 What is lightstack?

Just as **K3s** is to Kubernetes, **lightstack** is to OpenStack:
- **Lightweight**: Single ~35MB binary vs multi-GB Python distributions
- **Fast**: Go-based synchronous architecture (10x faster than traditional OpenStack)
- **Simple**: One process, one database, zero message queues
- **Drop-in Compatible**: Use existing Terraform scripts and OpenStack CLI unchanged (Horizon's UI is API-compatible but **not** a drop-in for resource management — see the [compatibility report](docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md))
- **Production Ready**: All HIGH and MEDIUM priority features complete
- **Modern**: Supports OpenStack Flamingo (2025.2) and later versions only

## 🆕 What's New since v0.6.1

**Real mode, tenancy, and hardening** — see [CHANGELOG.md](CHANGELOG.md) (Unreleased) for the full list:

- **SPEC-009**: single-node real mode — qcow2 overlays, network reconciler, cloud-init metadata, boot-from-volume
- **SPEC-021**: real noVNC console with revocable one-time tokens + `console-tokens` subcommand
- **SPEC-023**: tenant scoping & authorization model (`admin_or_owner`, `all_tenants` opt-in, domain-scope refusal)
- **SPEC-024**: Neutron API fidelity & real-mode networking (root-namespace bridges, external networks)
- **SPEC-025**: Nova client-discovery fixes + block-storage quota enforcement on all call sites
- **SPEC-026**: rename to lightstack | **SPEC-027**: database pool injection (ADR-001)
- **SPEC-014**: Ceph RGW/S3 Glance backend + tenant object storage (EC2 credentials, `/v3/s3tokens`)
- **Placement** service (`:8778`, stub) and `cleanup` / `healthcheck` subcommands

## 📦 What's Included

### OpenStack Services (~335 functional operations)
- **Keystone v3** (Identity) - JWT authentication, domains, service catalog, credentials, EC2 credentials, `/v3/s3tokens`
- **Nova v2.1** (Compute) - VM lifecycle, migrations, console access, availability zones
- **Neutron v2.0** (Network) - L3 routing, security groups, port forwarding, QoS
- **Cinder v3** (Block Storage) - Multi-backend volumes, snapshots, backups, volume groups
- **Glance v2** (Image Service) - Multi-backend images, sharing, import workflow, metadefs
- **Placement** (stub) - version discovery + empty resource-provider listings, for Horizon compatibility

Plus an EC2-compatible **metadata service** (`:8775`, no auth, guest-only trust model).

Coverage is measured in *functional operations*, not raw routes — see [docs/blueprint.md](docs/blueprint.md) for the per-resource detail and the reconciliation of the earlier "342 endpoints / 104%" count.

### Development Velocity
- **69+ Sprints Completed** (Sprint 1-70, excluding 43)
- **Coverage**: substantial against the 330-endpoint baseline (see [docs/blueprint.md](docs/blueprint.md))
- **Recent Achievements**:
  - Sprint 66-68: Horizon integration + performance optimization
  - Sprint 56-57: Nova server actions complete
  - SPEC-009 → SPEC-027: real-mode single node, noVNC console, tenant scoping, Neutron fidelity, quota enforcement

### Client Compatibility

**Zero-Modification Migration**: lightstack provides complete API compatibility - use your existing tools unchanged.

- ✅ **Terraform Provider**: All `openstack_*` resources work identically (compute, network, storage)
- ⚠️ **Horizon Dashboard**: **usable for resource management** — network, volume and instance create work from the UI; open gaps include the noVNC console (F1) ([report](docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md))
- ✅ **OpenStack CLI**: All `openstack` commands function identically
- ✅ **gophercloud SDK**: Full Go client library compatibility
- ✅ **python-openstackclient**: Python client verified

**Migration Path**: Point your Terraform provider or OpenStack client to lightstack endpoints - no script changes required.

### Architecture
- **Single Binary**: All services in one process (~35MB)
- **PostgreSQL 18+**: Unified state management (73 migrations, 35+ tables)
- **Synchronous Architecture**: No RabbitMQ/message queues (10x faster)
- **libvirt/KVM**: Real compute virtualization (stub mode for development)
- **Storage Backends**: Ceph RBD, AWS S3, MinIO, local filesystem (hybrid failover support)
- **Network Isolation**: Linux namespaces, VXLAN multi-node, iptables security groups
- **JWT Tokens**: Stateless authentication (HMAC-SHA256, 24-hour TTL)
- **Multi-Mode Support**: Seamless development (stub) to production (real) transition

## 🏗️ Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                         lightstack Binary                           │
├─────────────────────────────────────────────────────────────┤
│  Keystone (Identity)      :35357                      │
│  Nova (Compute)           :8774                       │
│  Neutron (Network)        :9696                       │
│  Cinder (Block Storage)   :8776                       │
│  Glance (Image)           :9292                       │
│  Placement (stub)         :8778                       │
│  Metadata (EC2-style)     :8775                       │
└─────────────────────────────────────────────────────────────┘
                         ↓
        ┌────────────────┼────────────────┐
        ↓                ↓                ↓
   PostgreSQL       libvirt (KVM)    Multi-Backend Storage
   (State DB)      (Compute)         (RBD/S3/Local)
                         ↓
                   netlink
                   (Networking)
```

## 🎯 Design Philosophy

### K3s Inspiration
Just as K3s removed heavyweight components from Kubernetes:
- **Removed**: RabbitMQ, memcached, multiple Python processes
- **Replaced with**: Single Go binary, PostgreSQL, direct API calls
- **Result**: 95% smaller, 10x faster, easier to deploy

### Synchronous Architecture
- No message queues (RabbitMQ/AMQP)
- Direct libvirt/Ceph/netlink calls
- Fail-fast design (1-second timeouts)
- Horizontal scaling via load balancer

### Multi-Tenancy
- Network namespace per project
- Linux bridges (single-node) or VXLAN (multi-node)
- iptables-based security groups (eBPF in future)
- Project-scoped JWT tokens

## 📚 Documentation

**Complete Documentation Index**: **[docs/INDEX.md](docs/INDEX.md)** - Full table of contents with learning paths and categorized guides.

**Quick Links**:
- [Getting Started](docs/QUICKSTART.md) - 5-minute quick start
- [Single-Node Deployment](docs/SINGLE_NODE_DEPLOYMENT.md) - Deploy with real KVM on one host
- [Production Scaling](docs/SCALING.md) - Multi-node HA cluster (3-node to 10+)
- [Horizon Compatibility Report](docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md) ⭐ **NEW** - Browser-level verification: dashboard **usable** for resource management after #532/#533 (findings F1–F18)
- [Horizon Integration](docs/HORIZON_INTEGRATION.md) - Web dashboard integration architecture
- [API Coverage Report](docs/API_COVERAGE_REPORT.md) - Superseded by [docs/blueprint.md](docs/blueprint.md)

**Learning Paths**:
| Goal | Guide | Time |
|------|-------|------|
| Quick Evaluation | [QUICKSTART](docs/QUICKSTART.md) → [QUICK_REFERENCE](docs/QUICK_REFERENCE.md) | 30 min |
| Demo/POC | [SINGLE_NODE_DEPLOYMENT](docs/SINGLE_NODE_DEPLOYMENT.md) → [HORIZON_INTEGRATION](docs/HORIZON_INTEGRATION.md) | 1 day |
| Production | [ARCHITECTURE](docs/ARCHITECTURE.md) → [SCALING](docs/SCALING.md) → [OPERATIONS](docs/OPERATIONS.md) | 1-2 weeks |

---

## Quick Start

### 🚀 Automated Single-Node Deployment (Recommended for Production Demos)

**Deploy lightstack + KVM Hypervisor on a single Linux host with one command**:

```bash
# Download and run interactive deployment script
wget https://raw.githubusercontent.com/cobaltcore-dev/lightstack/main/scripts/deploy-single-node.sh
chmod +x deploy-single-node.sh
sudo ./deploy-single-node.sh
```

**What you get** (in 15-20 minutes):
- ✅ Full lightstack installation with real KVM virtualization
- ✅ PostgreSQL database configured
- ✅ Network bridge for external connectivity
- ✅ Horizon dashboard + noVNC console
- ✅ OpenStack CLI tools ready to use
- ✅ Systemd service for automatic startup

**Requirements**: Ubuntu 24.04/22.04 or Debian 12, 16GB+ RAM, CPU with VT-x/AMD-V

**Documentation**: [scripts/README.md](scripts/README.md) | [docs/SINGLE_NODE_DEPLOYMENT.md](docs/SINGLE_NODE_DEPLOYMENT.md)

---

### ⚡ Quick Evaluation with Docker (Recommended for Testing)

**Deploy lightstack + Horizon Dashboard in Docker Compose**:

```bash
# 1. Clone repository
git clone https://github.com/cobaltcore-dev/lightstack.git
cd lightstack/deployments

# 2. Start all services (lightstack + Horizon + PostgreSQL)
#    (this compose file refuses to start without a JWT secret)
export LIGHTSTACK_JWT_SECRET=$(openssl rand -base64 32)
docker compose -f docker-compose-horizon.yml up -d

# 3. Access Horizon Dashboard
open http://localhost/dashboard
# Login: Domain=Default, User=admin, Password=secret
```

**What's included**:
- PostgreSQL 18 database
- lightstack services (Keystone, Nova, Neutron, Cinder, Glance)
- Horizon Dashboard (OpenStack Flamingo 2025.2)
- Complete web UI for cloud management

**Documentation**: [docs/UNIFIED_DEPLOYMENT.md](docs/UNIFIED_DEPLOYMENT.md) | [docs/QUICK_REFERENCE.md](docs/QUICK_REFERENCE.md)

---

### ⚡ API-Only Setup (CLI/SDK Development)

For headless deployment without web UI:

```bash
# 1. Clone repository
git clone https://github.com/cobaltcore-dev/lightstack.git
cd lightstack

# 2. Start lightstack services only
docker compose -f deployments/docker-compose.yml up -d

# 3. Install OpenStack CLI
brew install pipx && pipx install python-openstackclient

# 4. Configure environment
export OS_AUTH_URL=http://localhost:35357/v3
export OS_PROJECT_NAME=default
export OS_USERNAME=admin
export OS_PASSWORD=secret
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default

# 5. Create the prerequisites for a first VM
openstack network create my-net
openstack subnet create --network my-net --subnet-range 10.0.0.0/24 my-subnet
curl -LO https://download.cirros-cloud.net/0.6.2/cirros-0.6.2-x86_64-disk.img
openstack image create --file cirros-0.6.2-x86_64-disk.img --disk-format qcow2 cirros

# 6. Test it!
openstack token issue
openstack server create --flavor m1.small --image cirros --network my-net test-vm
```

(See [docs/QUICKSTART.md](docs/QUICKSTART.md) for the guided version of this flow.)

See [docs/QUICKSTART.md](docs/QUICKSTART.md) for the complete quick start guide.

---

### 🖥️ Horizon Dashboard Features

**IMPORTANT**: Horizon dashboard integration is **extended functionality** (not core lightstack feature). Horizon requires independent setup and configuration to work with lightstack. See setup guides below for detailed integration steps.

Lightstack serves the OpenStack APIs that Horizon (Flamingo 2025.2 and later) consumes. **The dashboard is usable for resource management**: creating a network with a subnet, creating a volume and launching an instance all work from the UI, verified in stub and real mode (#532, #533). Open UI gaps: the noVNC console (F1), standalone port create in real mode (F10), router gateways (F11), project create (F13) and domain switching (F18). See the [Horizon Compatibility Report](docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md) (findings F1–F18).

**Features** — each works through the **API/CLI**; the trailing note is the *dashboard* status from the
[compatibility report](docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md):
- ✅ Instance lifecycle (launch, start, stop, delete, resize, rebuild) — ⚠️ the launch wizard's "Create New Volume" boots from the image without creating a volume (F16)
- ✅ VNC console access (integrated with noVNC proxy) — ❌ UI console shows no guest (F1); `openstack console log show` works
- ✅ Network topology visualization
- ✅ Volume management (create, attach, snapshot)
- ✅ Image management (upload, download, launch)
- ✅ Security groups and firewall rules
- ✅ Floating IPs and port forwarding
- ✅ Multi-project isolation and RBAC

**Deployment Guides**:
- **Unified Deployment** (Recommended): [docs/UNIFIED_DEPLOYMENT.md](docs/UNIFIED_DEPLOYMENT.md) - lightstack + Horizon in one docker-compose file
- **Separate Horizon**: [docs/HORIZON_DEPLOYMENT.md](docs/HORIZON_DEPLOYMENT.md) - Deploy Horizon separately to existing lightstack
- **Quick Reference**: [docs/QUICK_REFERENCE.md](docs/QUICK_REFERENCE.md) - Command cheat sheet
- **Integration Details**: [docs/HORIZON_INTEGRATION.md](docs/HORIZON_INTEGRATION.md) - Architecture and troubleshooting

**Version Requirements**:
- lightstack supports OpenStack **Flamingo (2025.2) and later versions only**
- Earlier OpenStack releases (Zed, Yoga, etc.) are **not supported**
- Use Horizon Flamingo image: `quay.io/openstack.kolla/horizon:2025.2-ubuntu-noble`

**Configuration**:

Horizon connects to lightstack services using standard OpenStack configuration:

```python
# local_settings.py
OPENSTACK_HOST = "lightstack"  # or your lightstack server address
OPENSTACK_KEYSTONE_URL = "http://%s:35357/v3" % OPENSTACK_HOST
OPENSTACK_API_VERSIONS = {
    "identity": 3,
    "image": 2,
    "volume": 3,
    "compute": 2,  # integer 2 — Horizon rejects 2.1 here
}
OPENSTACK_KEYSTONE_MULTIDOMAIN_SUPPORT = True
OPENSTACK_KEYSTONE_DEFAULT_DOMAIN = "Default"
SESSION_TIMEOUT = 14400  # 4 hours (deliberately shorter than lightstack's 24h token_ttl)
CONSOLE_TYPE = 'VNC'  # a Horizon CONSOLES key; 'novnc' is not one, and fails without calling Nova
```

**Login Credentials**: Domain=Default, User=admin, Password=secret

### 📖 Installation & Deployment

**Quick Start - Docker Compose (Recommended for Development)**:
- See [docs/QUICKSTART.md](docs/QUICKSTART.md) - Get running in 5 minutes
- See [docs/UNIFIED_DEPLOYMENT.md](docs/UNIFIED_DEPLOYMENT.md) - lightstack + Horizon in one command
- Works on ARM64 (Apple Silicon) and AMD64 (Intel/AMD)

**Automated Single-Node Deployment (For Demos with Real KVM)** ⭐ NEW:
- Interactive script: [scripts/deploy-single-node.sh](scripts/deploy-single-node.sh)
- One command deployment on Linux with KVM hypervisor
- Includes Horizon dashboard and noVNC console
- 15-20 minute automated installation
- See [scripts/README.md](scripts/README.md) for usage

**Production Multi-Node Deployment (For Scaling)** ⭐ NEW:
- See [docs/SCALING.md](docs/SCALING.md)
- Scale from 3-node HA cluster to 10+ node production
- High availability with HAProxy + Keepalived + Patroni
- Shared storage with Ceph, VXLAN multi-node networking
- Load balancing, monitoring, backup, disaster recovery

**Upgrading lightstack** ⭐ NEW:
- Automated upgrade script: [scripts/upgrade-lightstack.sh](scripts/upgrade-lightstack.sh)
- Safe upgrade with automatic backup and rollback
- Supports version pinning and force rebuild
- 2-5 minute upgrade process
- See [scripts/README.md](scripts/README.md) for usage

## Configuration

lightstack can be configured through YAML files or environment variables.

**Quick configuration:**
```yaml
# config/lightstack.yaml
database:
  url: "postgres://lightstack:secret@localhost/lightstack?sslmode=disable"

keystone:
  jwt_secret: "change-me-in-production"
  token_ttl: 24h

nova:
  libvirt_mode: stub  # "stub" or "real"

neutron:
  networking_mode: stub  # "stub", "iptables", or "ebpf"

cinder:
  storage_mode: local  # "stub", "local", "rbd", or hybrid like "local,rbd"

glance:
  storage_mode: local  # "stub", "local", "s3", or hybrid like "local,s3" (Ceph RGW supported)
```

**For complete configuration guide, see [docs/CONFIGURATION.md](docs/CONFIGURATION.md)**

## Development

### Project Structure

```
lightstack/
├── cmd/lightstack/                     # Main binary entry point
├── internal/                    # Private packages
│   ├── keystone/               # Identity service
│   ├── nova/                   # Compute service
│   ├── neutron/                # Network service
│   ├── cinder/                 # Block storage
│   ├── glance/                 # Image service
│   ├── database/               # DB models and migrations (73 migrations)
│   ├── middleware/             # Auth, logging, CORS, recovery
│   └── common/                 # Shared utilities
├── pkg/                         # Public/reusable packages
│   ├── hypervisor/             # libvirt abstraction (real + stub modes)
│   ├── networking/             # netlink, VXLAN, security groups
│   └── storage/                # Storage backends (RBD, S3, local)
├── migrations/                  # SQL migrations (146 files - 73 up/down pairs)
├── test/contract/              # Contract tests (97 test files, TDD-first)
├── test/*.sh                   # Integration tests (20+ bash scripts)
├── config/                     # Configuration files (YAML)
├── docs/                       # Documentation
└── deployments/                # Docker Compose and deployment configs
```

### Development Workflow

```bash
# First-time setup: activate the sign-off hook (requires @sap.com email in git config)
git config core.hooksPath .githooks
git config user.email "firstname.lastname@sap.com"

# Install development tools
make install-tools

# Run with hot reload
make dev

# Run tests
make test

# Run contract tests
./test/quick_test.sh

# Format code
make fmt

# Lint code
make lint
```

### CLI Subcommands

The binary ships operational subcommands alongside the API servers:

```bash
lightstack cleanup [--dry-run]          # Reclaim all host state: bridges, TAPs, dnsmasq,
                                         # iptables rules, legacy namespaces (SPEC-010)
lightstack healthcheck [--ready|--live] # Probe this node's health endpoints, exit 0/1 (SPEC-013)
lightstack console-tokens list          # Inspect issued noVNC console tokens (SPEC-021)
lightstack console-tokens purge         # Revoke console tokens
```

See `docs/OPERATIONS.md` and `docs/UPGRADING.md` for day-to-day management.

## Default Credentials

The seed data creates:

- **User:** `admin`
- **Password:** `secret`
- **Project:** `default`

**⚠️ Change these in production!**

## 📊 Project Status

### API Coverage: substantial (~335 functional operations vs the 330-endpoint baseline)

| Service | Status |
|---------|--------|
| Keystone (Identity) | ✅ Production Ready |
| Nova (Compute) | ✅ Production Ready |
| Neutron (Network) | ✅ Production Ready |
| Cinder (Block Storage) | ✅ Production Ready |
| Glance (Image) | ✅ Production Ready |
| Placement | ⚠️ Stub (version discovery + empty listings, for Horizon) |

**See [docs/blueprint.md](docs/blueprint.md) for the reconciled, per-resource coverage analysis.** (The earlier "104% (342/330)" figure counted raw routes — URL aliases and version discovery — and is superseded.)

**Priority Status**:
- ✅ **HIGH Priority**: 100% complete (all critical production features)
- ✅ **MEDIUM Priority**: 100% complete (all important management features)
- ⏳ **LOW Priority**: Optional only (enterprise SSO, DVR - <5% demand)

### Testing & Validation

- ✅ **Contract Tests**: **223/233 passing (95.7%)** - Using gophercloud SDK (OpenStack Go client), as of 2026-03-30
  - Keystone: 55/55 (100%)
  - Full report: [CONTRACT_TESTS_FINAL.md](docs/testing/CONTRACT_TESTS_FINAL.md)
- ✅ **TDD Methodology**: All endpoints developed test-first (RED → GREEN → REFACTOR)
- ✅ **20+ Integration Tests**: Bash scripts with OpenStack CLI validation
- ✅ **Schema Validation**: OpenStack API spec compliance
- ✅ **Horizon Testing**: browser-level verification done — create/write flows work in stub and real mode; the noVNC console is still open (F1) ([report](docs/compatibility/HORIZON_COMPATIBILITY_REPORT.md))
- ✅ **Client Compatibility**: OpenStack CLI, Terraform, gophercloud, python-openstackclient
- 🚀 **CI/CD Strategy**: Comprehensive E2E and automation plan - [E2E_CI_TESTING_STRATEGY.md](docs/testing/E2E_CI_TESTING_STRATEGY.md)

### Current Capabilities

**✅ Fully Implemented (~335 functional operations):**
- Complete server lifecycle management (create, start, stop, reboot, delete, resize, rebuild)
- All server actions (migrate, evacuate, rescue, snapshot, backup, password reset)
- Full networking (L3 routing, floating IPs, port forwarding, security groups, QoS)
- Multi-backend storage (local, Ceph RBD, S3) with automatic failover
- Volume operations (snapshots, backups, transfers, cloning, volume groups)
- Image management (upload, download, sharing, import workflow, metadefs)
- Multi-tenancy (domains, projects, users, groups, RBAC)
- Console access (noVNC in real mode; serial console log via CLI — SPICE/RDP are refused with a clear error)
- Tenant usage reporting and availability zones
- Service catalog management and credential management
- Performance optimization (pagination, indexes, <3s queries)

**⏳ Optional Work (not implemented by design):**
- Keystone Federation/SAML (~10 endpoints) - Enterprise SSO, <5% demand
- Neutron DVR/Advanced (~8 endpoints) - Large cloud provider features
- Nova Microversions (variable) - Incremental enhancements, add on-demand

**Strategic Decision**: lightstack exceeds the OpenStack baseline. Remaining features are enterprise-only with <5% demand, better implemented on-request based on user feedback.

### Performance Characteristics

**Stub Mode Benchmarks:**
- Token Creation: ~5ms
- Server List: ~10ms (100 servers)
- Network Create: ~8ms
- Volume Create: ~6ms

**Real Mode Benchmarks:**
- VM Creation: ~2-5s (depends on backend)
- Volume Attach: ~1-2s
- Network Setup: ~500ms
- Floating IP Associate: ~200ms

**Scalability:**
- Tested with 1000+ concurrent connections
- 10,000+ resources per project
- Sub-second response times maintained

### Roadmap

**Completed (v0.1-v0.6.1 + unreleased):**
- ✅ All 5 core OpenStack services (Keystone, Nova, Neutron, Cinder, Glance) + Placement stub
- ✅ Substantial coverage vs the 330-endpoint baseline (see [docs/blueprint.md](docs/blueprint.md))
- ✅ All HIGH and MEDIUM priority features
- ✅ Real libvirt/KVM integration with stub mode fallback
- ✅ Multi-backend storage (Ceph RBD, S3, hybrid failover)
- ✅ VXLAN multi-node networking
- ✅ L3 routing, floating IPs, and port forwarding
- ⚠️ Horizon dashboard: usable for resource management; open gaps include the noVNC console (findings F1–F18)
- ✅ Comprehensive test coverage (97 contract test files, 20+ integration tests)
- ✅ Production deployments validated
- ✅ Performance optimization (pagination, indexes)

**Current Focus (stability, real-mode polish & release process):**
- 🔧 Bug fixes and error handling improvements
- 🔧 Performance monitoring and optimization
- 🔧 Release engineering: version stamping, tagged releases, published images (see the review notes in `docs/review/`)

**Future Considerations (on-demand):**
- [ ] Additional services (Barbican, Designate, Octavia) - based on user requests
- [ ] Optional enterprise features (Federation/SAML, DVR) - if demanded
- [ ] eBPF-based security groups (kernel-space filtering performance boost)
- [ ] High availability (multi-node control plane)

**Philosophy**: lightstack has exceeded the OpenStack baseline. Focus is now on stability, performance, and real-world production hardening rather than speculative feature additions.

## 🤝 Contributing

Contributions welcome! See `docs/CONTRIBUTING.md` for guidelines.

**Current focus areas (stability phase):**
- Bug fixes and error handling improvements
- Performance monitoring and optimization
- Documentation and tutorials
- Release engineering (tagged releases, published container images, chart alignment)
- Real-world deployment validation
- Community feedback integration

**Future expansion areas (on-demand):**
- Optional enterprise features (Federation/SAML, DVR) - if user demand exists
- Additional services (Barbican, Designate, Octavia) - based on requests
- eBPF security groups
- High availability features

## 📚 Additional Documentation

**📖 Complete Documentation Index**: **[docs/INDEX.md](docs/INDEX.md)** - Full table of contents with learning paths and categorized guides.

### Getting Started
- **[Quick Start](docs/QUICKSTART.md)** - Get running in 5 minutes
- **[Installation Guide](docs/INSTALLATION.md)** - Complete setup instructions
- **[Quick Reference](docs/QUICK_REFERENCE.md)** - Command cheat sheet

### Deployment Guides
- **[Unified Deployment](docs/UNIFIED_DEPLOYMENT.md)** - lightstack + Horizon in one command
- **[Single-Node Deployment](docs/SINGLE_NODE_DEPLOYMENT.md)** ⭐ NEW - Real KVM on one host (demos/POC)
- **[Production Scaling](docs/SCALING.md)** ⭐ NEW - Multi-node HA cluster (3-node to 10+)
- **[Docker Deployment](docs/DOCKER_DEPLOYMENT.md)** - Container-specific guide
- **[Multi-Architecture](docs/MULTIARCH.md)** - ARM64 and AMD64 support

### Dashboard Integration
- **[Horizon Integration](docs/HORIZON_INTEGRATION.md)** - Integration overview (dashboard usable for resource management)
- **[Horizon Deployment](docs/HORIZON_DEPLOYMENT.md)** - Deploy Horizon separately
- **[Horizon Setup](docs/HORIZON_SETUP.md)** - Configuration and troubleshooting
- **[Elektra Analysis](docs/ELEKTRA_INTEGRATION_ANALYSIS.md)** ⭐ NEW - SAP Elektra (not compatible)

### Configuration & Operations
- **[Configuration Guide](docs/CONFIGURATION.md)** - All configuration options
- **[Operations Guide](docs/OPERATIONS.md)** - Day-to-day management
- **[Storage Modes](docs/STORAGE_MODES.md)** - Local, Ceph RBD, S3, hybrid
- **[Networking Modes](docs/NETWORKING_MODES.md)** - Stub, iptables, eBPF
- **[S3 Configuration](docs/S3_CONFIGURATION.md)** - AWS S3 and MinIO setup
- **[Troubleshooting](docs/TROUBLESHOOTING.md)** - Common issues and solutions

### Development & API
- **[Architecture](docs/ARCHITECTURE.md)** - System design and components
- **[API Coverage Report](docs/API_COVERAGE_REPORT.md)** - Superseded by [docs/blueprint.md](docs/blueprint.md)
- **[What's Left?](docs/WHATS_LEFT.md)** - Optional enterprise features overview
- **[API Reference](docs/API.md)** - OpenStack API compatibility details
- **[Contributing](docs/CONTRIBUTING.md)** - Development guidelines

### Advanced Topics
- **[VXLAN Implementation](docs/VXLAN_IMPLEMENTATION.md)** - Multi-node overlay networking
- **[L3 Router Implementation](docs/L3_ROUTER_IMPLEMENTATION.md)** - Routing and floating IPs
- **[Real Libvirt Mode](docs/REAL_LIBVIRT_MODE.md)** - KVM integration details
- **[eBPF Status](docs/EBPF_STATUS.md)** - eBPF security groups (experimental)

### Testing & Validation
- **[Contract Tests](test/contract/README.md)** - 223/233 passing (95.7%) using gophercloud SDK
  - Full results: [CONTRACT_TESTS_FINAL.md](docs/testing/CONTRACT_TESTS_FINAL.md)
- **[Integration Tests](test/)** - 20+ bash scripts for workflow validation
- **[E2E & CI Strategy](docs/testing/E2E_CI_TESTING_STRATEGY.md)** - Comprehensive testing and automation plan

**For complete documentation index with learning paths, see [docs/INDEX.md](docs/INDEX.md)**

## 📝 License

Apache License 2.0 - See [LICENSE](LICENSE)

## 🙏 Credits

**Project**: lightstack - OpenStack 3 Kubernetes-style
**Inspired by**: K3s (Lightweight Kubernetes)
**Language**: Go 1.27+
**Repository**: github.com/cobaltcore-dev/lightstack

---

**Status**: Unreleased (post-v0.6.1) | **Coverage**: ~335 functional operations vs 330-endpoint baseline | **Horizon**: usable for resource management
**Tests**: ✅ 223/233 Contract Tests PASS (95.7%, 2026-03-30)
**Updated**: October 12, 2026
