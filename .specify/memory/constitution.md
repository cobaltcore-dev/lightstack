<!--
SYNC IMPACT REPORT — constitution amendment
===========================================
Version change: 1.3.0 → 1.4.0 (MINOR — a material change to one limit of a permitted exception)

Modified principles:
  - II. Synchronous Operations — title and rules unchanged. The fourth limit of "Permitted
    exception — reservation around external work" changes: the node that made a reservation
    corrects it at startup or by a periodic pass once its owner has stopped (was: once, at
    startup), and the partial result is reclaimed when the resource is deleted (was: by the
    correction). The correction stays a stateless pass that accepts no work. The exception's
    rationale gains the reason.

Changed sections:
  - Technology Constraints → Forbidden Patterns: the async-worker exemption names the correction of
    interrupted reservations at startup and by a periodic pass.
  - Amendment Log: entry for v1.4.0.

Added sections: none.
Removed sections: none.

Template deferral check (templates are never edited — report only):
  - ✅ $HOME/.specify/templates/plan-template.md — Constitution Check still defers
    ("[Gates determined based on constitution file]", line 34).
  - ✅ $HOME/.specify/templates/spec-template.md — no principle references.
  - ✅ $HOME/.specify/templates/tasks-template.md — no principle references.
  - ✅ $HOME/.claude/commands/*.md — no reference to Article II's exceptions, to reservations or to
    the startup correction, and no CLAUDE.md-only guidance.

Runtime guidance updated in the repo:
  - docs/specs/028-sqlite-database-layer/plan.md — the Constitution Check cites v1.4.0 and states
    the amended fourth limit; the post-design re-check names the two background loops added in
    review (the reservation owners' heartbeat and the periodic sweep) and both API changes (FR-035a,
    FR-036a); the project structure lists v1.4.0.
  - docs/UPGRADING.md ("Volumes interrupted by a restart") — interrupted reservations turn `error`
    at the next start or by the periodic pass, and keep their backing until they are deleted.

Follow-up TODOs:
  - ⚠ CLAUDE.md "The Nine Articles (Constitution)" and .claude/memory/constitution.md (v1.0,
    "Library-First" …) still describe a different, generic document (flagged since v1.2.0;
    unchanged here — needs a decision).
  - ⚠ README.md:587 and docs/API_COVERAGE_REPORT.md:127 quote 35 MB as historical measurements; left
    as recorded (carried from v1.3.0).
-->

# Lightstack Constitution

## Core Principles

### I. OpenStack Flamingo API Compatibility (NON-NEGOTIABLE)
Lightstack MUST maintain 100% API compatibility with OpenStack Flamingo (2025.2 release).
- All API endpoints must match Flamingo specifications exactly
- Horizon dashboard (Flamingo version) must work without modification
- OpenStack CLI tools must function identically to native OpenStack
- API responses must match expected JSON structure, fields, and data types
- Microversion support follows Flamingo microversion ranges

**Verification**: Every feature must pass contract tests using real OpenStack clients (Horizon, python-openstackclient, gophercloud).

**Permitted exception — recorded deliberate divergence** (added in v1.2.0). A divergence from
upstream behaviour does NOT violate this principle when it satisfies **all four** limits below.
Anything failing any one of them is a defect, not a divergence:

1. **Necessity.** Upstream's behaviour is either not implementable against Lightstack's data model, or
   matching it would break callers that work today. Convenience, effort and taste are not grounds.
2. **Recorded as a requirement.** The divergence is stated as a functional requirement in the
   feature's spec — not as a code comment, not as a row in a plan's tradeoff table, and not as a
   commit message. A reader of the spec must be able to find it without reading the code.
3. **Narrow and enumerated.** The divergence names exactly the surface it applies to. "This endpoint
   behaves differently" is not enumerable; "these five query parameters are refused" is.
4. **Never silent.** A client MUST be able to detect the divergence from the response it receives.
   A divergence a client cannot see is indistinguishable from a bug, and every defect this project
   has spent a feature repairing had that shape: a success status over a discarded intent.

Every divergence meeting these limits MUST also appear in the register below. The register exists
because the third entry in it survived undocumented across releases and was found only by reading
upstream's own guide during a later feature's research — nobody had known to look.

**Rationale**: this principle as originally written was contradicted by code already merged to
`main` in three places. Leaving it that way meant each new feature re-justifying the same departure
in its own plan, which an analysis pass correctly rated a critical conflict: a justification in a
plan is not an amendment. The exception is therefore stated once, with its limits, exactly as the
Article II exception was in v1.1.0.

### II. Synchronous Operations
All API operations complete before returning - no message queues, no async state machines.
- VM creation: libvirt calls complete within API request
- Network operations: namespace/bridge creation happens immediately
- Volume operations: storage backend operations complete synchronously
- Database updates happen within the same transaction as the operation

**Rationale**: Simplicity over scalability. Trade throughput for debuggability and operational simplicity.

**Permitted exception — stateless reconciliation loops** (added in v1.1.0). A periodic loop that compares
recorded state against observed reality and corrects the record is NOT an async worker and does not
violate this principle:

- It carries no queue, no job records, no retries and no per-request state.
- It never defers work an API request should have done — every API operation still completes before
  returning, and persists its own result.
- It exists only to correct drift the product did not cause: a guest that crashed, a resource removed
  out of band, or a record that has aged past its retention.

A loop that accepts work from a request, or that an API response waits on, remains forbidden.

**Permitted exception — reservation around external work** (added in v1.3.0; fourth limit amended in
v1.4.0). An operation whose
external work — a storage, libvirt, network or HTTP call — cannot run inside a database transaction
MAY record its result in two short transactions around that work instead of one. It does not violate
this principle when all of the following hold:

- The first transaction makes the operation's checks (quota, locks) and records a reservation: a row
  in a transitional state such as `creating`, which counts against every limit the finished row
  would count against.
- The external work runs after that transaction commits and before the next one begins. No
  transaction is open across it.
- The request finalises the row, or removes it together with any partial external result, before it
  returns. The client receives the response the single-transaction form would have given.
- A reservation left by a process that stopped mid-request is corrected by the node that made it:
  the row is set to `error` — at startup, or by a periodic pass once its owner has stopped — and its
  partial result is reclaimed when the resource is deleted, as upstream. The correction is a
  stateless pass that accepts no work.

**Rationale**: a transaction held across external work holds its locks for the length of that work —
on SQLite, the whole database's write lock for as long as an image copy takes. The work itself can
never be inside the transaction, so "the same transaction as the operation" was never achievable for
these operations. What the principle protects still holds: the request completes before it answers.
The correction leaves the partial result to the resource's delete because it cannot prove the owner
stopped: a process whose liveness signal lapsed — a database outage, a paused process — may still
be writing that result, and removing it from under a live operation is worse than leaving it to the
delete that cleans up the `error` resource anyway.

### III. Fail-Fast Design
External dependency failures return immediately (< 1 second timeouts).
- libvirt connection failures: 1 second timeout
- Ceph operations: 1 second timeout
- Database queries: connection pool exhaustion returns immediately
- Network namespace operations: fail fast if Linux capabilities missing

**Never**: Retry loops, exponential backoff, or delayed error responses. User gets immediate feedback.

**Permitted exception — bounded lock waits** (added in v1.3.0). Waiting for a database lock that
another request holds is not an external dependency failure, and does not violate this principle when
it is bounded:

- PostgreSQL row and advisory lock waits are unchanged.
- On SQLite, a writer waits for the database's single write lock through the driver's busy timeout,
  which MUST NOT exceed 5 seconds. The driver's internal sleep-and-retry while it waits is the lock
  wait itself, not an application retry loop.
- Lightstack itself MUST NOT retry a failed statement or transaction. A failure the wait cannot
  cure — SQLite's `SQLITE_BUSY_SNAPSHOT`, a read made stale by another writer — is prevented by taking
  the write lock when the transaction begins, not by retrying.

**Rationale**: SQLite serializes writers. Without a bounded wait, every concurrent write would fail at
once; with an unbounded one, a stuck writer would hang every request. Five seconds is far above the
milliseconds a write transaction takes once no transaction spans external work (Article II).

### IV. Multi-Mode Architecture
Every service supports stub mode (development) and real mode (production).
- **Stub mode**: Returns fake data, no external dependencies, safe on macOS
- **Real mode**: Full implementation with libvirt/Ceph/iptables, requires Linux
- Mode detection: Check `svc.libvirtMode == "stub"` or `svc.vmManager == nil`
- Default to stub mode when dependencies unavailable

**Requirement**: All features must work in stub mode for development/testing without Linux.

### V. Project Isolation
All resources scoped by `project_id` from JWT token.
- Database queries auto-filter by project_id
- Network namespaces per project for network isolation
- No cross-project resource access without explicit admin role check
- JWT contains: user_id, project_id, roles

### VI. Single Binary Deployment
Lightstack runs as one small, self-contained binary, including the embedded SQLite driver, with
embedded services.
- All six services (Keystone, Nova, Neutron, Cinder, Glance, Metadata) in cmd/lightstack/main.go
- Shared database connection pool — PostgreSQL, or one embedded SQLite file, chosen by the database
  URL
- Shared auth middleware (JWT validation)
- Shared logging middleware (structured JSON)

SQLite serves a single node. A deployment with several Lightstack nodes — multi-node networking or
replicas — MUST use PostgreSQL, and the binary refuses SQLite in multi-node mode.

**No**: Microservices, service meshes, or separate processes per service.

## Known Divergences from Upstream

Every deliberate divergence permitted by Article I's exception clause, with the requirement that
records it. **This list is exhaustive by construction**: a divergence absent from it is either a
defect or an undocumented departure, and both are bugs.

| # | Divergence | Upstream behaviour | Why permitted | Recorded as |
|---|---|---|---|---|
| 1 | A compute microversion header that does not parse is treated as no negotiation; the request succeeds | `400 Bad Request` | Refusing would turn requests that succeed today into failures, for no compatibility gain — no client sends a malformed header deliberately | SPEC-025 A-006 (`internal/nova/microversion.go`); restated as SPEC-025-followup FR-041a |
| 2 | A request with **no** compute version header resolves to the advertised **maximum** microversion | The advertised **minimum** is assumed | Adopting upstream's default would remove behaviour from every existing caller in a single release | SPEC-025-followup FR-040a |
| 3 | A top-level domain-scoped token request is refused with `400` naming the scope | Domain-scoped tokens are issued | Lightstack's role assignments bind a user to a role *within a project*, so there is no domain-level grant a domain-scoped token could authorize | SPEC-023 (`internal/keystone/handlers.go`) |
| 4 | Five server-list filter parameters (`host`, `hostname`, `availability_zone`, `tags`, and `ip` until its condition is met) are refused with `400` naming the filter | `host` and `hostname` are honoured for administrators and **silently ignored** for other callers; the rest are honoured | The product records none of those attributes, so honouring is impossible; and both compatible alternatives are silent — an empty list reads as a fact about the deployment, an ignored filter is the defect being repaired | SPEC-025-followup FR-004, bounded by FR-004a |

**Entry 2 is the cautionary one.** It predates every feature that has examined it, was not introduced
by any decision anyone recorded, and was found only when a later feature's research read upstream's
microversion guide directly. It also *caps* what a microversion gate can achieve — with no header
resolving to the maximum, a gate only ever affects a client that negotiates explicitly. An
undocumented divergence does not merely go unfixed; it silently bounds the value of later work.

## Technology Constraints

### OpenStack Version Targeting
- **Target Version**: OpenStack Flamingo (2025.2)
- **API Versions**:
  - Identity (Keystone): v3
  - Compute (Nova): v2.1 (microversions 2.1-2.90)
  - Network (Neutron): v2.0
  - Block Storage (Cinder): v3 (microversions 3.0-3.71)
  - Image (Glance): v2
  - Placement: v1.0 (microversions 1.0-1.40)

### Required Dependencies
- **Database**: PostgreSQL 14+, or SQLite embedded through `modernc.org/sqlite` (pure Go, no cgo) for
  single-node deployments — chosen by the database URL scheme. golang-migrate migrates both from the
  one `migrations/` folder.
- **Language**: Go 1.26+ (stdlib + minimal external deps; the embedded SQLite driver requires 1.26)
- **Real Mode** (Linux only):
  - libvirt + KVM (github.com/digitalocean/go-libvirt)
  - netlink (github.com/vishvananda/netlink)
  - iptables (github.com/coreos/go-iptables)
  - Optional: Ceph RBD (github.com/ceph/go-ceph), S3 (AWS SDK v2)

### Forbidden Patterns
- **No message queues**: RabbitMQ, Kafka, Redis streams
- **No async workers**: Celery, background job processors, or any loop an API response waits on
  (stateless reconciliation loops and the correction of interrupted reservations — at startup and by
  a periodic pass — are permitted; see Article II)
- **No state machines**: Conductor-style workflows
- **No service discovery**: Consul, etcd (services on fixed ports)
- **No container orchestration required**: Runs standalone or in Docker Compose

## Development Workflow

### Test-First Development
When implementing new features:
1. **Write contract tests first** - Use OpenStack clients (gophercloud, openstackclient)
2. **Get approval** - Review test strategy with user
3. **Confirm RED** - Tests must fail initially
4. **Implement** - Write code to make tests pass (GREEN)
5. **Refactor** - Clean up while keeping tests green

Integration tests in `test/` directory use bash + OpenStack CLI for end-to-end validation.

### Backwards Compatibility
- All API endpoints maintain OpenStack API compatibility
- Breaking API changes require OpenStack microversion bumps
- Horizon dashboard compatibility tested in `test/horizon_compat_test.sh`
- Database migrations must be reversible (up/down migrations)

### Security Requirements
- JWT secrets MUST be changed in production (config/lightstack.yaml)
- Database passwords via environment variables in production
- Token TTL default: 24 hours (configurable)
- bcrypt for password hashing (no plaintext passwords in database)

## Quality Gates

### Before Merge
- [ ] OpenStack CLI compatibility verified (openstack server create, etc.)
- [ ] Horizon dashboard tested (if UI-affecting changes)
- [ ] Contract tests pass (test/contract/ or test/integration_test.sh)
- [ ] Both stub and real modes tested (if applicable)
- [ ] Database migrations applied successfully (up and down) on both backends — PostgreSQL and SQLite
- [ ] Both database backends exercised (unit tests and contract suite) when a change touches SQL or
  the schema
- [ ] No hardcoded credentials or secrets

### Performance Standards
- API response time: < 100ms for metadata operations
- Fail-fast timeouts: 1 second for external dependencies
- Database connection pool: 20 connections default
- VM creation: Complete within 10 seconds (real mode)

## Governance

This constitution supersedes all other development practices. When in conflict:
1. **OpenStack API compatibility** always wins
2. **Simplicity** over performance optimization
3. **Synchronous operations** over async complexity
4. **Fail-fast** over retry resilience

Amendments require:
- Documentation of rationale
- User approval for breaking changes
- Migration plan for existing deployments

## Amendment Log

### v1.4.0 — 2026-11-04 — correcting interrupted reservations: a periodic pass, reclaim on delete (Article II)

**Rationale**: SPEC-028's review found two ways in which the reservation exception's fourth limit no
longer described a safe implementation. A correction run only at startup misses a process that
crashed moments before the restart: its liveness signal is still fresh, so it cannot yet be told
from a live process sharing the node, and nothing would revisit it — its reservation would stay
`creating` until the next restart. And a correction that reclaims the partial result can remove it
from under a live process whose liveness signal only lapsed — a database outage, a paused process —
while that process is still building it. Upstream Cinder's own cleanup sets such a resource to
`error` and leaves its backing to the delete (`cinder/volume/manager.py`, `_do_cleanup`).

**Scope**: when the correction may run widens to a periodic pass, bounded as the v1.1.0
reconciliation-loop exception bounds such loops: no queue, no job records, no per-request state, and
no work accepted from a request. What the correction may do narrows: it sets the row to `error` and
removes no external result. The other three limits are unchanged.

**Migration plan**: none required. SPEC-028, which introduced the exception, is not yet released; its
plan's Constitution Check cites the amended limit.

### v1.3.0 — 2026-10-12 — SQLite as a second database; reservations and bounded lock waits (Articles II, III, VI)

**Rationale**: SPEC-028 (cloud-storage#598, part 1 of #595) adds SQLite as an opt-in alternative to
PostgreSQL for single-node deployments. Its analysis found the plan contradicting the constitution in
three places, each justified in the plan rather than amended — the pattern v1.2.0 already named as
insufficient:

- Article VI and the Technology Constraints named PostgreSQL as the only database, and a binary size
  the build had already outgrown (47.0 MB stripped before SQLite).
- Article II required an operation's database updates in one transaction, while volume and snapshot
  creation must not hold a transaction across a storage call — on SQLite that would hold the whole
  database's write lock for an image copy.
- Article III forbade retry loops, backoff and delayed errors, while SQLite's busy timeout is a
  bounded, internal sleep-and-retry for the write lock.

**Scope**: clarification and extension, not relaxation. PostgreSQL stays the default, and every
existing rule keeps its force. Both exceptions are bounded by explicit limits — the reservation by its
four conditions, the lock wait by 5 seconds and the ban on application retries.

**Migration plan**: none required. PostgreSQL deployments see no change. SPEC-028 implements the
exceptions, and its plan's Constitution Check cites them.

### v1.2.0 — 2026-09-02 — bounded deliberate divergence permitted (Article I)

**Rationale**: Article I stated that Lightstack MUST maintain 100% API compatibility, while code already
merged to `main` diverged from upstream in three places — a tolerated malformed microversion header
(SPEC-025), a missing version header resolving to the maximum rather than the minimum (origin
unrecorded, found during SPEC-025-followup research), and a refused domain-scoped token request
(SPEC-023). A fourth was proposed by SPEC-025-followup FR-004. The principle was describing a rule
the project had already, deliberately, departed from — the same situation Article II was in before
v1.1.0 — and every feature was left re-justifying the departure in its own plan. An analysis pass
rated that a CRITICAL conflict, correctly: a plan's tradeoff table is not an amendment.

**Scope**: clarification, not relaxation. The four limits are deliberately hard to satisfy —
necessity, recorded as a requirement, narrow and enumerated, and never silent. The fourth limit is
the load-bearing one: it is what separates a divergence from the class of defect this project has
repeatedly had to repair, where a success status was returned over a discarded intent. Divergence
for convenience remains forbidden, and the compatibility target is unchanged.

**Migration plan**: none required. No behaviour changes. Three shipped divergences stop being
undocumented deviations and are entered in the new register; the fourth is permitted on the same
terms. Future divergences require a register entry, which is the enforcement mechanism.

### v1.1.0 — 2026-08-31 — stateless reconciliation loops permitted (Article II)

**Rationale**: the Forbidden Patterns list read as prohibiting any background loop, which conflicted
with code already merged to `main` — the real-mode network-state reconciler (SPEC-009,
`internal/neutron/reconciler.go`), started from `cmd/lightstack/main.go`. It is not an async worker in
the sense the principle guards against: no queue, and no API response waits on it. The principle was
describing a rule the project had already, deliberately, departed from. Rather than leave every future
feature to re-justify the same deviation, the exception is now stated once, with its limits.

**Scope**: clarification, not relaxation. Deferring request work to a background loop is still
forbidden, and the synchronous-operations rule for API operations is unchanged.

**Migration plan**: none required. No behaviour changes. One shipped component stops being a deviation,
and SPEC-025's compute state reconciler and action-history reclamation are permitted on the same terms.

**Version**: 1.4.0 | **Ratified**: 2026-03-13 | **Last Amended**: 2026-11-04
