# Feature Specification: Inject the database pool through service constructors (ADR-001)

**Feature Branch**: `feature/533` (feature id: `027-database-pool-injection`; lands in PR cobaltcore-dev/lightstack#79)
**Created**: 2026-10-01
**Status**: Draft
**Input**: cobaltcore-dev/cloud-storage#554 — "[FEAT] - lightstack - Refactor - inject `*pgxpool.Pool` through service constructors (implement ADR-001)" (epic). Triggered by the PR #79 review comment on `internal/common/project.go:46` ([r4148510424](https://github.com/cobaltcore-dev/lightstack/pull/79#discussion_r4148510424)). The design is fixed by [ADR-001](../../adr/ADR-001-database-dependency-injection.md) (Accepted 2026-04-20) and [SPEC-006](../006-v070-foundation/README.md) Step 4.

## Context

Every Lightstack service reads its database through one process-wide, mutable handle. Nothing is handed to a service; each one reaches for that handle directly. ADR-001 decided to replace that with a connection pool passed to each service when it is built. The decision is made and is not reopened here. This feature is the migration.

Measured on this branch (`44ed3d1`, 2026-10-01):

| Package | Production references | Test references |
|---|---|---|
| `internal/compute` | 3 | — |
| `internal/metadata` | 10 | 5 |
| `internal/teardown` | 1 | — |
| `internal/common` | 1 | — |
| `internal/glance` | 54 | 13 |
| `internal/keystone` | 104 | 57 |
| `internal/cinder` | 115 | 29 |
| `internal/neutron` | 188 | 39 |
| `internal/nova` | 214 | 35 |
| `test/benchmark` | — | 6 |
| **Total** | **690 in 60 files** | **184 in 29 files** |

Beyond those, the handle is created and closed by the process entrypoint and the cleanup job, probed by the readiness check, and used internally by the transaction helper.

Two facts shape the work:

- **Nothing implements the pattern yet.** No service holds its own connection. The contributor guide and the architecture document both describe the injected pattern as if it existed, so a reader has no real example to copy.
- **Most references sit where a service is already in hand.** 667 of the 690 are inside methods of a component that can hold the pool. The other 23 are in 18 package-level helpers, plus the startup admin bootstrap and the cleanup job's entry point.

## Clarifications

### Session 2026-10-01 (pre-spec)

- Q: Where does the migration land — one PR per package as #554 proposes, or here? → A: **Here, on `feature/533`, inside PR #79, in full**, including deleting the process-wide handle.

### Session 2026-10-01

- Q: What is the test strategy for a behavior-preserving refactor (the constitution's Test-First rule)? → A: **Baseline, then identical.** The full gate results are recorded before any conversion, and the existing suites guard behavior. The Keystone demonstrator is written first and stays RED until Keystone takes a pool. Once the handle is deleted, the compiler is the final guard. There are no per-package demonstrators and no extra lint guard against a reintroduced pool variable.
- Q: How is the migration delivered — one commit, one per package, or milestone commits? → A: **One commit for the whole migration.** The steps run in FR-013's order, and each must build before the next starts. There is a single review-and-commit stop at the end. The branch owner commits.
- Q: May this spec, or the PR it lands in, reference other PRs or branches? → A: **No.** The spec is self-contained: it covers this branch's code only.
- Q: Does the PR carry a re-runnable conversion script? → A: **No.** The PR contains only the converted code. A script used to do the conversion stays outside the repository.
- Q: Do services that never touch the database take the pool too (#554 says "every `NewService()`")? → A: **No — database-using services only.** Placement never touches the database and keeps its parameterless constructor.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Nothing changes for anyone running or calling Lightstack (Priority: P1)

An operator deploys the new build the same way as the old one and an API client calls it the same way. Every endpoint answers with the same status codes, headers and bodies. Startup, migrations, the admin bootstrap, readiness and liveness, graceful shutdown and the `cleanup` job all behave as before, with the same connection-pool sizing. The change is invisible from outside the process.

**Why this priority**: A refactor that changes behavior is a regression. This story is the gate every other story is measured against.

**Independent Test**: Record the full gate set before the change — unit tests including the database-backed ones, the contract suite, lint, the quick integration suite, the Horizon UI probe and a short real-mode smoke on the host — and run it again after. Results are identical.

**Acceptance Scenarios**:

1. **Given** a running stack, **When** the contract suite runs, **Then** it reports the same passed/failed/skipped counts as the pre-change baseline.
2. **Given** an unreachable database at startup, **When** Lightstack starts, **Then** it stops with the same fatal error as before.
3. **Given** a running stack whose database becomes unreachable, **When** readiness is probed, **Then** it reports not-ready exactly as before, because it probes the same pool the services use.
4. **Given** the `cleanup` job, **When** it runs with the database reachable and again with it unreachable, **Then** it enumerates from the database in the first case and falls back to host-only cleanup in the second, as before.
5. **Given** a termination signal, **When** Lightstack shuts down, **Then** it drains, stops its listeners and closes the database pool once, as before.

---

### User Story 2 - A contributor tests a service against a database they supply (Priority: P1)

A contributor writing a database-backed test builds the service with a connection they created, without any process-wide setup. Two tests in the same process can point two services at two different databases. This is the payoff ADR-001 promised.

**Why this priority**: It is the reason for the migration and the ticket's named acceptance proof. It can be delivered by converting a single package (Keystone) before the rest.

**Independent Test**: A Keystone test opens its own connection to the test database, constructs the service with it, exercises a database-backed endpoint and passes when run on its own, with no package-level test setup involved. It is written first and is RED until Keystone takes a pool.

**Acceptance Scenarios**:

1. **Given** a test that opens its own database connection, **When** it constructs a service with that connection, **Then** every database access that service makes goes through that connection.
2. **Given** a test with no database at all, **When** it constructs a service for a code path that never touches the database, **Then** it works as it does today.
3. **Given** the database-backed test suites, **When** no test database is configured, **Then** they skip exactly as they do today.

---

### User Story 3 - Every component declares its database dependency where it is built (Priority: P2)

A reader of the startup wiring sees which components use the database by looking at how they are constructed, not by searching for a hidden process-wide handle. The contributor guide and the architecture document describe the pattern as it now exists and point at real code.

**Why this priority**: It completes the migration across all packages and removes the process-wide handle for good. It depends on the pattern from Story 2 being in place.

**Independent Test**: Search the repository for the process-wide handle — zero hits. Inspect every constructor of a database-using component — the pool is its first parameter. Follow the documentation's example — it names code that exists.

**Acceptance Scenarios**:

1. **Given** the codebase, **When** it is searched for the process-wide database handle, **Then** there are no hits in production or test code.
2. **Given** any database-using service, **When** its constructor is read, **Then** the database pool is its first parameter.
3. **Given** the contributor guide and the architecture document, **When** a reader follows their description of the pattern, **Then** it matches the real constructor signatures and links an existing example.
4. **Given** the converted codebase, **When** new code tries to use the removed handle, **Then** it does not build.

### Edge Cases

- **A service built without a database** (unit tests of database-free paths, stub-mode code): works as it does today. A path that does reach the database fails loudly, never silently.
- **Database consumers that are not services**: the background workers (network reconciler, VXLAN coordinator, compute node registry), the startup admin bootstrap, the readiness check, the transaction helper and the shared quota-target helper all receive the pool explicitly. The network reconciler gets it through the Neutron service it is built with. None obtains it implicitly.
- **Package-level helpers** (20 today: 18 with 23 direct references, plus 2 that reach the database only through the transaction helper): they receive the pool from their caller like everything else.
- **Cross-service calls**: Nova allocates ports through Neutron and reads Glance's image store. Each service uses the pool it was given, and in production that is the same pool.
- **The `cleanup` job** opens its own pool, as a separate process mode does today, closes it when done, and still falls back to host-only cleanup when the database is unreachable.
- **Concurrent admin bootstrap**: the existing concurrency test still passes.
- **Transactions**: same semantics, on the injected pool.
- **Helpers that only served the process-wide handle**: removed, not adapted. These are the two exported functions with no callers today, and the close function, whose callers now close the pool they own.
- **The benchmark suite** under `test/benchmark` still builds and opens its own pool.
- **Lint**: ADR-001 §5 planned to switch the `unused` linter off during the migration. It was never switched off, so it simply stays on throughout.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: The process-wide mutable database handle MUST be removed. No production or test code under `internal/`, `pkg/`, `cmd/` or `test/` may reference it.
- **FR-002**: Every database-using service MUST receive its database pool at construction, as the first constructor parameter (the ADR-001 pattern), and MUST use only that pool for all of its database access — handlers, helpers, background work and transactions. Placement, which never touches the database, keeps its parameterless constructor.
- **FR-003**: Every other database consumer — background workers, the startup admin bootstrap, the readiness check, the `cleanup` job, the transaction helper and shared cross-service helpers — MUST receive the pool explicitly, either directly from whoever creates or calls it, or through the service it is built with.
- **FR-004**: Production MUST use exactly one pool, shared by every service, created once at startup with today's configuration and closed once at shutdown after the listeners stop. DI is for architecture and testability, not isolation (ADR-001).
- **FR-005**: No externally observable behavior may change: API responses (status codes, headers, bodies), schema and migrations, configuration keys and environment variables, startup failure behavior, readiness and liveness semantics, and `cleanup` job behavior.
- **FR-006**: The change MUST be a clean break inside the binary. There is no transitional alias or compatibility shim for the removed handle, and helpers that served only the removed handle are deleted.
- **FR-007**: Injection MUST use the concrete pool directly. Interface-based injection, per-service pools and a repository layer are excluded (rejected in ADR-001; the constitution's Anti-Abstraction article).
- **FR-008**: At least one Keystone test MUST construct a service with its own pool and no package-level test setup, demonstrating the payoff (#554 acceptance criterion). It is written before Keystone is converted and stays RED until Keystone takes a pool. It is the only new test: per-package demonstrators and a lint guard against a reintroduced pool variable are not added, because the compiler guards the result once the handle is deleted.
- **FR-009**: Database-backed tests MUST keep running against a real PostgreSQL (the constitution's real-dependencies-over-mocks rule), through a pool the test package supplies to the services it constructs, and MUST keep skipping when no test database is configured.
- **FR-010**: Every quality gate MUST stay green, with results identical to a pre-change baseline recorded before any conversion: unit tests including the database-backed ones, the contract suite, lint with zero issues (the `unused` linter included), the quick integration suite and the Horizon UI probe. A short real-mode smoke on the host (the constitution's both-modes gate) MUST also behave as before; the branch owner runs it.
- **FR-011**: Documentation MUST describe the pattern as implemented. The contributor guide's DI paragraph and its database-query idiom example, the architecture document's constructor diagram, the troubleshooting and stub-placeholder code samples that show the removed handle, the open-findings entry H-3, and ADR-001 (a note on where it was implemented) all change accordingly.
- **FR-012**: The shared quota-target helper raised in review r4148510424 MUST take its pool from the calling service. The review thread is answered with what was done.
- **FR-013**: The work MUST be sequenced in ADR-001's smallest-first order. Each package's conversion is a step that builds and passes before the next begins, so a failure is always attributable to the step that caused it. The whole migration is delivered as **one commit**, reviewed once at the end.

### Key Entities

- **Database pool**: the one connection pool of a Lightstack process. Created by the process entrypoint (or by the `cleanup` job in its own process mode), lent to every component that needs it, closed once by its creator.
- **Service**: Keystone, Nova, Neutron, Cinder, Glance and Metadata — the database-using services. Each holds the pool it was constructed with. Placement uses no database and is not one of them.
- **Auxiliary consumer**: a non-service component that touches the database — the auth service, the network reconciler, the VXLAN coordinator, the compute node registry, the admin bootstrap, the readiness check, the `cleanup` job, the transaction helper and the shared quota-target helper.
- **Test pool**: a pool a test package or a single test opens against the test database and hands to the services it builds.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: References to the process-wide handle drop from 690 (production) + 184 (test) to **zero**, measured by a repository-wide search.
- **SC-002**: **100 %** of database-using components receive the pool at construction or call time. No component obtains it any other way, verified against the inventory in the plan.
- **SC-003**: The contract suite reports the **same passed/failed/skipped counts** as the pre-change baseline. Unit tests (including the database-backed ones), the quick integration suite and the Horizon UI probe pass, and lint reports **0 issues**. Every step of the real-mode host smoke behaves as before.
- **SC-004**: The Keystone demonstrator test passes when run **on its own**, against a pool it opened itself, with no package-level setup.
- **SC-005**: **Zero** current documents (excluding historical specs, ADR context sections and archives) show the removed handle as the way to query the database, and the DI description links at least one real example.

## Assumptions

- ADR-001 fixes the design: the concrete pool, injected through constructors, shared by all services. This feature does not revisit it.
- The work lands in PR #79 rather than one PR per package, as one commit (decisions 2026-10-01). Sequencing (FR-013) keeps every intermediate step building.
- Pool configuration semantics are unchanged. The migration CLI keeps working from the database URL and never touched the process-wide handle.
- One shared test pool per test package, opened by that package's test setup, is acceptable for the existing database-backed tests. FR-008's demonstrator is the exception that proves the pattern.
- The `unused` linter is already enabled — `.golangci.yml` has no disable list — so ADR-001 §5's staging step is moot.
- No API, schema, migration or configuration change is involved, so `docs/UPGRADING.md` gains no entry. The packages are `internal/`, so there are no external Go consumers.

## Dependencies

- ADR-001 and SPEC-006 Step 4 — the design and the original plan.
- cloud-storage#554 — the epic this feature implements. PR #79 links it.
- PR #79's review thread r4148510424 — answered once this lands.
- A test database for the database-backed unit tests. Locally that is `make db-up`. The tasks use a `lightstack_test` database on the compose stack's PostgreSQL, as CI does, because both setups claim the same container name. The compose stack also serves the contract, integration and Horizon-probe gates.
- A real-mode host for the smoke run.

## Out of Scope

- Interface-based injection, per-service pools and a repository pattern (rejected in ADR-001).
- Any API, schema, migration or configuration change.
- Code that is not on this branch.
- Splitting the migration into one PR, or one commit, per package.
