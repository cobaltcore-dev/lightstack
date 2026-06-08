# Implementation Plan: SPEC-007 Helm Prerequisites — Admin-Password Bootstrap + Configurable Catalog Scheme

**Branch**: `007-helm-prereqs-bootstrap-catalog` | **Date**: 2026-06-08 | **Spec**: [spec.md](./spec.md)
**Input**: Feature specification from `docs/specs/007-helm-prereqs-bootstrap-catalog/spec.md`

## Summary

Two small, config/bootstrap-level changes to the O3K binary that must land before the Helm chart (SPEC-008):

- **WS-2 (P1)** — Wire the existing-but-dead `keystone.admin_user` / `keystone.admin_password` config into a startup **bootstrap** that provisions the admin account as a **first-boot seed**: when configured, replace the seeded `admin/secret` (or create a custom-named admin + disable the seeded one), using bcrypt and atomic, idempotent SQL. Never reconciled/overwritten after provisioning; never logged.
- **WS-3 (P2)** — Add an `O3K_ENDPOINT_SCHEME` env var (default `http`) that rewrites the advertised scheme to `https` at presentation time, alongside the existing `O3K_ENDPOINT_HOST` host rewrite. Applies to **both** the Keystone catalog **and** `common.BaseURL` self-links/pagination (FR-013a). Catalog shape is unchanged. Assumes the chart terminates TLS **per service port** (port-based — Option 1), so `https://<host>:<port>` resolves.

Technical approach: a new `internal/keystone/bootstrap.go` called from `cmd/o3k/main.go` immediately after `database.MigrateUp`; a scheme-rewrite helper applied in `substituteURLTemplates` and `buildHardcodedCatalog` in `internal/keystone/auth.go`. No DB migration is added (seed retained; scheme is presentation-time).

## Technical Context

**Language/Version**: Go 1.26
**Primary Dependencies**: Gin (HTTP), pgx/pgxpool (`internal/database.DB` global), `golang.org/x/crypto/bcrypt`, zerolog; gophercloud + python-openstackclient for contract tests
**Storage**: PostgreSQL 18 — existing `users`, `domains`, `projects`, `roles`, `role_assignments`, `services`, `endpoints` tables. **No schema change.**
**Testing**: `go test ./internal/keystone/...` (unit) + bash integration in `test/` + SPEC-000 contract tests
**Target Platform**: Linux single binary (stub mode cross-platform); identical behavior single-node / multi-node
**Project Type**: Single Go binary (modular monolith) — `internal/` packages + `cmd/o3k`
**Performance Goals**: Bootstrap adds a handful of one-shot queries at startup (negligible); catalog rewrite is O(endpoints) string ops, result cached 24h
**Constraints**: No OpenStack API shape change / no microversion bump (FR-013, FR-015); no queues / synchronous only; no new external dependencies; fail-soft posture per clarifications (warn-and-continue, never crash on missing password or invalid scheme); never log the password (FR-006b); scheme also applies to `common.BaseURL` self-links (FR-013a); WS-3 assumes the chart exposes O3K with **per-service-port TLS** (port-based) so `https://<host>:<port>` resolves — single-`:443` path/subdomain ingress is out of scope
**Scale/Scope**: Tiny — one admin account; ~5 catalog services; ~3 source files touched + tests

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-check after Phase 1 design.*

| Principle | Assessment |
|-----------|------------|
| **I. Flamingo API compatibility (NON-NEGOTIABLE)** | PASS — catalog JSON shape unchanged (only URL scheme); no new/changed endpoints; auth flow unchanged. Verified by SPEC-000 contract tests (FR-016). |
| **II. Synchronous operations** | PASS — bootstrap runs synchronously at startup before serving; catalog rewrite is synchronous per request. No queues/async. |
| **III. Fail-fast design** | PASS — no external-dependency retries/backoff introduced. The deliberate *warn-and-continue* on missing password (Clarification: option C) / invalid scheme (option A) is a config-validation choice, not a dependency-timeout concern; the install-blocking gate is owned by the chart. |
| **IV. Multi-mode architecture** | PASS — both workstreams are pure config/DB/presentation logic with no external deps; identical in stub and real modes (FR-008). |
| **V. Project isolation** | PASS — bootstrap only provisions the admin and grants it the standard admin role on the seeded `default` project; no other identity's scoping changes (FR-007, FR-004b). |
| **VI. Single binary deployment** | PASS — bootstrap lives in `cmd/o3k/main.go` + `internal/keystone`; no new process/service. |
| **Security (bcrypt, no plaintext, no secrets in logs)** | PASS — reuses `HashPassword` (bcrypt DefaultCost); password value never logged (FR-006b). |
| **DB migrations reversible** | N/A — no migration added (seed retained; scheme is presentation-time). |

**Result**: All gates PASS. No violations → Complexity Tracking not required.

## Project Structure

### Documentation (this feature)

```text
docs/specs/007-helm-prereqs-bootstrap-catalog/
├── spec.md              # Feature spec (11 clarifications, decision-complete)
├── plan.md              # This file
├── research.md          # Phase 0 output
├── data-model.md        # Phase 1 output
├── quickstart.md        # Phase 1 output
├── contracts/           # Phase 1 output (observable contracts + SPEC-000 reference)
└── checklists/
    └── requirements.md  # Spec quality checklist (from /speckit.specify)
```

### Source Code (repository root)

```text
cmd/o3k/
└── main.go                      # WS-2: call keystone.BootstrapAdmin(ctx, cfg.Keystone) after database.MigrateUp

internal/keystone/
├── bootstrap.go                 # WS-2 (new): BootstrapAdmin — first-boot seed; custom user (domain_id + admin role) + neutralize seed
├── bootstrap_test.go            # WS-2 (new): unit tests (provision, idempotent, custom user, no-password warn)
├── auth.go                      # WS-3: scheme rewrite in substituteURLTemplates + buildHardcodedCatalog;
│                                #        WS-2 reuses HashPassword (bcrypt) — no change to verify path
└── auth_catalog_test.go         # WS-3 (new): catalog scheme unit tests (http default, https, invalid→http)

internal/common/
├── config.go                    # KeystoneConfig.AdminUser/AdminPassword already exist (no struct change needed)
└── baseurl.go                   # WS-3 (FR-013a): honor O3K_ENDPOINT_SCHEME in self-link/pagination URLs (line 13 currently pins http://)

test/
└── *.sh                         # integration: admin-password bootstrap + https catalog (extend existing suite)
```

**Structure Decision**: Single-binary modular monolith (existing O3K layout). WS-2 is isolated in a new `internal/keystone/bootstrap.go` invoked from `main.go`; WS-3 is a localized change in `internal/keystone/auth.go`. No new packages, no schema migration, no config-struct additions (fields already exist). `O3K_ENDPOINT_SCHEME` is read from the environment (mirrors `O3K_ENDPOINT_HOST`), so it needs no config-struct field either.

## Complexity Tracking

> No Constitution violations. Section intentionally empty.
