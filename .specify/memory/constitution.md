<!--
SYNC IMPACT REPORT — constitution amendment
===========================================
Version change: 1.5.0 → 1.6.0 (MINOR — one new permitted exception; one exception's scope stated to
match the component it was written for; one divergence registered)

Modified principles:
  - I. OpenStack Flamingo API Compatibility — rules unchanged. Known Divergences gains entry 5:
    a security group resolves only in the project it applies to, administrators included
    (SPEC-033 FR-004, FR-018).
  - II. Synchronous Operations — title and rules unchanged.
    - "Permitted exception — stateless reconciliation loops": the loop corrects the record, or
      makes node-local state match it — re-asserting what the record holds and removing what it no
      longer holds, its own state only (was: corrects the record). The drift it may correct now
      names node-local state lost to a restart or edited by hand, and three cases no request can
      reach without a queue: the nodes that did not serve a change, a request whose process stopped
      after storing its change, and a node whose own undo failed after the request had answered with
      an error. A request that changes node-local state applies it before answering, or fails and
      undoes it. Its three limits are otherwise unchanged.
    - New: "Permitted exception — a change undone when its external work fails", with five limits.
      It covers records whose node-local state is derived from them alone — stored, changed or
      removed — and leaves resources whose external work cannot be derived again to the
      reservation exception.

Changed sections:
  - Technology Constraints → Forbidden Patterns: the async-worker exemption names loops that make
    node-local state match the record.
  - Known Divergences from Upstream: entry 5.
  - Amendment Log: entry for v1.6.0.

Added sections: none.
Removed sections: none.

Template deferral check (templates are never edited — report only):
  - ✅ $HOME/.specify/templates/plan-template.md — Constitution Check still defers
    ("[Gates determined based on constitution file]", line 34).
  - ✅ $HOME/.specify/templates/spec-template.md — no principle references.
  - ✅ $HOME/.specify/templates/tasks-template.md — no principle references.
  - ✅ $HOME/.claude/commands/*.md — no reference to Article II's exceptions, to reconciliation
    loops or to the register, and no CLAUDE.md-only guidance.

Runtime guidance updated in the repo:
  - docs/specs/033-sg-port-membership/ — spec (both analysis sessions; FR-007b, FR-008, FR-008a–c,
    FR-010, FR-021a), plan (the Constitution Check cites v1.6.0), research (R5, R8, R19–R24) and
    tasks.
  - CLAUDE.md — Project Isolation: security groups resolve only in their own project (register
    entry 5). Two database backends: a change to security-group membership or rules, or to a
    floating IP's association, stores, applies and, on failure, undoes.

Follow-up TODOs:
  - ⚠ Neutron request paths log a failed node-local step and answer success: network, subnet and
    DHCP changes, routers and their interfaces and gateways, port forwarding, port creation's FDB and
    eBPF steps, port and instance-port teardown, and the unenforced LS-SG chains. Network creation's
    undo is not compare-and-set. They are departures, not permitted by this amendment; SPEC-033's
    follow-ups list them with their locations. SPEC-033 touches three of them: it moves a group
    deletion's LS-SG chain removal after the commit, keeps a floating IP's deletion leaving its port
    forwardings' NAT, and withdraws a deleted port's floating IPs, logging a withdrawal that fails.
  - ⚠ CLAUDE.md "The Nine Articles (Constitution)" and .claude/memory/constitution.md (v1.0,
    "Library-First" …) still describe a different, generic document (flagged since v1.2.0; unchanged
    here — needs a decision).
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

**Permitted exception — stateless reconciliation loops** (added in v1.1.0; scope stated in v1.6.0). A
periodic loop that compares recorded state against observed reality and corrects one from the other is
NOT an async worker and does not violate this principle. It corrects the record where reality is
authoritative — a guest that crashed — and makes reality match the record where the record is — a
node's firewall, NAT, bridges and DHCP, rebuilt from the database where a record holds them and
removed where none does any more. It removes only state of its own making, recognisable as such, and
removes nothing on a read of the records it knows to be incomplete. Either way:

- It carries no queue, no job records, no retries and no per-request state.
- It never defers work an API request should have done — every API operation still completes before
  returning, and persists its own result. A request that changes node-local state applies it on the
  node that serves it before answering, or fails and undoes it ("a change undone when its external
  work fails", below).
- It exists only to correct drift: a guest that crashed, a resource removed out of band, a record
  that has aged past its retention, or node-local state lost to a restart or edited by hand. For
  node-local state it also covers three cases no request can reach without a queue: the nodes that
  did not serve a change, a request whose process stopped after storing its change, and a node whose
  own undo could not restore its state after the request had already answered with an error.

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

**Permitted exception — a change undone when its external work fails** (added in v1.6.0). An
operation whose external work only applies node-local state derived from database records — a
floating IP's NAT and filter, a firewall rebuilt from security-group membership and rules — MAY
store its change to those records in one short transaction, do the external work, and undo the change
in a second transaction if the work fails. It does not violate this principle when all of the
following hold:

- Every record the operation stores, changes or removes is a complete final state without the
  external work: the node-local state is derived from the records alone, and the reconciliation loop
  re-asserts or removes it on every node. A resource whose external work produces what its record
  cannot derive again — a volume's backing storage, a guest's disk — keeps the reservation exception
  above. A stored record counts against its limits from the first transaction on, as a reservation
  would, and an undo that removes it releases them.
- The first transaction makes the operation's checks, takes its locks and stores the change. The
  external work runs after it commits and before any second transaction begins. No transaction is
  open across it.
- If the external work fails, the second transaction undoes exactly this request's change — a
  record it created is removed, one it removed is restored, one it changed is put back — on each
  record only if it still holds what this request left, so a later change by another request
  stays. The node-local state is then re-applied from the restored records, and the request answers
  as one transaction would have: an error, nothing changed.
- Every external step on the request path, and every wait for other work on the same node-local
  state, is bounded at one second (Article III). A step or wait that exceeds it is a failed external
  step.
- A change whose process stopped mid-request stays stored, as a committed single-transaction change
  would. It is a valid final state, not a reservation, and the node's reconciliation loop applies it
  within one interval — adding what the records hold and removing what they no longer hold.

**Rationale**: a record whose node-local state is derived from it is complete the moment it commits.
The loop builds that state from it on every node, the serving node included after a restart, so a
transitional row would tell readers that something is unfinished when nothing is. What the
reservation's limits protect is kept: no transaction is held across external work, a limit counts the
row from the first transaction, and the answer is the one the single-transaction form would have
given. What they add for resources the loop cannot rebuild — a transitional state other requests
must respect, an `error` state for an interrupted one — has nothing to apply to. Network creation
already has this shape — the row first, then the bridge, and a bridge that cannot be created takes the
row back — though its undo is not yet compare-and-set.

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

**Permitted exception — re-establishing a dependency in the background** (added in v1.5.0). A
background loop that restores the process's own connection to an external dependency, or completes a
one-time preparation that depends on it, does not violate this principle when all of the following
hold:

- No request waits on the loop. While the dependency is unreachable or the preparation incomplete, a
  request that needs it fails at once, as it would without the loop. The request may make at most one
  attempt of its own — or wait for one already running — within the dependency's bound.
- Every attempt, the loop's and a request's, is bounded as the dependency's calls are: 1 second for
  libvirt.
- The pause between the loop's attempts is capped — 1 to 5 seconds for libvirt — and the loop stops
  when the process does.
- The loop carries no queue, no job records and no per-request state, and accepts no work from a
  request.

**Rationale**: the libvirt reconnect loop shipped with `6ed30f7` (2026-09-24) — without it a libvirtd
restart left real mode dead until the pod was restarted by hand — contradicted "Never: retry loops,
exponential backoff", and no spec or plan recorded it. SPEC-031 extends it to the first connection and
to the console socket directory preparation, which real-mode readiness waits for: a retry driven only
by guest starts would deadlock, since no guest start reaches a pod that is not Ready. What the article
protects is unchanged: a request still gets immediate feedback.

### IV. Multi-Mode Architecture
Every service supports stub mode (development) and real mode (production).
- **Stub mode**: Returns fake data, no external dependencies, safe on macOS
- **Real mode**: Full implementation with libvirt/Ceph/iptables, requires Linux
- **Mode selection**: the configuration chooses the mode; an unset mode defaults to stub
- **Mode detection**: check the configured mode (`svc.libvirtMode == "stub"`,
  `svc.networkingMode == "stub"`). A nil `svc.vmManager` means no hypervisor was wired in (stub-mode
  tests); it never stands for real mode
- **No runtime fallback**: a configured real mode never falls back to stub. An unreachable dependency
  is an outage — the operations that need it fail as they do when it is lost later — never fake data

**Requirement**: All features must work in stub mode for development/testing without Linux.

**Rationale** (v1.5.0): read at runtime, "default to stub mode when dependencies unavailable" was how a
real-mode deployment whose libvirt was unreachable at startup reported servers `ACTIVE` with no guest
behind them, while the console path already treated the same state as an outage (SPEC-031). Stub mode
stays the default when no mode is configured, which is what the rule was for.

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
| 5 | A security group resolves only in the project it applies to, administrators included. An administrator's `addSecurityGroup` / `removeSecurityGroup` on another project's server resolves a name or an id in the server's project; port create and update accept only the port's project's groups. A group of any other project answers `404` | Add and remove resolve a name in the caller's own project and accept, by id, any group an administrator can see; Neutron lets an administrator put any project's group on a port | Names: administrators adding a tenant's group by name succeed today, and upstream's rule would refuse them. Ids: Lightstack's groups are private to their project, administrators included — listing and showing are scoped to the caller's project — so "any group an administrator can see" has no counterpart in its data model | SPEC-033 FR-004, FR-018 |

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
  (stateless reconciliation loops — including those that make node-local state match the record,
  re-asserting and removing it — and the correction of interrupted reservations — at startup and by a
  periodic pass — are permitted, see Article II; so is re-establishing a dependency in the
  background, see Article III)
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

### v1.6.0 — 2026-11-17, refined 2026-11-18 — a change undone when its external work fails; loops that make node-local state match the record (Article II); register entry 5 (Article I)

**Rationale**: SPEC-033 (cloud-storage#579) makes security-group membership reach the ports and the
firewall built from them. Its analysis found the plan justifying two departures from Article II
instead of amending it, the pattern v1.2.0 named as insufficient:

- A membership change stores rows and then rebuilds the node's firewall, which no transaction can
  hold. The reservation exception covers that shape only with a transitional row counted against
  limits; a membership row has no limit and no state between before and after that any reader uses.
- The network reconciler, the loop v1.1.0 was written for, re-asserts the firewall from the database.
  The exception's text spoke of correcting the record, and of drift the product did not cause, while
  the feature relies on the loop for the nodes that did not serve a change, for a request whose
  process stopped after storing its change, and for a node whose own undo failed after the request
  had already answered with an error.

The same analysis found a divergence from upstream recorded in the spec but missing from the
register: a security group resolves only in the project it applies to, administrators included.

A second analysis of SPEC-033 on 2026-11-18, before the amendment reached `main`, found its first
draft too narrow in one place and silent in two:

- It excluded every operation that creates or removes a record. A rule added to a group, a floating
  IP created on a port and a network created with its bridge all create records whose node-local
  state is derived from them alone, exactly as membership is. The line that matters is whether the
  loop can rebuild the external result from the record, not whether a row comes into being.
- It promised that the loop brings the nodes that did not serve a change into line, which for a
  removal means removing state no record holds any more. The loop only re-asserted.
- It added the rule that a request applies its node-local change before answering, or fails and
  undoes it, and recorded only two request paths that do not yet comply. There are more.

**Scope**: clarification and extension, not relaxation. The new exception is narrower than the
reservation: it applies only to records whose node-local state the loop derives from them alone, and
it requires a compare-and-set undo, a one-second bound on every external step and wait, and the answer
the single-transaction form would have given. The loop exception's wording now matches the component
it was written for: it re-asserts and removes, removing only what it can recognise as its own. Its
limits are unchanged, and the three cases it gains are ones no request can reach without a queue.

**Migration plan**: no deployment change. The network reconciler already re-asserts node-local state
and already removes bridges and DHCP of deleted networks; SPEC-033 extends the removal to floating-IP
NAT and filters. SPEC-033 is the first feature to use the change-then-undo form within its limits, for
membership, rules and floating IPs; network creation has the form's shape, but not yet its
compare-and-set undo. The rule that a request applies its node-local change before answering was already
implied by "it never defers work an API request should have done"; it is now stated. Request paths
that log a failed node-local step and answer success — network, subnet and DHCP changes, routers and
their interfaces and gateways, port forwarding, port creation's FDB and eBPF steps, port and
instance-port teardown, and the unenforced LS-SG chains — do not meet it, and network creation's undo
is not compare-and-set. They are departures, not permitted by this amendment, and SPEC-033's
follow-ups list each with its location. SPEC-033 touches three of them without bringing them in
line: a group deletion's LS-SG chain removal moves after the commit; a floating IP's deletion still
leaves its port forwardings' NAT; and a deleted port's floating IPs are now disassociated with it and
withdrawn after it, a failed withdrawal logged because the port's teardown has no undo. Register entry 5 records behaviour already on
`main` for names, and extends the same rule to ids and the port API.

### v1.5.0 — 2026-11-06 — re-establishing a dependency in the background (Article III); no runtime fallback to stub (Article IV)

**Rationale**: SPEC-031 (cloud-storage#630) found two places where the text no longer described the
product, or would have forbidden its fix:

- Article III forbids "retry loops, exponential backoff", yet `main` has re-dialed libvirt in the
  background with a 1-to-5-second backoff since `6ed30f7` (2026-09-24), and no spec or plan recorded
  it. SPEC-031 needs the same loop for the first connection and for the console socket directory
  preparation, because real-mode readiness waits for that preparation and no guest start can reach a
  pod that is not Ready.
- Article IV's "default to stub mode when dependencies unavailable", read at runtime, is exactly how a
  real-mode deployment with an unreachable libvirt reported servers `ACTIVE` with no guest behind
  them. SPEC-031 FR-019 removes that path.

**Scope**: an exception with four limits, as v1.1.0 and v1.3.0 added theirs: no request waits on the
loop, every attempt keeps the dependency's bound, the pause is capped, and the loop takes no work.
Article IV is restated to its original intent — stub when no mode is configured — and its runtime
reading is ruled out. Neither change relaxes what a request experiences.

**Migration plan**: none for the constitution itself. The reconnect loop has already shipped. SPEC-031
ships the first-connection and preparation retries and ends the runtime fallback; its
`docs/UPGRADING.md` notes (FR-024) cover what an existing real-mode deployment will notice.

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

**Version**: 1.6.0 | **Ratified**: 2026-03-13 | **Last Amended**: 2026-11-26
