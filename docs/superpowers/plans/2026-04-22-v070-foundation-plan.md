# v0.7.0 Foundation — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Execute all 7 steps from [SPEC-006](../../specs/006-v070-foundation/README.md) to harden lightstack's codebase quality and add trust signals for CTO evaluation.

**Spec**: [SPEC-006](../../specs/006-v070-foundation/README.md)
**ADRs**: [ADR-001](../../adr/ADR-001-database-dependency-injection.md), [ADR-002](../../adr/ADR-002-rate-limiting-architecture.md)
**Tech Stack:** Go 1.26, Gin, pgx v5, golang.org/x/time/rate, golangci-lint, Terraform OpenStack provider

## Phase Overview

| Phase | Scope | Steps | Parallel-Safe | Est. Tasks |
|-------|-------|-------|---------------|------------|
| 1 | CI & Safety Net | Steps 1-3 | Yes (different dirs) | 5 |
| 2 | Database DI | Step 4 | No (touches all pkgs) | 8 |
| 3 | Rate Limiting | Step 5 | Yes (new middleware) | 4 |
| 4 | Cleanup | Steps 6-7 | Partially | 4 |

**Total: ~21 tasks**

---

## Phase 1: CI & Safety Net (Steps 1-3)

**Branch:** `fix/ci-linting-and-conformance`
**Parallel-safe:** Yes (lint touches CI/test files, conformance touches test/terraform/, test mapping is docs)

### Task 1.1: Fix test file lint errors

- [ ] Run `golangci-lint run ./...` to identify all current errors
- [ ] Fix the 78 test file errors
- [ ] Configure `.golangci.yml` with staged config (disable `unused` for now)
- [ ] Verify: `make lint` passes locally

### Task 1.2: Re-enable linting in CI

- [ ] Update `.github/workflows/ci.yml`: replace echo stub with actual `golangci-lint run`
- [ ] Verify: push to branch, CI lint step passes

### Task 1.3: Create Terraform conformance configs

- [ ] Create `test/terraform/` directory
- [ ] Write 15 `.tf` files (one per resource type from SPEC-006)
- [ ] Create `test/terraform/run-conformance.sh` wrapper script
- [ ] Add conformance job to `.github/workflows/ci.yml` (sequential, after Lightstack starts)
- [ ] Verify: `terraform init && terraform plan` succeeds for each config against running Lightstack

### Task 1.4: Map failing contract tests

- [ ] Run `go test -v ./... 2>&1 | grep SKIP` in `test/contract/`
- [ ] Create `docs/testing/FAILING_TEST_MAP.md` with table: service, test, reason, overlap with v0.7.0 scope
- [ ] Verify: all 28 skips accounted for

---

## Phase 2: Database Dependency Injection (Step 4)

**Branch:** `refactor/database-dependency-injection`
**Parallel-safe:** No (touches all service packages)
**ADR:** [ADR-001](../../adr/ADR-001-database-dependency-injection.md)

### Task 2.1: Update WithTx and database package

- [ ] Add pool parameter to `WithTx()`: `WithTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error`
- [ ] Update 3 existing callers (nova/handlers.go:1703, cinder/volumes.go:1447, cinder/volumes.go:1656)
- [ ] Keep `database.DB` global alive (not removing yet)
- [ ] Verify: `go build ./...` passes, contract tests pass

### Task 2.2: Migrate metadata + compute (11 refs)

- [ ] Add `pool *pgxpool.Pool` to metadata and compute service structs
- [ ] Replace `database.DB.` with `svc.pool.` in all handlers
- [ ] Update `NewService()` constructors and callers in `main.go`
- [ ] Verify: `go build ./...` passes

### Task 2.3: Migrate glance (53 refs)

- [ ] Same pattern as Task 2.2 for glance package (5 files)
- [ ] Verify: contract tests for glance pass

### Task 2.4: Migrate keystone (85 refs)

- [ ] Same pattern for keystone package (7 files, including auth.go)
- [ ] Verify: token issuance works, contract tests pass

### Task 2.5: Migrate cinder (116 refs)

- [ ] Same pattern for cinder package (9 files)
- [ ] Verify: contract tests for cinder pass

### Task 2.6: Migrate neutron (184 refs)

- [ ] Same pattern for neutron package (15 files)
- [ ] Verify: contract tests for neutron pass

### Task 2.7: Migrate nova (212 refs)

- [ ] Same pattern for nova package (15 files)
- [ ] Verify: contract tests for nova pass

### Task 2.8: Remove global database.DB

- [ ] Remove `var DB *pgxpool.Pool` from `internal/database/db.go`
- [ ] Update `database.Connect()` to return the pool instead of setting global
- [ ] Update `main.go` to capture returned pool
- [ ] Remove `database.Close()` (caller manages pool lifecycle)
- [ ] Verify: `go build ./...` passes (compiler catches any missed refs), ALL contract tests pass
- [ ] Re-enable `unused` linter in `.golangci.yml`

---

## Phase 3: Rate Limiting (Step 5)

**Branch:** `feat/rate-limiting`
**Parallel-safe:** Yes (new files in middleware/)
**ADR:** [ADR-002](../../adr/ADR-002-rate-limiting-architecture.md)

### Task 3.1: Add rate limiting config

- [ ] Add `RateLimitingConfig` struct to `internal/common/config.go`
- [ ] Add `rate_limiting` field to `Config` struct
- [ ] Config already documented in `config/lightstack.yaml` (added in docs update)

### Task 3.2: Implement rate limiting middleware

- [ ] Create `internal/middleware/ratelimit.go`
- [ ] Implement `RateLimitMiddleware(cfg common.RateLimitingConfig) gin.HandlerFunc`
- [ ] IP extraction: `X-Forwarded-For` header, fallback to `c.ClientIP()`
- [ ] Project extraction: from auth middleware context (`c.GetString("project_id")`)
- [ ] Eviction goroutine with configurable interval
- [ ] Max entries cap with graceful degradation
- [ ] Return 429 via `common.SendError(c, common.NewRateLimitError("rate limit exceeded"))`

### Task 3.3: Register middleware in main.go

- [ ] Add `RateLimitMiddleware` to all 7 HTTP server factories
- [ ] Only register if `cfg.RateLimiting.Enabled` is true

### Task 3.4: Write rate limiter tests

- [ ] Create `internal/middleware/ratelimit_test.go`
- [ ] Implement all 12 unit tests + 1 integration test from SPEC-006
- [ ] Verify: `go test -race ./internal/middleware/...` passes

---

## Phase 4: Cleanup (Steps 6-7)

**Branch:** `fix/route-dedup-and-pagination`
**Parallel-safe:** Partially (cinder routes and pagination touch different files)

### Task 4.1: Audit Cinder routes

- [ ] List all duplicate routes in `internal/cinder/volumes.go:38-166`
- [ ] Identify which paths the Terraform provider hits (check conformance test logs)
- [ ] Create route inventory document

### Task 4.2: Redirect old Cinder routes

- [ ] Add 301 redirects from legacy paths to canonical v3 paths
- [ ] Verify: both old and new paths return correct responses
- [ ] Verify: Terraform conformance tests still pass

### Task 4.3: Audit pagination handlers

- [ ] List all 34 list handlers
- [ ] Identify which use `ParsePagination()` (10), inline parsing (24), or no pagination
- [ ] Skip non-paginated handlers

### Task 4.4: Migrate remaining pagination handlers

- [ ] Replace inline `c.Query("limit")` / `c.DefaultQuery("limit", ...)` with `common.ParsePagination()`
- [ ] Verify: contract tests for list endpoints pass

---

## Out of Scope (Separate Plans)

- **Load testing** — P2 TODO, needs `wrk` or `hey` benchmark after rate limiter ships
- **DevStack comparison** — Infrastructure provisioning task, follows conformance configs
- **Config struct changes** — `RateLimitingConfig` in config.go is code work in Phase 3
- **New services** — Barbican, Designate, Octavia are Phase 3 of roadmap
