# Contributing to Lightstack

Thank you for considering contributing to lightstack! This document provides guidelines for contributing to the project.

---

## Table of Contents

- [Code of Conduct](#code-of-conduct)
- [Sign-off Requirement (DCO)](#sign-off-requirement-dco)
- [Getting Started](#getting-started)
- [Development Setup](#development-setup)
- [Making Changes](#making-changes)
- [Testing](#testing)
- [Submitting Changes](#submitting-changes)
- [Review Process](#review-process)
- [Code Style](#code-style)
- [Documentation](#documentation)
- [Areas Needing Help](#areas-needing-help)

---

## Code of Conduct

This project follows a standard code of conduct:
- Be respectful and inclusive
- Focus on constructive feedback
- Prioritize the project's goals and users
- Help others learn and grow

---

## Sign-off Requirement (DCO)

Every commit in a pull request must carry a `Signed-off-by` trailer with an `@sap.com` address:

```
Signed-off-by: Name <name@sap.com>
```

This is enforced by CI. The `dco-check` job (named **SAP Sign-off Check** in [`.github/workflows/ci.yml`](../.github/workflows/ci.yml)) inspects **every commit** in the PR — not just the head — and rejects any commit lacking a valid trailer. The aggregate **CI Status** job then fails the PR. The `@sap.com` address is a project requirement (corporate sign-off).

The easiest way to comply:

```bash
# Sign every commit as you create it
git commit -s

# Fix up an existing commit that is missing the trailer
git commit --amend -s

# Re-sign every commit in a PR at once (then force-push your branch)
git rebase --signoff <base>
```

Your git identity must be configured to the sap.com address (`git config user.email`). Sign-off certifies that you wrote the code and have the right to submit it under the project's license — the [Developer Certificate of Origin (DCO)](https://developercertificate.org/), the same certification used by the probot DCO app. In short: by signing off, you state that the contribution is yours (or properly attributed) and may be contributed to this project as open source.

> **Note**: force-pushing your *own* feature branch to add missing sign-offs is fine. Never force-push `main`, `stable/**`, or `releases/**` — see [Protected Branches](#protected-branches).

---

## Getting Started

### Prerequisites

**Required:**
- Go 1.27.x — use the exact version pinned in [`go.mod`](../go.mod)
- PostgreSQL 18 or higher
- Docker (for `make db-up` and the container-based test stacks)
- Git

**Optional (for real mode testing):**
- libvirt + KVM (Linux only)
- Ceph cluster (for RBD storage)
- AWS S3 / MinIO / Ceph RGW (for S3 storage)

### Fork and Clone

```bash
# Fork the repository on GitHub, then:
git clone https://github.com/YOUR_USERNAME/lightstack.git
cd lightstack
git remote add upstream https://github.com/cobaltcore-dev/lightstack.git
```

---

## Development Setup

### 1. Install Tools

```bash
# Install Go dependencies
make install-deps

# Install development tools: golangci-lint (pinned, built into .tools/bin),
# air (hot reload), the golang-migrate CLI, and Helm + the helm-unittest
# plugin needed by 'make chart-test'
make install-tools
```

### 2. Start PostgreSQL

```bash
# Dockerized PostgreSQL 18 (container name: lightstack-postgres)
make db-up

# Stop it again with
make db-down

# Or use your local PostgreSQL instance
```

### 3. Run Migrations

```bash
make migrate
```

### 4. Build and Run

```bash
# Build
make build

# Run (builds if needed, starts with config/lightstack.yaml)
make run

# Or run with hot reload (requires air)
make dev
```

`make run` applies any pending database migrations at startup (the binary
runs migrations on boot before serving), so `make migrate` is only needed
when you want to apply them ahead of time or against another database.

### 5. Verify Installation

```bash
# Test authentication
export OS_AUTH_URL=http://localhost:35357/v3
export OS_USERNAME=admin
export OS_PASSWORD=secret
export OS_PROJECT_NAME=default
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default

openstack token issue
```

---

## Making Changes

### Branch Naming

Use descriptive branch names:
- `feature/add-floating-ips` - New features
- `fix/volume-attachment-bug` - Bug fixes
- `refactor/storage-backend` - Code refactoring
- `docs/api-reference` - Documentation updates

### Protected Branches

The `main`, `stable/**`, and `releases/**` branches are push-protected:

- **Always** work in a feature branch and open a pull request — never commit directly to these branches.
- **Never** force-push to them. Force-pushing your own feature branch (e.g. to add a missing sign-off) is fine.

### Commit Messages

Follow conventional commits format:

```
<type>(<scope>): <description>

[optional body]

[optional footer]
```

**Types:**
- `feat`: New feature
- `fix`: Bug fix
- `refactor`: Code refactoring
- `docs`: Documentation changes
- `test`: Adding or updating tests
- `chore`: Maintenance tasks

**Examples:**
```
feat(glance): add S3 backend support for image storage

fix(nova): correct power_state in server detail response

refactor(cinder): simplify volume attachment logic

docs(readme): update installation instructions
```

Every commit must also carry a `Signed-off-by: Name <name@sap.com>` trailer —
use `git commit -s` and it is added automatically. CI rejects PRs containing
any commit without it; see [Sign-off Requirement (DCO)](#sign-off-requirement-dco).

### Code Organization

```
lightstack/
├── cmd/lightstack/               # Main binary entry point
├── internal/              # Internal packages (not importable)
│   ├── keystone/         # Keystone service implementation
│   ├── nova/             # Nova service implementation
│   ├── neutron/          # Neutron service implementation
│   ├── cinder/           # Cinder service implementation
│   ├── glance/           # Glance service implementation
│   ├── database/         # Database models and migrations
│   ├── middleware/       # HTTP middleware
│   └── common/           # Shared utilities
├── pkg/                   # Public packages (importable)
│   ├── hypervisor/       # libvirt abstraction
│   ├── networking/       # netlink abstraction
│   └── storage/          # Storage backends
├── migrations/            # SQL migrations
├── config/               # Configuration files
├── docs/                 # Documentation
└── test/                 # Integration tests
```

---

## Testing

### Unit Tests

```bash
# Run all unit tests (excludes test/contract — use the contract-test flow below for those)
make test

# Run tests for specific package
go test ./internal/keystone/...

# Run with coverage
go test -cover ./...
```

**DB-backed unit tests.** Some unit tests (e.g. keystone admin bootstrap, nova
handler tests) run against a real PostgreSQL and **skip** when no test database
is configured. To run them, point `LIGHTSTACK_TEST_DB_URL` at a scratch
database — CI does exactly this:

```bash
# One-time: create the scratch database (the test harness connects and
# migrates, but does not create the database itself)
docker exec lightstack-postgres psql -U lightstack -c 'CREATE DATABASE lightstack_test'

LIGHTSTACK_TEST_DB_URL="postgres://lightstack:secret@localhost:5432/lightstack_test?sslmode=disable" \
  go test $(go list ./... | grep -v '/test/contract')
```

### Contract Tests

Contract tests (`test/contract/`) verify OpenStack API compatibility through
the real gophercloud client libraries. They need a running Lightstack:

```bash
# 1. Build and start the server (make db-up + make run also work)
make build
./bin/lightstack --config config/lightstack.yaml &

# 2. Point the OpenStack environment at it
export OS_AUTH_URL=http://localhost:35357/v3
export OS_USERNAME=admin
export OS_PASSWORD=secret
export OS_PROJECT_NAME=default
export OS_USER_DOMAIN_NAME=Default
export OS_PROJECT_DOMAIN_NAME=Default

# 3. Run the suite (bypass the test cache so a stale PASS never survives an environment change)
go clean -testcache && go test ./test/contract/... -timeout 10m
```

`make test-contract` wraps the same suite (with a 5-minute timeout) and checks
the server is up first.

**Known environmental failures — the baseline.** Five contract tests seed
their fixtures through `docker exec lightstack-postgres psql`:

- `TestNeutronGetAgent_Contract`
- `TestNeutronAddL3AgentToRouter_Contract`
- `TestTrunkBasicCRUD`
- `TestTrunkSubports`
- `TestCinderUnmanageVolume_Contract`

They need a running container named `lightstack-postgres` — which the compose
stack and `make db-up` both provide. On a bare host (PostgreSQL running
natively, no such container) these five fail identically on every run. That is
the known environmental baseline, not a product regression: compare your run
against it before investigating a failure.

See [`test/contract/README.md`](../test/contract/README.md) for how to write
contract tests, and [`docs/testing/CONTRACT_TEST_RESULTS.md`](testing/CONTRACT_TEST_RESULTS.md)
for current results.

### Helm Chart Tests

```bash
# Lint + render + unit-test both charts (charts/lightstack, charts/horizon).
# No cluster and no running Lightstack needed.
make chart-test
```

Requires the Helm CLI and the helm-unittest plugin — both are installed by
`make install-tools`. This is the same check CI's "Helm Charts" job runs.

### Linting

```bash
# golangci-lint (pinned in the isolated tools module, built into .tools/bin)
make lint
```

### Integration Tests

```bash
# Run full integration test suite (requires a running Lightstack)
./test/quick_test.sh

# Run Horizon compatibility tests
./test/horizon_compat_test.sh
```

### Manual Testing

```bash
# Test authentication
openstack token issue

# Test compute
openstack server create --flavor m1.small --image cirros --network private test-vm
openstack server list
openstack server delete test-vm

# Test networking
openstack network create test-network
openstack subnet create --network test-network --subnet-range 192.168.1.0/24 test-subnet
openstack network delete test-network

# Test storage
openstack volume create --size 10 test-volume
openstack volume list
openstack volume delete test-volume

# Test images
openstack image create --file cirros.img --disk-format qcow2 test-image
openstack image list
openstack image delete test-image
```

---

## Submitting Changes

### Pull Request Process

1. **Update your fork:**
   ```bash
   git fetch upstream
   git rebase upstream/main
   ```

2. **Run tests:**
   ```bash
   make test                                   # unit tests
   make lint                                   # golangci-lint
   make chart-test                             # Helm charts (if touched)
   # contract tests against a running server — see Testing above
   go clean -testcache && go test ./test/contract/... -timeout 10m
   ```

3. **Format code:**
   ```bash
   make fmt
   ```

4. **Check sign-off:** every commit must carry `Signed-off-by: Name <name@sap.com>`
   (`git commit -s`). Fix missing trailers with `git commit --amend -s` or
   `git rebase --signoff`, then force-push your branch.

5. **Push changes:**
   ```bash
   git push origin feature/your-feature-name
   ```

6. **Create Pull Request:**
   - Go to GitHub and create a PR from your fork
   - Fill out the PR template
   - Link any related issues

### Pull Request Checklist

- [ ] Every commit is signed off (`Signed-off-by: Name <name@sap.com>`)
- [ ] Unit tests pass (`make test`, DB-backed tests included via `LIGHTSTACK_TEST_DB_URL`)
- [ ] Contract tests pass against the known baseline (see [Testing](#testing))
- [ ] Helm chart tests pass (`make chart-test`) if charts were touched
- [ ] Code is formatted (`make fmt`)
- [ ] Linter passes (`make lint`)
- [ ] Documentation is updated
- [ ] Commit messages follow conventional format
- [ ] PR description is clear and complete
- [ ] Related issues are linked

---

## Review Process

Expect **multiple review rounds** — this is the norm, not a red flag. Reviews
here are comprehensive: a round walks the whole diff, reports every finding
with a severity and a rationale, and the PR is not merged until a round comes
back with a clean verdict (no findings). Finding and fixing real defects over
two or three rounds is the process working as intended.

How a round-trip looks:

1. **Review round** — findings are reported, each with a severity
   (critical / high / medium / low) and the reasoning behind it.
2. **Fix commits** — each round's findings are addressed in dedicated
   commits (not squashed away), so the history shows what was found and how
   it was resolved.
3. **Re-run** — the review loop re-runs on the updated branch and either
   reports the next round's findings or returns a clean verdict.
4. **Archive** — the final review reports are kept under
   [`docs/review/`](review/) for reference (see the existing reports there
   for the format).

### What a good fix commit looks like

A fix commit references the finding it addresses, explains the defect (not
just the change), and says how the fix is pinned by a test. This is a real
fix commit from the round-2 loop of an open PR (`fix/authz-and-correctness`, commit
`0e14680` — not yet in `main` when this was written; lightly trimmed):

```
fix: round-2 review findings — os-interface port_id path, upload-cap 413

Round 2 of the review loop (0 High, 1 Medium, 4 Low — all fixed):

MEDIUM (F1) — the port_id path of os-interface scanned the ports row's
  nullable device_id into a plain string. Every free port has NULL
  there, so the scan FAILED on every successful attach; the error was
  dropped and the attach worked only because pgx fills earlier columns
  before the failed one — and any real database error on that lookup
  surfaced as a bogus 200 with an empty net_id and MAC. The lookup now
  reads COALESCE(device_id::text, '') and handles its errors like the
  instance lookup above it. Pinned by
  TestAttachInterfacePortIDReportsRealState (attach a device_id-NULL
  port, assert the response carries the port's real net_id, MAC and
  fixed_ips).

LOW (F4) — uploadImageLocal applied the 5-second metadata-op timeout
  to the body copy, so an over-cap upload hit the deadline (503) long
  before the 10 GiB MaxBytesReader cap could answer 413. The copy now
  runs under the caller's context only (cancelled on client
  disconnect), matching the S3 path's connect-only timeout policy
  (FR-014). maxImageUploadBytes is a var now, and
  TestOverCapUploadAnswers413 pins the 413 end-to-end (image left
  queued, not stuck saving).

Unit suite (incl. DB-backed): PASS. Contract suite: unchanged (only
the 5 docker-exec-dependent tests fail, same as baseline).

Signed-off-by: Name <name@sap.com>
```

Points that matter:

- The subject follows conventional commits and names the round.
- Each finding is referenced by its ID and severity, and its defect is
  explained — the *why*, not just the *what*.
- The commit states which test pins each fix, so the finding cannot silently
  regress.
- The closing lines report the verification runs (and note any known
  environmental baseline failures rather than hiding them).

---

## Code Style

### Go Style Guidelines

Lightstack follows standard Go conventions:

1. **Naming:**
   - Use `camelCase` for local variables
   - Use `PascalCase` for exported functions/types
   - Use descriptive names (`imageStore` not `is`)

2. **Error Handling:**
   ```go
   // Good
   if err != nil {
       return fmt.Errorf("failed to create volume: %w", err)
   }

   // Bad
   if err != nil {
       return err
   }
   ```

3. **Context:**
   - Always pass `context.Context` as first parameter
   - Use `ctx` as the variable name
   - Respect context cancellation

4. **Comments:**
   - Public functions must have doc comments
   - Comments should explain "why", not "what"
   - Use complete sentences

5. **Imports:**
   - Standard library first
   - Third-party packages second
   - Internal packages last

**Example:**
```go
package glance

import (
    "context"
    "fmt"
    "net/http"

    "github.com/gin-gonic/gin"
    "github.com/google/uuid"

    "github.com/cobaltcore-dev/lightstack/internal/database"
    "github.com/cobaltcore-dev/lightstack/pkg/storage"
)

// CreateImage creates a new image in the image service.
// It returns the image ID and any error encountered.
func (svc *Service) CreateImage(ctx context.Context, req CreateImageRequest) (string, error) {
    if err := validateImageRequest(req); err != nil {
        return "", fmt.Errorf("invalid image request: %w", err)
    }

    imageID := uuid.New().String()
    // ... implementation
    return imageID, nil
}
```

### HTTP Response Format

Follow OpenStack API conventions:

```go
// Success (200/201)
c.JSON(http.StatusOK, gin.H{
    "resource": gin.H{
        "id":   resourceID,
        "name": name,
        // ... fields
    },
})

// List (200)
c.JSON(http.StatusOK, gin.H{
    "resources": []gin.H{
        // ... items
    },
})

// Error (4xx/5xx)
c.JSON(http.StatusBadRequest, gin.H{
    "error": gin.H{
        "message": "Invalid request",
        "code":    400,
        "title":   "Bad Request",
    },
})
```

---

## Documentation

### Code Documentation

- All exported functions must have doc comments
- Use examples for complex functions
- Document error conditions

### User Documentation

When adding features, update:
- `README.md` - If it affects quick start
- `docs/STORAGE_MODES.md` - If it adds storage options
- `docs/REAL_LIBVIRT_MODE.md` - If it affects VM lifecycle
- `CHANGELOG.md` - For every notable change (keep-a-changelog format)
- `docs/WHATS_LEFT.md` - If it opens or closes a known gap

### Example Documentation

```go
// CreateVolume creates a new block storage volume.
//
// The volume is created using the configured storage backend
// (local, RBD, or S3). If the backend is unavailable, it returns
// an error immediately (fail-fast).
//
// Parameters:
//   - ctx: Request context (respects cancellation)
//   - size: Volume size in GB (1-1000)
//   - name: Optional volume name
//
// Returns:
//   - volumeID: UUID of the created volume
//   - error: Any error encountered
//
// Example:
//   volumeID, err := svc.CreateVolume(ctx, 10, "my-volume")
//   if err != nil {
//       log.Fatalf("Failed to create volume: %v", err)
//   }
func (svc *Service) CreateVolume(ctx context.Context, size int, name string) (string, error) {
    // ... implementation
}
```

---

## Areas Needing Help

Cross-check anything here against [`docs/WHATS_LEFT.md`](WHATS_LEFT.md) and
[`CHANGELOG.md`](../CHANGELOG.md) before starting — this list only carries
items that are still open. (Floating IPs and router L3 forwarding used to be
listed here; both have shipped — see `internal/neutron/floatingip.go` and
[`docs/L3_ROUTER_IMPLEMENTATION.md`](L3_ROUTER_IMPLEMENTATION.md).)

### High Priority

1. **Multi-node Networking**
   - Cross-node VM communication (VXLAN overlay)
   - Multi-node real mode is the open Part B of the single-node real-mode
     work — see the SPEC-009 note in
     [`docs/WHATS_LEFT.md`](WHATS_LEFT.md) and
     [`docs/VXLAN_IMPLEMENTATION.md`](VXLAN_IMPLEMENTATION.md)
   - Distributed DHCP

### Medium Priority

2. **eBPF Security Groups**
   - East-west XDP filtering: the eBPF object does not load in a stock
     build, so eBPF mode currently adds no east-west filtering — see
     [`docs/EBPF_STATUS.md`](EBPF_STATUS.md) for the exact state and what
     remains
   - Kernel-space filtering, performance improvement over iptables

3. **Live Migration**
   - VM migration between hosts
   - Storage migration
   - Downtime minimization

4. **High Availability**
   - Multi-node control plane
   - Leader election
   - Failover handling

### Low Priority

5. **Heat Orchestration**
   - Template parsing
   - Stack creation
   - Resource dependencies

6. **Placement API**
   - Currently a stub — resource scheduling, affinity/anti-affinity,
     resource providers (see [`STATUS.md`](../STATUS.md) — "Placement API" future work; current
     shape in [`internal/placement/placement.go`](../internal/placement/placement.go))

The full roadmap, including items deliberately *not* being built, lives in
[`ROADMAP.md`](../ROADMAP.md) and [`docs/WHATS_LEFT.md`](WHATS_LEFT.md).

---

## Getting Help

- **GitHub Issues**: https://github.com/cobaltcore-dev/lightstack/issues
- **Documentation**: `docs/` directory
- **Code Examples**: `test/` directory

---

## License

By contributing to lightstack, you agree that your contributions will be licensed under the Apache License 2.0.

---

**Thank you for contributing to lightstack!**
