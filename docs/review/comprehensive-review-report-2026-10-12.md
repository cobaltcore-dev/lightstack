# Comprehensive Code Review — 2026-10-12

**Reviewed at**: HEAD `e909227` (`v0.6.1-63-ge909227`)
**Scope**: all of `internal/`, `pkg/`, `cmd/`, `charts/`, CI, release/deployment readiness
**Companion**: documentation audit applied the same day (README/CLAUDE/STATUS corrections)

---

## Executive Summary

Lightstack is in unusually good shape for a single-binary OpenStack reimplementation.
The most recent work (tenant scoping, Neutron fidelity, Nova gaps, quota enforcement)
shows disciplined engineering:

- The SQL placeholder helper (`common.SQLArgs`) genuinely eliminates the class of drift
  bugs it was built for — **no SQL-injection sites found** (the two `fmt.Sprintf`-into-SQL
  sites interpolate only identifiers/pre-rendered assignments).
- Quota enforcement holds an advisory lock across check *and* write in one transaction.
- Every reconciler (instance state, network, console tokens, node heartbeat, VXLAN
  coordinator) is a clean ticker/`ctx` loop wired into shutdown; sweeps use compare-and-set.
- Migrations 060–073 are idempotent, transactional, and have rollback-aware down files.
- CORS is allow-list only; all HTTP servers set read/write/idle timeouts; tenant isolation
  is consistent and carefully documented.

**No "will not start / data loss on the normal path" defects were found.** The problems
that remain are concentrated in four places: a chain of JWT/identity weaknesses, several
shutdown/goroutine races, release-process debt, and a few correctness loose ends.

## Release verdict for v0.7.0

| Must fix before release | Can ship as documented known-issues |
|---|---|
| **R1** version stamping broken | S3/S4, C1–C4, P1–P5, A1–A4 |
| **R2** tag/chart/README version drift | R3 (no release CI — but see below) |
| **S2** well-known default JWT secret | R4 (auto-migration caveat) |
| **S1** token revocation is a no-op | R5 (postgres pin drift) |

---

## 1. Security

### S1 — HIGH — Token revocation is a no-op; disabled users keep working until expiry
- `internal/keystone/handlers.go:311-316` — `RevokeToken` returns `204` and does nothing.
- `internal/keystone/auth.go:836-862` — `ValidateToken` (gates every service) checks only
  signature + expiry; never re-checks user existence/`enabled` or role liveness.
- Consequence: `DELETE /v3/auth/tokens` answers success while doing nothing; disabling a
  user or stripping the admin grant does not stop already-issued tokens (default TTL 24h).
- Inconsistency: `BuildTokenResponse` (auth.go:709-787) *does* fail closed on disabled
  users/projects — RGW clients get fail-closed semantics while the API middleware does not.
- **Fix:** add a `jti` claim + revocation list (table or Redis `revoked:<jti>`); check
  user `enabled` in `ValidateToken`; make `RevokeToken` honest.

### S2 — HIGH — Well-known default JWT secret ⇒ forged-admin-token takeover
- `config/lightstack.yaml:15` (`jwt_secret: "change-me-in-production"`),
  `deployments/docker-compose.yml:67` (same literal as compose default),
  `.env.example:9` (yet another distinct public literal).
- `internal/common/config.go:378-381` warns only for the exact string
  `"change-me-in-production"` — the `.env.example` value trips nothing.
- Combined with S1 (no DB cross-check), anyone who knows the public secret can mint a
  token with `roles:["admin"]` and get full admin over every API.
- **Fix:** refuse to start on empty/known-default secrets (the Helm chart already
  fail-fasts on empty `secrets.jwtSecret` — `charts/lightstack/templates/_helpers.tpl:70-74`;
  mirror that in the binary); generate a random secret in docker-compose.

### S3 — MEDIUM — Project-scoped `admin` role is honored as deployment-wide authority
- Roles are scoped to the token's project at issue (`auth.go:341-367`), yet `admin`
  widens `?all_tenants` (`internal/nova/server_filter.go:69`), cross-project instance
  access (`internal/nova/authz.go:51-61`), quotas (`internal/common/project.go:90`) and
  deployment-wide surfaces (`authz.go:119-131`).
- Mirrors upstream Nova's `admin_or_owner` permissiveness, but real Keystone separates
  system scope; here it does not.
- **Fix:** document the model as a deliberate tradeoff, or introduce a system scope.

### S4 — MEDIUM — Rate limiting configured and ADR-accepted, but entirely unimplemented
- `config/lightstack.yaml:166-172` ships a `rate_limiting` block;
  `docs/adr/ADR-002-rate-limiting-architecture.md` is **Accepted**.
- Zero code wires it (`internal/nova/handlers.go:3156`, `internal/cinder/volumes.go:2554`
  return `"rate": []gin.H{} // No rate limiting implemented`).
- Consequence: unthrottled brute-force on public `POST /v3/auth/tokens` (bcrypt per
  attempt) and dead config.
- **Fix:** implement ADR-002 (a minimal IP limiter on `/auth/tokens` is the highest-value
  slice) or delete the config + retract the ADR.

### S5 — LOW — Metadata service binds all interfaces, unauthenticated by design
- `cmd/lightstack/main.go:332-341`; identity is TCP peer IP; `X-Instance-ID` trusted from
  loopback only unless `trust_instance_header`.
- **Fix:** document the trust model; warn when binding publicly in stub mode.

### S6 — LOW — No minimum secret-length guard; no `jti` claim (ties into S1/S2).

### S7 — INFO — CORS allow-list only, no wildcard (lives in `internal/middleware/logging.go:313-316` — odd placement).

## 2. Concurrency & Lifecycle

### C1 — MEDIUM — WaitGroup Add-after-Wait race during shutdown
- `main.go:421` calls `nova.Shutdown()` (cancel + `wg.Wait()`) *before* the HTTP servers
  drain (`main.go:448`); in-flight requests can still `svc.wg.Add(1)`
  (`nova/handlers.go:1092,3429`, `advanced_actions.go:89,162,254`, `cinder/volumes.go:1621`).
- **Fix:** drain HTTP servers first, then cancel + wait service workers.

### C2 — MEDIUM — Unbounded `wg.Wait()` can hang the drain
- `nova/handlers.go:94-97` waits without a timeout, blocking the documented
  `LIGHTSTACK_DRAIN_SECONDS` guarantee.
- **Fix:** bound the wait with `select` + `time.After`.

### C3 — LOW — `WithTx` leaks the transaction on a panic in `fn`
- `internal/database/tx.go:13-27` has no `defer tx.Rollback`.
- **Fix:** committed-flag + `defer tx.Rollback(ctx)`.

### C4 — LOW — Cinder async state transitions have no reconciler or state guard
- `cinder/volumes.go:1621-1639` flips snapshot → `available` in a fire-and-forget
  goroutine; a DB error leaves it stuck in `creating` forever. Same stub-mode pattern in
  Nova.
- **Fix:** CAS transitions (like `applyVerdict`, `state_reconciler.go:209-218`) or a sweep.

### C5 — INFO — `context.Background()` in `BuildServiceCatalog` (`keystone/auth.go:885`).

### C6 — POSITIVE — Background lifecycle otherwise clean (all reconcilers ctx-aware, stopped on shutdown, intervals clamped).

## 3. Correctness

### P1 — MEDIUM — `UpdateQuotaSet` not atomic, accepts arbitrary resource keys
- `internal/cinder/quotas.go:204-238` upserts each resource separately (partial
  application on failure); caller-controlled resource names persist.
- **Fix:** wrap in `database.WithTx`; validate resource keys.

### P2 — LOW — Advisory-lock key hashing can collide
- `quotas.go:418` — `pg_advisory_xact_lock(hashtext($1))` is 32-bit.
- **Fix:** two-int4 advisory lock on a 64-bit hash.

### P3 — LOW — Unbounded `limit` in listings (`glance/images.go:282-288` accepts `limit=1e9`).
- **Fix:** clamp to an absolute max like upstream.

### P4 — POSITIVE — `(created_at, id)` total-order marker pagination with visibility-scoped marker resolution is done right (`images.go:328-358`).

### P5 — MEDIUM (release-note) — Placement is a pure stub (`internal/placement/placement.go:82-101`)
- Always-empty `resource_providers`/`resource_classes`/`traits` while advertising max
  microversion `1.40`. Fine for Horizon; **must be documented as not-implemented**.

### P6 — Migration safety (060–073): all additive/idempotent; data-changing ones have justified downs.
- **Rollback caveat for v0.7.0 release notes:** a v0.6.x binary against a DB at 065+
  would write the dropped console columns — the documented rollback must be
  *migrate-down first, then roll back the binary*. Worth an explicit line in
  `docs/UPGRADING.md`.

## 4. Architecture / Code Health

- **A1 — MEDIUM** — `NetworkNamespaceManager` is dead except teardown
  (`pkg/networking/netns.go:22-24`, `neutron/network.go:28,48`, `teardown.go:87`).
  Intentional (legacy reclaim) but a divergence risk; document as legacy-only.
- **A2 — INFO** — `internal/nova` is ~20k lines across ~10.4k code + ~9.9k test (near 1:1 —
  good); the maintenance lever is splitting `handlers.go`.
- **A3 — INFO** — Stub mode simulates async transitions with hard timers; stub tests can
  pass while real mode fails. Compensated by real-mode bash tests; keep documenting.
- **A4 — INFO** — Coverage shape: unit (`-race`, DB-backed) + contract (hard 85% gate) +
  bash integration + helm-unittest. No e2e job (explicit TODO `ci.yml:318`); `placement`,
  `compute`, `database` have no unit tests.

## 5. Release / Deployment Readiness

### R1 — CRITICAL — Version stamping is broken
- `Makefile:7` passes `-ldflags "-X main.Version=$(VERSION)"` but **no `var Version`
  exists** in `cmd/lightstack/main.go`. Go silently ignores `-X` for an undefined symbol —
  every shipped binary is unidentifiable (no `--version`, no startup log, no
  `debug.ReadBuildInfo`).
- **Fix:** declare `var Version = "dev"`, add `lightstack --version`, log at startup,
  add a CI round-trip check.

### R2 — HIGH — Tag/chart/README version drift
- `charts/lightstack/Chart.yaml`: `version: 0.3.0`, `appVersion: "0.7.0"` — no v0.7.0
  release exists; tags stop at `v0.6.1`; `v1.0.0` sits 451 commits behind HEAD as a
  misleading "latest".
- **Fix:** cut a real `v0.7.0`, delete/re-aim the stale `v1.0.0`, sync README/STATUS,
  bump chart `version` with appVersion aligned.

### R3 — MEDIUM — No release CI/CD; no published image
- Only `ci.yml` + `security.yml`. Nothing builds/pushes an image or a release;
  `values.yaml` `image.repository: ""` is fail-fast by design (good), but nothing
  publishes an image to satisfy it.
- **Fix:** tag-triggered release workflow (goreleaser or buildx multi-arch → ghcr,
  checksums/SBOM, GitHub Release, chart publish as OCI).

### R4 — MEDIUM — Auto-migration on startup with `log.Fatalf` (`main.go:104-107`)
- Multi-replica startup can race; a dirty migration = CrashLoopBackOff. Readiness gates
  until migrations finish, stretching probe windows.
- **Fix (later):** init-container/job migration in the chart; wire the existing
  `MigrateDown`/force functions (`internal/database/migrate.go:81-209`) into a subcommand.

### R5 — LOW — Postgres pin drift: CI `18.3-alpine` vs compose `18.4-alpine` vs `18-alpine`. Unify on 18.4.

### R6 — POSITIVE — Chart hygiene is a strength (schema validation, render-time `fail()` gates, drainSeconds < terminationGracePeriod).

## Top-10 Action List

| # | Severity | Item |
|---|----------|------|
| 1 | Critical | R1: declare `main.Version`, expose `--version`/startup log |
| 2 | High | S2: refuse-to-start on empty/known-default JWT secret |
| 3 | High | S1: real revocation (jti + blacklist) + user-enabled check |
| 4 | High | R2: cut v0.7.0, remove stale v1.0.0, align Chart.yaml |
| 5 | Medium | R3: release CI (image publish, SBOM, chart release) |
| 6 | Medium | C1+C2: drain HTTP before service workers; bound `wg.Wait()` |
| 7 | Medium | S4: auth-endpoint rate limiter or remove dead config |
| 8 | Medium | P1: atomic + validated `UpdateQuotaSet` |
| 9 | Medium | S3: document the admin-scope model |
| 10 | Low | P3 (clamp limits), C3 (WithTx rollback), C4 (cinder state guard), R5 (pg pin), A1 (netns doc) |
