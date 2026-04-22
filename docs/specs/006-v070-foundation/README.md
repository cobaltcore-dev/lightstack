# SPEC-006: v0.7.0 Foundation — Architecture & Quality

**Status**: Draft
**Version**: 1.0
**Created**: 2026-04-20
**Priority**: High
**Compliance**: Must pass SPEC-000 (OpenStack API Compatibility)
**ADRs**: [ADR-001](../../adr/ADR-001-database-dependency-injection.md), [ADR-002](../../adr/ADR-002-rate-limiting-architecture.md)

## Overview

v0.7.0 is an internal hardening release that addresses the remaining findings from the April 2026 codebase review (H-3, H-4, H-6, H-8) plus two new quality initiatives (Terraform conformance testing, pagination standardization). No new API endpoints. No new services. No breaking changes.

The release was shaped by two formal reviews: a CEO-level scope review and an engineering architecture review, both with independent AI outside voices.

## Goals

1. Eliminate the global database pool and inject dependencies through constructors
2. Re-enable CI linting with a staged configuration
3. Create a public Terraform conformance test suite
4. Add rate limiting to protect against brute-force and resource exhaustion
5. Consolidate duplicate Cinder routes via redirects
6. Standardize pagination across all list handlers
7. Map all failing contract tests by service area

## Non-Goals

- Load testing (deferred, tracked as TODO)
- DevStack comparison infrastructure (deferred, conformance configs ship without it)
- New services (Barbican, Designate, Octavia — Phase 3 of roadmap)
- Interface-based DB injection (direct pool injection chosen per ADR-001)
- Per-endpoint rate limiting (two-tier sufficient per ADR-002)
- Modular service splitting (Phase 1 of roadmap)

## Execution Sequence

### Step 1: CI Linting

**Finding**: H-4 (CI linting disabled, 78 test file errors)

**Approach**:
- Fix 78 lint errors in test files
- Re-enable golangci-lint in `.github/workflows/ci.yml`
- Use staged configuration: disable `unused` linter during DI migration (Step 4)
- Re-enable full lint rules after DI migration completes

**Validation Gate**: `make lint` passes on CI for all PRs

### Step 2: Terraform Conformance Configs

**Finding**: New (identified by outside voice during CEO review)

**Approach**:
- Create `test/terraform/` directory with 15-20 Terraform configurations
- Each config exercises one `openstack_*` resource type (create, read, update, destroy)
- Run sequentially in CI against O3K (no parallelism, avoids connection pool exhaustion)
- No DevStack comparison yet (infrastructure-heavy, deferred)
- Pin Terraform OpenStack provider version for CI stability

**Resources to test**:
- `openstack_compute_instance_v2`
- `openstack_compute_flavor_v2`
- `openstack_compute_keypair_v2`
- `openstack_networking_network_v2`
- `openstack_networking_subnet_v2`
- `openstack_networking_router_v2`
- `openstack_networking_secgroup_v2`
- `openstack_networking_secgroup_rule_v2`
- `openstack_networking_floatingip_v2`
- `openstack_networking_port_v2`
- `openstack_blockstorage_volume_v3`
- `openstack_identity_project_v3`
- `openstack_identity_user_v3`
- `openstack_identity_role_v3`
- `openstack_images_image_v2`

**Validation Gate**: All Terraform configs apply and destroy cleanly against O3K in CI

### Step 3: Map Failing Tests

**Finding**: 28 contract tests are skipped/failing (Cinder 5, Nova 13, Neutron 2, Keystone 2, Glance 5, Template 9)

**Approach**:
- Document all 28 skips in a tracking table (service, test name, reason)
- Verify none of the Cinder skips overlap with routes being redirected in Step 6
- Do not fix — mapping only, provides regression awareness

**Validation Gate**: Tracking document exists with all 28 skips accounted for

### Step 4: Database Dependency Injection

**Finding**: H-3 (665 global `database.DB` references across 53 files)
**ADR**: [ADR-001](../../adr/ADR-001-database-dependency-injection.md)

**Approach**:
- Direct `*pgxpool.Pool` injection through service constructors
- Same pool for all services (DI for architecture, not isolation)
- Service-by-service migration (smallest first):
  1. metadata (8 refs, 1 file)
  2. compute (3 refs, 1 file)
  3. glance (53 refs, 5 files)
  4. keystone (85 refs, 7 files)
  5. cinder (116 refs, 9 files)
  6. neutron (184 refs, 15 files)
  7. nova (212 refs, 15 files)
- `WithTx()` gets pool parameter: `WithTx(ctx, pool, fn)`
- Global `var DB *pgxpool.Pool` stays alive during transition
- Remove global as final step (compiler catches stragglers)

**Validation Gate**: Zero `database.DB` references remaining; all 258 contract tests pass

### Step 5: Rate Limiting

**Finding**: H-6 (no rate limiting implementation)
**ADR**: [ADR-002](../../adr/ADR-002-rate-limiting-architecture.md)

**Approach**:
- `golang.org/x/time/rate.Limiter` (standard library)
- Two tiers: IP-based for auth endpoints, project_id-based for authenticated
- `sync.Map` storage with 10-minute eviction goroutine
- 10,000 entry cap, degrade open on overflow
- Configurable via `rate_limiting` block in `o3k.yaml`
- 12 unit tests + 1 integration test

**Validation Gate**: Rate limiter tests pass; POST /v3/auth/tokens returns 429 under rapid fire

### Step 6: Cinder Route Redirect

**Finding**: H-8 (duplicate route registration in Cinder)

**Approach**:
- Audit all Cinder routes to inventory duplicates
- Pick v3 paths as canonical (matches Terraform provider default)
- Redirect old paths via HTTP 301 to canonical paths
- Do NOT remove old paths (removal deferred to v0.8.0 after conformance proves they're unused)

**Validation Gate**: Both old and new Cinder paths return correct responses; conformance tests pass

### Step 7: Pagination Migration

**Finding**: Pagination inconsistency (10 handlers use `ParsePagination()`, 24 use inline parsing)

**Approach**:
- Audit all 34 list handlers to identify which actually paginate
- Skip handlers that don't paginate (no API behavior change)
- Migrate remaining handlers to `common.ParsePagination()`
- Conformance tests validate pagination behavior before and after

**Validation Gate**: All paginated list handlers use `ParsePagination()`; contract tests pass

## Dependencies

```
Step 1 (Lint) ──────────┐
Step 2 (Conformance) ───┤── Parallel
Step 3 (Map Tests) ─────┘
                         |
                         v
Step 4 (DB DI) ──────── Sequential (biggest item, benefits from lint gate)
                         |
                         v
Step 5 (Rate Limit) ────┐
Step 6 (Route Redirect) ┤── Parallel (different packages)
                         |
                         v
Step 7 (Pagination) ──── Sequential (uses DI pattern, conformance validates)
```

## Success Criteria

- [ ] golangci-lint passes in CI on all PRs
- [ ] 15+ Terraform conformance configs run in CI
- [ ] 28 failing tests mapped by service area
- [ ] Zero `database.DB` global references remaining
- [ ] Rate limiting active, configurable, tested (13 tests)
- [ ] Cinder routes consolidated via redirect
- [ ] All list handlers use consistent pagination
- [ ] All 258+ contract tests continue passing
- [ ] Zero API regressions from v0.6.0

## References

- [Full Repo Review Report](../../review/full-repo-review-report.md) — 42 findings, 32 fixed in v0.6.0
- [ADR-001: Database DI](../../adr/ADR-001-database-dependency-injection.md)
- [ADR-002: Rate Limiting](../../adr/ADR-002-rate-limiting-architecture.md)
- [v0.6.0 Implementation Plan](../../superpowers/plans/2026-04-07-full-repo-review-fixes.md)
