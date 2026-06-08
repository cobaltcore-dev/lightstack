# Phase 0 Research: SPEC-007 Helm Prerequisites

The spec is decision-complete (11 clarifications across 4 sessions), so there are **no open `NEEDS CLARIFICATION` items**. This document records the *how* — implementation decisions and their rationale, grounded in the current codebase.

## R1 — Bootstrap placement

**Decision**: Add `internal/keystone/bootstrap.go` exposing `BootstrapAdmin(ctx, cfg common.KeystoneConfig) error`, called from `cmd/o3k/main.go` **immediately after `database.MigrateUp`** (`main.go:~86`) and before service wiring.

**Rationale**: At that point the pool (`database.DB`) is connected and the seed migration has run, so the admin row exists. Keystone already uses the `database.DB` global (e.g. `BuildServiceCatalog`), so the bootstrap can too — no new DI plumbing. Running before servers start guarantees the credential is in place before the API is reachable.

**Alternatives considered**: Inside `NewAuthService`/`NewService` constructors — rejected (constructors shouldn't perform DB writes / side effects, and ordering vs. migrations is less explicit). A dedicated migration — rejected (a migration can't read runtime config like `admin_password`).

## R2 — First-boot-seed detection (WS-2, FR-002)

**Decision**: On each start, resolve the admin row and apply this precedence (detection via `bcrypt.CompareHashAndPassword`):
1. **stored hash already verifies the configured password** → no-op (steady state; also cleanly handles the dev case `admin_password: secret`, so no needless re-hash on every start).
2. **stored hash verifies the default `secret`** (and ≠ configured) → write the configured password (first-boot seed).
3. **otherwise** (operator-set or API-rotated to some other value) → leave untouched (FR-002 no-clobber).

Account absent → create it (custom-name path, R3).

**Rationale**: Satisfies the first-boot-seed model (Clarification: option B) without adding a schema column or sentinel. bcrypt-comparing against the literal default is exact and self-contained. The seed migration (`002_seed_data.up.sql`) stores `admin` with `bcrypt('secret')` and UUID `…0001`, so a fresh DB reliably hits the "still default" branch.

**Alternatives considered**: A `bootstrapped` boolean / metadata table — rejected (extra schema, more state, not needed). Always overwrite on start (option A) — rejected by clarification (clobbers runtime rotation on every pod restart). Reconcile-on-config-change with a stored marker (option C) — rejected by clarification (extra state).

## R3 — Custom `admin_user`: neutralize + role grant (FR-004a, FR-004b)

**Decision**: When `admin_user` is set and ≠ `admin`:
1. `INSERT` the named user with `bcrypt(configured)`, a fresh UUID, **and `domain_id = 00000000-…-0100` (the seeded `Default` domain)** `ON CONFLICT (name) DO NOTHING` (first-boot seed — don't overwrite if it already exists). The `domain_id` is required: login filters users by domain (`auth.go:168`, default domain `Default`), so a custom admin without it would be unfindable.
2. Grant it the standard admin role: `INSERT INTO role_assignments (user_id, project_id=…0002 default, role_id=…0003 admin) ON CONFLICT DO NOTHING` (mirrors the seed's grant for `admin`).
3. Disable the seeded default: `UPDATE users SET enabled=false WHERE name='admin'`.

**Rationale**: Reuses the seed's fixed UUIDs for the `default` project (`…0002`) and `admin` role (`…0003`), so the custom admin is functionally identical to the default one. `enabled=false` is the native Keystone disabled-user mechanism (column exists in `001_initial_schema`), reversible, preserves the row + audit. All three statements are idempotent.

**Alternatives considered**: Delete the seeded admin — rejected (loses audit, cascades role assignments). Scramble its password — rejected (less clear than `enabled=false`; account would still appear active).

## R4 — Concurrency safety (FR-008a)

**Decision**: No locks / leader election. Use atomic, idempotent statements:
- Create paths use `INSERT … ON CONFLICT (name) DO NOTHING` (same pattern as the seed).
- The default-name password write uses an optimistic guard: `UPDATE users SET password_hash=$new WHERE id='…0001' AND password_hash=$observed` (the hash read during detection). If a peer node won the race, 0 rows update — and the peer already wrote the configured password, so the end state is correct either way.

**Rationale**: Matches the constitution (synchronous, no queues, simplicity) and the existing seed's `ON CONFLICT` convention. Two nodes converging on the same configured plaintext is safe regardless of bcrypt salt differences. Verified conceptually against the unique `users.name` constraint.

## R5 — Catalog scheme rewrite (WS-3, FR-009–014)

**Decision**: Add a shared helper in `internal/common` (so both `keystone` and `common` use it — `keystone` already imports `common`):
- `EndpointScheme()` — reads `O3K_ENDPOINT_SCHEME`, trims + lowercases, returns `http` when unset/empty, accepts `http`/`https`, and on any other value logs a one-time warning (via `sync.Once`) and returns `http` (FR-011, FR-014).
- `ForceScheme(url, scheme)` — rewrites a leading `http://`/`https://` to `<scheme>://`.

Call `ForceScheme` at every URL choke-point so **every** advertised URL honors the scheme:
- `substituteURLTemplates` (`auth.go:642`, DB catalog) after the existing host rewrite,
- `buildHardcodedCatalog` (fallback catalog), and
- `common.BaseURL` (`baseurl.go:13`, resource self-links / pagination) — replacing its hardcoded `http://` (FR-013a).

This covers both the Keystone catalog and the self-links other services emit (FR-010, FR-013a).

**Rationale**: A single choke-point per URL keeps both catalog paths consistent. Validating once avoids per-request/per-URL log spam. The scheme is fixed at process start (env var), so the existing 24h per-project catalog cache stays correct — no cache-key change needed. Stored `endpoints` rows are untouched (FR-013).

**Ordering / host composition**: The DB endpoints are stored as `http://o3k:<port>/...` (seeded in `040`, then rewritten to `o3k:` by migration `052_fix_endpoint_hostnames`). `substituteURLTemplates` already maps `http://o3k:` → `http://<O3K_ENDPOINT_HOST>:`. `forceScheme` runs **after** that host rewrite, so `http://o3k:9292/v2` → `http://<host>:9292/v2` → `https://<host>:9292/v2`. Host and scheme thus compose correctly (FR-012), satisfying User Story 2 (`https://<host>:<port>`).

**Alternatives considered**: Re-seed `https://` endpoints in migration 040 (clarified option C in round 1) — rejected (bakes scheme into DB; not per-deployment; touches catalog data). A config-struct field instead of env — rejected (the host rewrite it must compose with is env-based; mirroring it is simpler).

## R6 — bcrypt reuse

**Decision**: Reuse `HashPassword` (`auth.go:545`, `bcrypt.GenerateFromPassword(pw, bcrypt.DefaultCost)`) for the configured password. Verification path (`auth.go:184`, `bcrypt.CompareHashAndPassword`) is unchanged.

**Rationale**: Same hashing scheme as all user credentials (FR-005, constitution security requirement). No new crypto code.

## R7 — No database migration

**Decision**: Add no migration. The seed (`002`) is retained; WS-2 mutates rows at runtime via the bootstrap; WS-3 is presentation-time only.

**Rationale**: FR-013 forbids modifying stored catalog rows; the seed default is intentionally retained for the no-password dev case (Clarification round 1). Avoids reversible-migration overhead for a change that needs none.

## R8 — TLS exposure model (chart constraint, decided 2026-06-08)

**Decision**: WS-3 advertises `https://<host>:<service-port>`. The SPEC-008 chart MUST terminate TLS **per service port** (port-based — `docs/KUBERNETES_DEPLOYMENT.md` Option B: a LoadBalancer per service, or one L4 LB with TLS on each port). Single-`:443` path-based or per-subdomain ingress is out of scope.

**Rationale**: O3K serves each service on its own port (`main.go`: `:35357`/`:8774`/`:9696`/`:8776`/`:9292`/…), and the catalog lists one URL per service *including its port* (`052_fix_endpoint_hostnames`). A scheme-only rewrite keeps that port, so it composes cleanly with a per-port TLS LB; one host certificate (wildcard/SAN) covers all ports. This matches O3K's existing design and the Docker Compose port layout.

**Alternatives considered**: Path-based single-`:443` ingress (`/identity`→keystone, `/compute`→nova, …, doc Option A) — rejected: the catalog (and `BaseURL`) would have to advertise per-service path prefixes and drop the port, a change well beyond scheme-only (FR-013), re-opening WS-3 scope. Per-service subdomains on `:443` — rejected: needs per-service DNS + SAN cert + per-service host rewrite.

## Observability & logging (FR-006, FR-006a, FR-006b)

- One structured log line when the admin is created or seeded (naming the user, **never** the password).
- A prominent warning when no password is configured (boots with default — install gate is the chart's, not the binary's).
- A one-time warning on an invalid `O3K_ENDPOINT_SCHEME`.
- Audited via SC-008: assert the log event exists and the password value never appears in output.
