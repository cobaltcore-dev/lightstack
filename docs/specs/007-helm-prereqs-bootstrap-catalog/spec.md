# Feature Specification: SPEC-007 Helm Prerequisites — Admin-Password Bootstrap + Configurable Catalog Scheme

**Feature Branch**: `007-helm-prereqs-bootstrap-catalog`  
**Created**: 2026-06-04  
**Status**: Draft  
**Input**: User description: "Helm chart prerequisites (SPEC-007): two small config/bootstrap-level changes to O3K that must land before the Helm chart (SPEC-008) can be authored securely over TLS — (WS-2) admin-password bootstrap wiring and (WS-3) configurable service-catalog scheme (http/https)."

## Clarifications

### Session 2026-06-04

- Q: When no admin password is configured at startup, what should the product (the O3K binary) do? → A: Boot with the well-known default credential but emit a prominent warning (option C). The product does not refuse to start; the "fail the install when no password is supplied" gate is owned by the SPEC-008 Helm chart (FR-007a), not the binary.
- Q: When a non-empty admin password IS configured, does the binary impose strength validation (minimum length / complexity / denylist)? → A: No — permissive (option A). The binary accepts any non-empty (trimmed) value as supplied; password strength is enforced by the SPEC-008 chart, not the product. (A binary denylist would also reject the explicit dev/CI default of `secret`.)
- Q: How is the advertised catalog scheme made configurable? → A: A new `O3K_ENDPOINT_SCHEME` environment variable (option A), read alongside the existing `O3K_ENDPOINT_HOST` and applied at catalog-presentation time, defaulting to `http`. The stored catalog rows and migration 040 are left untouched.
- Q: What is logged when bootstrap creates or updates the admin password? → A: Minimal structured logging (option A) — one log line on create/update identifying the user but never the password value; plus the existing FR-006 no-password warning.
- Q: When a custom `admin_user` (≠ `admin`) is configured with a password, what happens to the seeded default `admin` account? → A: Neutralize it (option B) — bootstrap disables/locks the seeded `admin` so `admin/secret` can no longer authenticate. (When `admin_user` is the default `admin`, bootstrap simply updates that same row in place; the gap only exists on name-change.)
- Q: Does bootstrap reconcile the admin password to config on every start, or only seed it once? → A: First-boot seed only (option B). The configured `admin_password` is applied only at initial provisioning — when the admin account is absent or still carries the well-known default `secret`. Once the password is non-default, bootstrap never overwrites it; rotation after provisioning is done via the OpenStack Identity API, not by changing config. (Matches Keycloak/Grafana-style bootstrap. Chosen over upstream Keystone's reconcile-on-redeploy because O3K bootstraps on every process start, where a reconcile would clobber a runtime-rotated password on every pod restart.)
- Q: What happens when `O3K_ENDPOINT_SCHEME` is set to an unsupported value? → A: Warn and default to `http` (option A) — the binary logs a prominent warning, ignores the invalid value, and advertises `http`; it does not refuse to start. `http`/`https` are parsed case-insensitively and whitespace-trimmed.
- Q: How is multi-node concurrent first-boot provisioning made safe? → A: Atomic/idempotent DB operations (option A) — a conditional `UPDATE` guarded on the default credential plus `INSERT … ON CONFLICT`, with no external coordination (no locks, leader election, or designated bootstrapper). Concurrent nodes converge to one admin account.
- Q: Does a password change revoke existing tokens? → A: No (option A). O3K tokens are stateless (HMAC-signed, not server-stored, not password-bound); existing tokens remain valid until their TTL expiry (default 24h, configurable) after any password change. Immediate revocation is an explicit non-goal here; operators needing a tighter bound shorten the token TTL.
- Q: How is the seeded `admin` neutralized when a custom `admin_user` is configured (FR-004a)? → A: Set `enabled = false` (option A) on the seeded `admin` row — the native Keystone disabled-user mechanism (column already exists); reversible; the row and its role assignments are preserved (not deleted or scrambled).
- Q: What role/project provisioning does a custom `admin_user` get, and what does FR-007 constrain? → A: Mirror the seed's grants (option A) — bootstrap grants the custom admin the standard admin role on the seeded `default` project (FR-004b). FR-007 is scoped to preserving project isolation and not altering *other* identities' privileges; it does not forbid granting the configured admin its standard role or disabling the superseded seed `admin`.

### Session 2026-06-08

- Q: Which TLS exposure model must the chart use so WS-3's `https://<host>:<service-port>` URLs resolve? → A: Port-based per-service TLS (option 1) — the chart (SPEC-008) terminates TLS per service port, matching O3K's multi-port design and `docs/KUBERNETES_DEPLOYMENT.md` Option B. A single-`:443` path-based or per-subdomain ingress is out of scope (it would require advertising per-service path prefixes or hostnames, beyond scheme-only rewriting).
- Q: Does `O3K_ENDPOINT_SCHEME` apply only to the Keystone catalog, or also to resource self-links / pagination? → A: Both — it also governs the self-links and pagination URLs built by `common.BaseURL` (Nova/Neutron/Cinder/Glance responses), so a TLS-fronted deployment never emits `http://` self-links (FR-013a).

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Operator sets a non-default admin password at deploy time (Priority: P1)

An operator deploys O3K (single-node or multi-node) and supplies an admin password through configuration. On startup, the platform provisions the admin account with exactly that password. No deployment ever comes up reachable with the well-known `admin/secret` default.

**Why this priority**: This is a security-blocking defect. Today every fresh deployment is reachable with a publicly-known credential, and the configuration knobs that should fix it are dead code. Until this works, the Helm chart cannot require a non-default admin password, and even a single-node install is unsafe to expose. This is the gating prerequisite for SPEC-008.

**Independent Test**: Deploy O3K with a configured admin password, then attempt to authenticate. Authentication succeeds with the supplied password and fails with `secret`. Fully testable without WS-3.

**Acceptance Scenarios**:

1. **Given** a fresh deployment with an admin password configured, **When** the platform starts, **Then** the admin account is usable with the configured password and authentication with `secret` fails.
2. **Given** an already-provisioned admin account, **When** the operator changes the configured admin password and restarts, **Then** the password is unchanged — the configured value is a first-boot seed and is not re-applied; rotation is performed via the OpenStack Identity API, not by changing configuration.
3. **Given** a deployment that has already applied the configured admin password, **When** the platform restarts repeatedly with the same configuration, **Then** the admin account remains usable and no error or duplicate account results (idempotent).
4. **Given** a configured admin user name other than `admin`, **When** the platform starts, **Then** that user exists with the configured password and the roles/project scoping an admin requires.
5. **Given** a non-default admin password is in effect, **When** any project-scoped resource is accessed, **Then** project isolation is unchanged (the admin's scoping and roles are not broadened or narrowed by this feature).

---

### User Story 2 - Operator behind TLS gets working catalog URLs (Priority: P2)

An operator fronts O3K with TLS terminated at an ingress/load balancer and configures the advertised endpoint scheme as `https`. Clients that authenticate receive a service catalog whose endpoint URLs use `https://<host>:<port>`, so OpenStack CLI, Horizon, and Terraform follow the catalog to the correct, working URLs instead of broken `http://` ones.

**Why this priority**: Required only for the chart's optional TLS path. Without it, a TLS-fronted deployment hands clients `http://` catalog URLs that fail. It is P2 because deployments without TLS are unaffected and the default behavior is unchanged.

**Independent Test**: Configure the advertised scheme as `https`, authenticate, and inspect the returned service catalog — all endpoint URLs use `https://`. With no scheme configured, the same catalog uses `http://`. Testable independently of the admin-password work.

**Acceptance Scenarios**:

1. **Given** the advertised catalog scheme is configured as `https`, **When** a client authenticates and reads the service catalog, **Then** every endpoint URL uses the `https://` scheme with the correct host and port.
2. **Given** no advertised catalog scheme is configured, **When** a client reads the service catalog, **Then** every endpoint URL uses `http://` (unchanged default behavior).
3. **Given** a configured advertised host (existing host-rewrite behavior), **When** the advertised scheme is also configured as `https`, **Then** the catalog URLs reflect both the configured host and the `https` scheme together.
4. **Given** either scheme setting, **When** a client reads the catalog, **Then** the catalog structure (service types, names, endpoint interfaces/regions, fields) is identical except for the URL scheme — no API shape change.
5. **Given** the advertised scheme is `https`, **When** a client lists a paginated resource (e.g. servers, networks) or reads a resource's self-link, **Then** those URLs also use `https://` — not only the Keystone catalog.

---

### Edge Cases

- **No admin password supplied**: The platform boots with the default credential but emits a prominent warning — it does not fail silently and does not refuse to start. The SPEC-008 chart is responsible for blocking installation when no password is supplied.
- **Admin account already exists from the seed migration**: The seeded `admin` carries the well-known default; on first boot, bootstrap treats that default as "not yet provisioned" and replaces it with the configured password (no duplicate, no conflict error). On later starts the password is already non-default, so bootstrap leaves it untouched.
- **Custom `admin_user` configured**: Bootstrap provisions the named account with the configured password AND disables the seeded default `admin` (sets `enabled = false`) so `admin/secret` cannot authenticate; only the configured admin remains usable.
- **Configured password changed after provisioning**: Once the admin account is provisioned with a non-default password, changing the configured value and restarting has no effect (the config is a first-boot seed). Rotation is done via the OpenStack Identity API.
- **Empty or whitespace-only admin password**: Treated the same as "not supplied" (rejected), not accepted as a valid password.
- **Scheme configured to an unsupported value** (anything other than `http`/`https`, case-insensitive): The platform logs a prominent warning and falls back to the default `http`; it does not advertise a malformed scheme and does not refuse to start.
- **Mixed host/scheme rewrite**: Configuring the scheme must not break or bypass the existing host-rewrite behavior, and vice versa.
- **Stub vs. real mode**: Both workstreams behave identically across stub and real modes.

## Requirements *(mandatory)*

### Functional Requirements

#### WS-2 — Admin-password bootstrap

- **FR-001**: The platform MUST, at startup, provision the administrative account using the operator-supplied admin password when one is configured, instead of relying solely on the seed migration's fixed default credential.
- **FR-002**: When an admin password is configured, the platform MUST apply it only at initial provisioning — i.e., when the admin account does not yet exist, or when it still carries the well-known default credential (`secret`). Once the admin account has a non-default password (operator-applied or subsequently changed via the Identity API), bootstrap MUST NOT overwrite it. The configured password is a first-boot seed, not a per-start reconcile.
- **FR-003**: Bootstrap MUST be idempotent and safe to run on every start: once the admin account is provisioned, repeated starts MUST leave it usable and unchanged, MUST NOT overwrite its password, and MUST NOT create duplicates or error.
- **FR-004**: The platform MUST support configuring the admin account name (defaulting to `admin`) alongside the admin password, and apply the configured password to that named account.
- **FR-004a**: When `admin_user` is configured to a name other than the default `admin`, the platform MUST disable the seeded default `admin` account at bootstrap by setting its `enabled` flag to false, so that the well-known `admin/secret` credential can no longer authenticate. The row and its role assignments MUST be preserved (not deleted). (When `admin_user` is `admin`, the seeded row is updated in place per FR-002, so no separate neutralization is needed.)
- **FR-004b**: When provisioning a custom `admin_user` (a name other than the default `admin`), the platform MUST grant it the same role assignments the seed grants the default `admin` — the standard admin role on the seeded `default` project — so that the custom admin is a fully functional administrator.
- **FR-005**: The stored admin credential MUST be protected using the same one-way hashing scheme already used for user credentials (no plaintext or reversible storage).
- **FR-006**: When no admin password is configured (including empty/whitespace), the platform MUST emit a prominent startup warning that it is booting with the well-known default credential, and then continue to boot — it MUST NOT fail silently. The product itself does NOT refuse to start; blocking installation when no password is supplied is owned by the SPEC-008 chart (FR-007a), not the binary.
- **FR-006a**: When a non-empty admin password IS configured, the platform MUST accept it as supplied (after trimming surrounding whitespace) and MUST NOT impose minimum-length, complexity, or denylist validation in the binary — password strength is enforced by the SPEC-008 chart, not the product.
- **FR-006b**: The platform MUST emit a structured log entry when it provisions the admin account (creates it or seeds its initial password) at bootstrap, identifying the affected user. The platform MUST NEVER write the admin password value (plaintext or hash) to logs.
- **FR-007**: This feature MUST preserve project isolation and MUST NOT alter the roles, project membership, or scoping of any identity other than the admin account(s) it provisions. Establishing the configured admin (creating it and granting it the standard admin role per FR-004b) and disabling the superseded seed `admin` (FR-004a) are explicitly permitted; no other identity's privileges change.
- **FR-008**: The behavior MUST be identical across single-node and multi-node, and across stub and real modes.
- **FR-008a**: Admin provisioning MUST be safe under concurrent execution by multiple nodes against the same database, using atomic, idempotent operations (a conditional `UPDATE` guarded on the default credential, plus `INSERT … ON CONFLICT`) rather than external coordination (no locks, leader election, or designated bootstrapper). Concurrent first-boot provisioning MUST converge to a single admin account with the configured password and MUST NOT error or create duplicates.

#### WS-3 — Configurable service-catalog scheme

- **FR-009**: The platform MUST allow the advertised service-catalog endpoint scheme to be configured as either `http` or `https` via a dedicated environment variable (`O3K_ENDPOINT_SCHEME`), read alongside the existing `O3K_ENDPOINT_HOST` and applied at catalog-presentation time.
- **FR-010**: When the scheme is configured as `https`, every advertised catalog endpoint URL MUST use `https://` with the correct host and port.
- **FR-011**: When the scheme is not configured (`O3K_ENDPOINT_SCHEME` unset or empty), advertised catalog endpoint URLs MUST use `http://` (the existing default is preserved).
- **FR-012**: The scheme configuration MUST compose correctly with the existing advertised-host rewrite, so that host and scheme are both reflected in the final URLs.
- **FR-013**: The catalog response structure MUST remain unchanged except for the URL scheme — service types, names, endpoint interfaces, regions, IDs, and all other fields MUST be identical (no OpenStack API shape change, no microversion bump). The scheme MUST be applied at catalog-presentation time; the stored catalog rows (migration 040) MUST NOT be modified.
- **FR-013a**: The configured scheme MUST also apply to advertised resource self-links and pagination URLs in service responses (not only the Keystone catalog), so a TLS-fronted deployment never emits `http://` links. Default remains `http`.
- **FR-014**: The scheme value MUST be parsed case-insensitively and whitespace-trimmed. An unsupported value MUST NOT produce malformed catalog URLs and MUST NOT cause startup to fail; instead the platform MUST log a prominent warning and fall back to the default `http`.

#### Cross-cutting

- **FR-015**: Both workstreams MUST maintain 100% OpenStack API/CLI/Terraform compatibility and MUST NOT introduce queues or async state machines (operations remain synchronous).
- **FR-016**: All changes MUST pass the SPEC-000 contract tests.

### Key Entities *(include if feature involves data)*

- **Admin account**: The bootstrap administrative identity. Key attributes: account name (configurable, default `admin`), one-way-hashed password (derived from the configured value), and its existing roles/project scoping (unchanged by this feature).
- **Admin-password configuration**: Operator-supplied setting (password and optional user name) consumed at bootstrap and applied only at initial provisioning (first-boot seed). Presence/absence drives the no-password warning and the chart's install gate.
- **Catalog-scheme configuration**: Operator-supplied environment variable (`O3K_ENDPOINT_SCHEME`) selecting `http` (default) or `https` for advertised endpoint URLs; read alongside and composing with the existing advertised-host setting (`O3K_ENDPOINT_HOST`).
- **Service catalog**: The set of advertised service endpoint URLs returned at authentication; affected only in the URL scheme by WS-3.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: 0% of deployments come up reachable with the `admin/secret` default credential when an admin password is configured.
- **SC-002**: 100% of deployments configured with an admin password allow administrative authentication with that password and reject `secret` on first boot — including when a custom `admin_user` is configured (the seeded `admin` account is neutralized).
- **SC-003**: When no admin password is configured, the product emits an observable startup warning 100% of the time, and the SPEC-008 chart blocks installation 100% of the time (the install-blocking gate lives in the chart, not the binary).
- **SC-004**: Repeated restarts with unchanged configuration succeed 100% of the time with no duplicate accounts and no startup errors (idempotency).
- **SC-005**: When the advertised scheme is configured as `https`, 100% of catalog endpoint URLs use `https://`; with no configuration (or an invalid value), 100% use `http://`.
- **SC-006**: The service catalog is structurally identical (every field except URL scheme) between the `http` and `https` configurations — verified by comparison.
- **SC-007**: The full SPEC-000 contract test suite passes with both workstreams enabled, and existing OpenStack CLI/Terraform workflows succeed unchanged.
- **SC-008**: Bootstrap emits an observable create/update log event for the admin account on every applicable start, and the admin password value never appears in any log output (verified 100%).
- **SC-009**: Concurrent first-boot provisioning by multiple nodes against a fresh database results in exactly one admin account with the configured password, with no duplicate-key errors or startup failures (verified under simulated concurrent start).

## Assumptions

- **Fail-fast gate ownership**: Resolved (see Clarifications 2026-06-04). The product boots with the default credential and emits a prominent warning when no admin password is configured; it does not refuse to start. The "fail the install when no password is supplied" enforcement lives entirely in the SPEC-008 chart (FR-007a).
- **Scope of admin update**: Bootstrap sets the admin password only at initial provisioning (first-boot seed) and ensures the named admin account exists with its normal admin roles. It does not reconcile or rotate the password on later starts, and does not manage arbitrary additional users; broader user provisioning is out of scope.
- **Configuration surface**: The admin password/user are configured via `keystone.admin_password` / `keystone.admin_user` (config file). The catalog scheme is configured via the `O3K_ENDPOINT_SCHEME` environment variable (resolved 2026-06-04), mirroring the existing `O3K_ENDPOINT_HOST` rewrite.
- **Default preservation**: With neither scheme knob set, catalog behavior and API shapes are unchanged from today, so non-TLS and existing deployments are unaffected.
- **Seed migration retained**: The seed migration remains for fresh-DB bootstrapping. When a password is configured for the default `admin` name, bootstrap overrides the seeded credential in place; when a custom `admin_user` is configured, bootstrap neutralizes the seeded `admin` account (FR-004a, resolved 2026-06-04). The seeded `admin/secret` remains live only in the no-password-configured dev case (Round-1 clarification).
- **TLS exposure model**: WS-3 advertises `https://<host>:<service-port>` (same host, one port per service). The SPEC-008 chart MUST terminate TLS **per service port** (port-based exposure — `docs/KUBERNETES_DEPLOYMENT.md` Option B), so these URLs resolve. This matches O3K's multi-port design (each service on its own port); a single host certificate (wildcard/SAN) covers all ports. Single-`:443` path/subdomain routing is out of scope (resolved 2026-06-08).

## Dependencies

- **Depends on**: SPEC-000 (OpenStack API compliance — must pass its contract tests).
- **Blocks**: SPEC-008 (Helm charts), both single-node and multi-node — specifically chart requirements FR-007/FR-007a (admin password) and FR-008b (TLS catalog scheme). SPEC-008 MUST expose O3K with **per-service-port TLS termination** (port-based) so the advertised `https://<host>:<port>` catalog and self-link URLs resolve (see Assumptions → TLS exposure model).
- **Reuses**: existing Keystone bootstrap path and the existing advertised-host (`O3K_ENDPOINT_HOST`) rewrite.

## Out of Scope

- Authoring the Helm chart itself (Secrets, `values.yaml`, install-time gating) — that is SPEC-008.
- TLS termination, certificate management, and ingress configuration — handled by the chart/infrastructure; this spec only makes the advertised scheme configurable.
- Single-`:443` path-based or per-subdomain ingress. WS-3 advertises `https://<host>:<service-port>` (scheme + host only); routing all services through one `:443` host (via path prefixes like `/identity`, `/compute` or per-service subdomains) would require advertising per-service paths/hostnames, which is beyond scheme rewriting. The chart terminates TLS **per service port** instead (see Assumptions → TLS exposure model).
- Multi-node placement, metadata, north-south networking, and startup reconciliation — tracked separately as SPEC-007 post-conditions.
- General user/password management beyond the bootstrap admin account.
- Ongoing admin-password rotation via configuration. Once provisioned, the admin password is rotated through the OpenStack Identity API (e.g., `openstack user password set`), not by changing config — the configured `admin_password` is a first-boot seed (see Clarifications 2026-06-04).
- Token/session revocation on password change. O3K tokens are stateless (HMAC-signed, not server-stored, not password-bound); after any password change, existing tokens remain valid until their TTL expiry (default 24h, configurable). Immediate revocation would require stateful tokens and is a separate feature.
