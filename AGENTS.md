# AGENTS.md

Guidance for any coding agent (Claude Code, Codex, Cursor, …) working in this repository.
For deeper Claude Code-specific guidance see `CLAUDE.md`; contribution process: `docs/CONTRIBUTING.md`.

## What this is

Lightstack is a single Go binary that reimplements the five core OpenStack services
(Keystone, Nova, Neutron, Cinder, Glance) plus a Placement stub and an EC2-compatible
metadata service — inspired by how K3s simplified Kubernetes. One process, one PostgreSQL
database, zero message queues, synchronous APIs.

**SPEC-000: OpenStack API compliance is priority zero.** Upstream OpenStack behavior is the
reference implementation — never invent semantics. If Lightstack and upstream disagree,
Lightstack is wrong. All other specifications must comply with SPEC-000
(`docs/specs/SPEC-000-COMPLIANCE-ADDENDUM.md`).

One process, seven HTTP listeners: Keystone `:35357`, Nova `:8774`, Neutron `:9696`,
Cinder `:8776`, Glance `:9292`, Placement `:8778`, Metadata `:8775` (unauthenticated).

Each service supports **stub mode** (fake data, no external dependencies — default, works on
macOS) and **real mode** (libvirt, netlink, Ceph RBD, S3 — Linux only), selected per service in
`config/lightstack.yaml` (`nova.libvirt_mode`, `neutron.networking_mode`,
`cinder.storage_mode`, `glance.storage_mode`). Storage modes may be comma-separated hybrids
(`local,rbd`). Note: the `rate_limiting.*` config block is **dead config** — no code reads it.

## Repo map

| Path | Contents |
|------|----------|
| `cmd/lightstack/` | Main binary: server plus subcommands `cleanup` (SPEC-010 teardown), `console-tokens` (SPEC-021), `healthcheck` (SPEC-013) |
| `cmd/lightstack-migrate/` | Standalone migration binary |
| `internal/<service>/` | One package per OpenStack service: `keystone`, `nova`, `neutron`, `cinder`, `glance`, plus `placement`, `metadata` |
| `internal/common/` | Config loading, error helpers/pagination, `BaseURL`, endpoint `scheme`, SQL builder, roles |
| `internal/database/` | pgx pool usage, models, `WithTx` transactions, migration wiring (golang-migrate) |
| `pkg/paths/` | `state_path` resolution (node-local state root, default `/var/lib/lightstack`) |
| `pkg/hypervisor`, `pkg/networking`, `pkg/storage` | libvirt, netlink/VXLAN, and storage backends (local/RBD/S3) |
| `test/contract/` | gophercloud contract tests — the OpenStack API compliance gate |
| `test/*.sh`, `test/terraform/` | Bash integration suites (OpenStack CLI) and Terraform conformance |
| `docs/specs/NNN-*/` | Feature specs: `spec.md`, `plan.md`, `tasks.md`, `contracts/`, `checklists/` |
| `docs/adr/` | Architecture decision records |
| `docs/review/` | Comprehensive review reports (fix loops run until CLEAN) |
| `charts/lightstack/`, `charts/horizon/` | Helm charts |
| `deployments/` | Docker compose stacks, horizon config, systemd units |
| `migrations/` | Numbered `NNN_name.up.sql` / `.down.sql` migrations |

## Ground truth commands

```bash
make build          # Build bin/lightstack
make lint           # golangci-lint (pinned via tools/ module)
make test           # Unit tests (excludes test/contract)
make test-contract  # gophercloud contract tests — needs a RUNNING server
                    #   (docker compose -f deployments/docker-compose.yml up -d, or make run)
make chart-test     # Helm lint + template + unittest (needs helm + helm-unittest; make install-tools)
```

DB-backed unit tests need an existing scratch database they may **migrate**: the
harness connects to the database named in `LIGHTSTACK_TEST_DB_URL` and applies all
migrations — it does **not** create the database (see `internal/common/maindb_test.go`),
so never point it at anything shared:

```bash
make db-up   # start dev PostgreSQL in Docker
LIGHTSTACK_TEST_DB_URL=postgres://lightstack:secret@localhost:5432/postgres make test
make migrate # apply migrations (golang-migrate)
```

Without `LIGHTSTACK_TEST_DB_URL`, DB-backed unit tests skip rather than fail.

## Hard rules

- **DCO sign-off on every commit**: `git commit -s`, `Signed-off-by: Name <name@sap.com>`.
  The "SAP Sign-off Check" CI job rejects any PR commit missing a valid `@sap.com` trailer.
- **Conventional commits**: `feat(nova): …`, `fix(cinder): …`, `refactor(glance): …`,
  `docs`, `test`, `chore`.
- **Branches**: `main`, `stable*`, and `releases*` are protected — work on PR branches only.
- **API changes need contract tests**: any endpoint added or behavior changed gets a test in
  `test/contract/` (gophercloud). This is the SPEC-000 gate CI runs.
- **Tenant isolation and admin-only surfaces are security-critical** — see
  `docs/specs/022-project-tenant-isolation/` and `docs/specs/023-tenant-scoping-authz/`.
  Resources are scoped by `project_id`; the `admin` role override and `?all_tenants` follow
  upstream `admin_or_owner` semantics. Do not loosen scoping predicates.
- **Never commit secrets** (JWT secrets, passwords, keys). The seeded `admin`/`secret`
  credentials are development-only defaults.

## Working agreements

- **Significant features follow the spec pattern** (`docs/specs/README.md`): a numbered
  `docs/specs/NNN-name/` directory with `spec.md` → `plan.md` → `tasks.md` → implementation →
  validation. Read the spec covering the code you touch before changing it.
- **Expect review-fix loops**: comprehensive reviews (`docs/review/`) produce findings that are
  fixed and re-reviewed until the report is CLEAN. Budget for iteration.
- **Changelog entries** for user-visible changes (`CHANGELOG.md`).
- **Never edit an applied migration's meaning** — add a new numbered migration instead.
- Prefer the shared helpers over re-implementations: `common.BaseURL`, `common.HasRole`,
  `nova.targetInstance`, `database.WithTx`. The recurring review finding is "the right shared
  thing exists and one caller bypasses it."
- **Test-first**: write the failing test (unit, contract, or integration script) before the
  implementation; confirm it fails, then make it pass.
- Errors: wrap with `fmt.Errorf("...: %w", err)` and surface HTTP errors via the helpers in
  `internal/common/error_helpers.go` — never leak raw error strings to API clients.
