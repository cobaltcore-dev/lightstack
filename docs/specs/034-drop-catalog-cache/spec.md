# Feature Specification: Stop Caching the Service Catalog

**Feature Branch**: `feature/581`
**Created**: 2026-11-25
**Status**: Draft
**Input**: GitHub issue cobaltcore-dev/cloud-storage#581 — "lightstack — Keystone service-catalog
cache is never invalidated and caches post-substitution host URLs" (sub-issue of #536), body and both
comments.

## Background

Every token Lightstack issues or validates carries a service catalog: the list of endpoints a client
uses to find Nova, Neutron, Cinder, Glance and the rest. With the optional shared cache turned on,
that catalog is stored after its addresses have been resolved, under a key that does not cover every
input that shaped it, and nothing ever removes it. It is served for up to 24 hours after the
endpoints, the advertised scheme, or the deployment's own catalog rows change.

The cache is off in every shipped configuration, so the defect is latent. It is also the reason the
cache-on path has never run in a test or a host pass.

**Decision (recorded on the ticket, 2026-11-17): the catalog is not cached at all.** It is read from
the database on every token issue and validation. The saving the cache offered is one small query
over about twenty rows, on paths that already run several; a cache hit costs about as much as the
query on PostgreSQL and more than it on SQLite. Fixing the cache instead would need invalidation that
every future catalog change must remember, would still go stale after direct database edits, and
would still race in a rolling update where old and new nodes share one external cache. No cache, no
staleness, no race.

The shared cache itself stays: flavors, images and networks keep using it, so the cache setting
keeps its meaning.

## Clarifications

### Session 2026-11-25

- Q: Should the compose stacks' dead Redis wiring be removed, or the cache enabled through their configuration file? → A: Removed from `docker-compose.yml` and `docker-compose-sqlite.yml` (the two variables, the `redis` service, its dependency and its volume). The other compose files, including `docker-compose-kvm.yml`, start no Redis and set no cache variables; they take the cache setting from the mounted configuration file only and are unchanged.
- Q: How is the cache-on behaviour tested? → A: Automatically, against an in-process stand-in for the shared cache (one new test-only dependency), in addition to the cache-off regression test. The cache-on test must fail on the current code (RED) and pass after the change.
- Q: How is SC-006's performance claim verified? → A: Not by timing. It is replaced by a behavioural check: token issue and validation perform the same catalog read with the cache enabled and disabled. No benchmark; the ticket's revisit-on-measurement condition stands.
- Q: The fix removes Keystone's cache parameter, so a Keystone-level cache-on test cannot fail afterwards. What becomes of it? → A: It is the RED proof only. It is written against the current code with the in-process stand-in and shown failing, then removed in the change that removes the cache parameter. From then on "the catalog ignores the cache" holds by construction (FR-006) and by the source search (FR-007). The cache-off regression test stays.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - An endpoint change reaches the next token (Priority: P1)

An operator adds or removes a service or endpoint, or changes a service, through the Identity API. The next token
any user receives lists exactly the catalog the Identity API reports, whether or not the shared cache
is enabled.

**Why this priority**: Today the two surfaces disagree for up to a day with no way to force a refresh
short of flushing the cache or restarting. Clients send their tokens to whatever the catalog names,
so a stale catalog sends them to the wrong place, or to nowhere.

**Independent Test**: Create an endpoint through the Identity API, issue a token, check the catalog
lists it; delete it, issue a token, check it is gone. Repeat with the shared cache enabled.

**Acceptance Scenarios**:

1. **Given** the shared cache is enabled and reachable and a token was already issued for a project,
   **When** an operator creates an endpoint through the Identity API and a user of that project
   issues a new token, **Then** the new token's catalog lists the new endpoint.
2. **Given** the same setup, **When** the operator deletes that endpoint and the user issues another
   token, **Then** the endpoint is absent from that token's catalog.
3. **Given** the same setup, **When** a service is updated or deleted,
   **Then** the next token's catalog reflects the change.
4. **Given** the shared cache is disabled, **When** any of the above is done, **Then** the result is
   the same as with the cache enabled.

---

### User Story 2 - A deployment change reaches the first token after restart (Priority: P1)

An operator changes how the deployment advertises itself — the scheme (for example turning TLS on),
the advertised host, or the object-storage endpoint — or upgrades to a release whose database
migrations rewrite catalog rows. On every node, the first token issued after the restart carries the
new catalog.

**Why this priority**: The shared cache is external and persistent; a restart, rollout or upgrade
does not clear it. A TLS switch that keeps the host unchanged currently hands out plain-HTTP
endpoints for up to a day, and a release that adds or fixes catalog entries ships without clients
seeing them.

**Independent Test**: With the cache enabled, issue a token, restart with a different advertised
scheme, issue a token, check every endpoint uses the new scheme.

**Acceptance Scenarios**:

1. **Given** the shared cache is enabled and a token was issued under the old scheme, **When** the
   deployment restarts with a different advertised scheme, **Then** every endpoint in the first new
   token's catalog uses the new scheme.
2. **Given** the same, **When** the deployment restarts with a different object-storage endpoint
   configured, **Then** the first new token's catalog names the new object-storage endpoint.
3. **Given** a release whose migrations add or change catalog rows, **When** it starts against a
   database and cache that served the previous release, **Then** the first token's catalog includes
   the migrated rows.
4. **Given** several nodes sharing one cache during a rolling update, **When** the update completes,
   **Then** no node serves a catalog built by the previous release.

---

### User Story 3 - The sample deployments do what they say (Priority: P2)

A developer starts the bundled container-compose stacks (PostgreSQL and SQLite variants). Nothing in
them claims to enable caching while doing nothing.

**Why this priority**: Both stacks set two cache-related environment variables that the binary never
reads, and start a persistent cache container that nothing uses. Whoever enables the cache for real
believes it was already tested there. Lower priority because it misleads rather than breaks.

**Independent Test**: Start each compose stack; it comes up healthy, and its definition contains no
setting the binary ignores and no service nothing uses.

**Acceptance Scenarios**:

1. **Given** either bundled compose stack, **When** it is started, **Then** every service comes up
   healthy and the Identity, Compute, Network, Block Storage and Image APIs answer.
2. **Given** either compose definition, **When** it is read, **Then** it sets no cache-related
   environment variable and starts no cache service.

---

### User Story 4 - The documentation matches the behaviour (Priority: P3)

An operator reading the configuration and upgrade guides learns that the catalog is read on every
request, and is no longer told to set the advertised host to make catalog caching work, or to wait
24 hours for catalog changes to appear.

**Why this priority**: The current guides describe a mechanism that no longer exists. Wrong but
harmless once the behaviour is fixed.

**Independent Test**: Search the operator documentation for passages tying the catalog to the cache;
none remain, and the catalog is described as read on every request.

**Acceptance Scenarios**:

1. **Given** the configuration guide, **When** an operator reads the advertised-host section,
   **Then** it no longer says the advertised host decides what the catalog caches.
2. **Given** the upgrade guide, **When** an operator reads it, **Then** it states that catalog
   changes appear in the next token, and no passage says a cached catalog may lag by up to 24 hours.

---

### Edge Cases

- **Cache enabled but unreachable**: token issue and validation behave exactly as with the cache
  disabled; the catalog never depended on it.
- **Catalog entries left in an existing cache by a previous release**: never read, so harmless. They
  expire on their own TTL; no clean-up step is required of the operator.
- **A request addressing the deployment by a host other than the advertised one**: still gets a
  catalog naming the host it used (existing behaviour, unchanged); it simply no longer matters to any
  cache.
- **The database read fails part-way**: the existing fallback to the complete built-in catalog is
  kept unchanged; there is no longer a cache it must avoid poisoning.
- **Catalog edited directly in the database, bypassing the API**: reflected in the next token, the
  same as an API edit.
- **Other cached resources (flavors, images, networks)**: behave as before with the cache enabled.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Every token issue and every token validation that returns a catalog MUST build it from
  the current catalog records, with no copy kept between requests.
- **FR-002**: The catalog returned MUST be identical with the shared cache enabled, disabled, or
  enabled but unreachable.
- **FR-003**: A service created, updated or deleted, and an endpoint created or deleted, through the
  Identity API MUST be reflected in the catalog of the next token issued or validated after the change returns.
- **FR-004**: A change to the advertised scheme, the advertised host or the object-storage endpoint
  MUST be reflected in the first catalog served after the restart that applies it, on every node.
- **FR-005**: Catalog records added or changed by a database migration MUST appear in the first
  catalog served after that migration runs.
- **FR-006**: The Identity service MUST hold no reference to the shared cache once the catalog no
  longer uses it; nothing else in the Identity service uses it.
- **FR-007**: No code path MUST read or write a catalog entry in the shared cache (verifiable by the
  absence of the catalog cache key prefix from the source).
- **FR-008**: Code, tests and comments that exist only to govern catalog caching — the
  "which host may be cached" rule, its test, and the comments explaining the cache key — MUST be
  removed. Host resolution for the catalog's addresses MUST be kept unchanged.
- **FR-009**: Flavors, images and networks MUST keep using the shared cache exactly as before; the
  cache setting keeps its current meaning for them.
- **FR-010**: The bundled PostgreSQL and SQLite compose stacks (`docker-compose.yml`,
  `docker-compose-sqlite.yml`) MUST NOT set cache-related environment variables the binary ignores,
  and MUST NOT start a cache service, its dependency or its volume. No environment-variable override
  is added in their place. Compose files that already start no cache service (for example the KVM
  stack, which reads the cache setting from its mounted configuration file) are unchanged.
- **FR-011**: Operator documentation MUST describe the catalog as read on every request and MUST NOT
  describe catalog caching, catalog cache keys, or a catalog lag tied to the cache TTL.
- **FR-012**: An automated test MUST show that an endpoint created through the Identity API appears in
  the next token's catalog and a deleted one disappears from it, running against the project's real
  test database. Before the change, the same test run with the shared cache enabled against an
  in-process stand-in MUST fail — the new endpoint, the deleted endpoint and a changed advertised
  scheme all go unseen — as the recorded proof of the defect. That cache-enabled variant is removed
  with Keystone's cache parameter; the cache-disabled test stays and passes after the change.
- **FR-013**: The change MUST add no migration, no runtime dependency, no configuration setting and
  no API change. The in-process cache stand-in of FR-012 is a test-only dependency of the proof
  step and leaves the module again with it; the finished change adds no dependency.

### Key Entities

- **Service catalog**: The per-token list of services and their endpoints, built from the service
  and endpoint records with the request's host, the advertised scheme and the object-storage endpoint
  substituted in. After this change it has no stored form outside the records it is built from.
- **Shared cache**: The optional external key-value store enabled by the cache setting. It keeps
  serving flavors, images and networks; it no longer holds catalogs.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: With the shared cache enabled, an endpoint created through the Identity API is listed
  in the very next token's catalog — 0 seconds of lag, against up to 24 hours today.
- **SC-002**: With the shared cache enabled, after a restart that changes the advertised scheme,
  100% of endpoints in the first new token's catalog use the new scheme.
- **SC-003**: A search of the source for the catalog cache key prefix returns no results.
- **SC-004**: The full unit test suite passes on both database backends, and the contract test suites
  pass, with no test removed other than the one governing catalog caching.
- **SC-005**: Both bundled compose stacks start healthy with no cache service and no ignored
  cache-related variables.
- **SC-006**: Token issue and validation perform the same catalog read whether the shared cache is
  enabled or disabled; with the cache enabled, issuing or validating a token writes and reads no
  catalog entry in it — true by construction once the Identity service holds no cache (FR-006) and
  checked by SC-003's source search.

## Assumptions

- The ticket's decision (Option 0: stop caching the catalog) is settled and not revisited here. If a
  measurement ever shows token issue or validation bound by the catalog read, raw-row caching with
  invalidation is the documented fallback — not part of this feature.
- The compose stacks drop their cache wiring rather than demo the cache (see Clarifications): once
  the catalog is not cached they have nothing of their own to show with it. The cache remains
  available to anyone who enables it in the configuration file.
- The failing-before demonstration for User Stories 1 and 2 is automated (FR-012) against an
  in-process cache stand-in, so it needs no cache server in the test setup or CI. The ticket's manual
  run against a real cache server stays a host check, not the RED proof.
- Stale catalog entries a previous release left in an existing cache need no migration or clean-up;
  nothing reads them and they expire on their own TTL.
- The Identity API has no endpoint-update operation today, so FR-003 names the five catalog writes
  that exist. Adding endpoint updates is out of scope.
- The unrelated Horizon report passage about catalog caching describes Horizon's own session cache,
  not Lightstack's, and is left alone.
