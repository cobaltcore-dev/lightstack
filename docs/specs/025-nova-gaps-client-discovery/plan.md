# Implementation Plan: Nova Gaps & Client Discovery

**Branch**: `feature/547` (feature dir `025-nova-gaps-client-discovery`) | **Date**: 2026-08-31 |
**Spec**: [spec.md](spec.md)
**Input**: Feature specification from `docs/specs/025-nova-gaps-client-discovery/spec.md` (cloud-storage#547)

## Summary

Ten defects in two families — **client discovery** (version documents and self-links that clients parse
before any API call) and **compute answers that are wrong in a way the status code cannot reveal**.

The technical approach is dominated by one finding that runs through most of the work: **the product
already has the right shape in almost every case, and the defect is that one caller doesn't use it.**

- Self-links: one shared producer exists (`common.BaseURL`); five services bypass it or mis-call it.
- Server references: one shared resolver exists (`nova.targetInstance`); the action sub-handlers ignore
  the resolved value the dispatch table already hands them.
- Reconcilers: two exist (`neutron.NetworkReconciler`, `console.Reconciler`); state reconciliation and
  history reclamation follow that pattern rather than inventing one.
- Boot-from-volume: both halves exist (Cinder's create-from-image, Nova's root-volume attach); F16 wires
  them together through a narrow interface, matching the existing `NeutronService` precedent.

Consequently the change is broad but shallow: **no new Go modules**, two small reversible migrations, and
two new configuration settings. The two genuinely new pieces of behaviour are the self-heal of a missing
hypervisor guest and the `fault` field that makes a failed boot readable.

## Technical Context

**Language/Version**: Go 1.26 (module pins `go 1.26`; Constitution requires 1.21+)
**Primary Dependencies**: `gin-gonic/gin` (HTTP), `jackc/pgx/v5` + `pgxpool` (PostgreSQL),
`digitalocean/go-libvirt` (`DomainDefineXML`, `DomainCreate`, `DomainLookupByUUID`, `DomainGetState` —
all already used), `google/uuid`. **No new Go modules. No new chart dependencies.**
**Storage**: External PostgreSQL. **Two migrations**, both reversible:
`067_instance_fault` (`instances.fault_code`, `fault_message`, `fault_created`, all nullable) and
`068_instance_actions_time_index` (index on `instance_actions(start_time)` for the reclamation sweep).
**Testing**: `go test ./...` (unit), `test/contract/nova` + `test/contract/keystone` (gophercloud
contract), bash + `python-openstackclient` in `test/` (integration), `helm-unittest` (chart values).
**Target Platform**: Linux for real mode (libvirt/KVM); stub mode anywhere. Real-mode acceptance on k3s.
**Project Type**: Single Go binary hosting six HTTP services (`cmd/lightstack/main.go`).
**Performance Goals**: FR-024c is the binding one — reading a server MUST NOT contact the hypervisor, so
a server listing stays O(1) in hypervisor calls regardless of instance count. Constitution standards
hold: <100ms metadata operations, 1s fail-fast on external dependencies, VM creation within 10s.
**Constraints**: Constitution Article I (Flamingo API compatibility) is non-negotiable and is the
tie-breaker for every shape decision in this feature — it is *why* several of the clarifications went
the way they did. Two new deployment settings must have documented defaults so an existing deployment
that sets neither still behaves correctly.
**Scale/Scope**: 10 defects, 9 user stories, 80 functional requirements. Touches 5 of the 6 services
(compute, identity, network, block storage, placement) plus shared middleware and config. ~17 handler
signature changes in one file are the largest single mechanical block.

## Constitution Check

*GATE: evaluated before Phase 0, re-evaluated after Phase 1 design, and re-evaluated again after the
`/cl.analyze` pass on 2026-08-31. Result: **PASS.** The background-loop conflict was resolved by
amending the constitution to v1.1.0 rather than by justifying a deviation; two tracked items remain,
neither of which conflicts with a principle.*

### I. OpenStack Flamingo API Compatibility (NON-NEGOTIABLE) — PASS, and this is the point

The whole feature exists to close compatibility gaps, and Article I decided four clarifications:
product-wide self-links, upstream's lifecycle status mapping, upstream's `addresses` shape carrying
both address kinds, and owner-or-admin access to a deleted server's action history.

**One nuance found during research.** Upstream gates deleted-instance action history on **microversion
2.21**, and access is "restricted by default to administrative users or the owner of the server" —
which confirms FR-043 exactly, but adds a gate Lightstack cannot currently express: **Nova has no microversion
negotiation at all.** It emits `OpenStack-API-Version` (`handlers.go:288`) and never reads the request
header. Resolution: implement a minimal, local microversion check for this one endpoint (R7). It is the
product's first such gate; generalising it is explicitly out of scope and flagged for its own ticket.

### II. Synchronous Operations — PASS (under the v1.1.0 permitted exception)

Two background loops are required by the spec: state reconciliation (FR-024b) and action-history
reclamation (FR-042a). This was originally recorded here as a justified deviation on the strength of
in-tree precedent. **The `/cl.analyze` pass rejected that framing**: precedent is not an exemption the
constitution grants, and the Forbidden Patterns list as written was already contradicted by code merged
to `main` — `neutron.NewNetworkReconciler` (SPEC-009, `cmd/lightstack/main.go:213`) and `console.NewReconciler`
(SPEC-021, `main.go:266`).

The constitution was therefore **amended to v1.1.0** (2026-08-31) to state the exception once, with its
limits: a stateless loop that corrects drift the product did not cause is permitted; a loop that accepts
work from a request, or that an API response waits on, remains forbidden. Both of this feature's loops
sit inside that boundary — neither holds a queue and no response waits on either.

Server creation is *already* asynchronous (VM creation runs in a goroutine, `handlers.go:629`), which is
a pre-existing tension this feature does not widen. FR-016a **reduces** it: the pre-flight moves the
common failure into the synchronous path.

### III. Fail-Fast Design — PASS

No retry loops, no backoff anywhere in this plan. The self-heal (FR-025) performs one define-and-start
inside the lifecycle action's existing 10-second context, which sits inside the Constitution's own
10-second VM-creation standard. Rejecting `503` for an unreachable hypervisor (Q6) is not a fail-fast
question — timeouts are unchanged; only the status naming the condition is.

### IV. Multi-Mode Architecture — PASS, with stub semantics stated per story

Every story must work in stub mode. Explicitly:
- **B12**: stub already persists state transitions; self-heal is a no-op there (nothing to re-create).
- **B13**: pre-flight refusal is fully testable in stub mode; the `fault` field is written in both modes.
- **B17**: pure read path — works in both.
- **F16**: stub mode creates the volume row and the attachment, with no backing file, matching how the
  block-storage service already behaves in stub mode.
- **B3/B4/B7/B14/B15/B16**: mode-independent.

### V. Project Isolation — PASS

FR-043 widens a read, so it gets explicit treatment: authorization comes from
`instance_actions.project_id` (the **owning** project, as SPEC-023 established) plus
`common.HasRole(c, common.RoleAdmin)`. FR-043b requires a non-owner to be unable to distinguish a
deleted server from one that never existed, which is the same *absent* answer the rest of the compute
surface gives. B15 keeps SPEC-023's project rebinding intact — removing it would break the override.

### VI. Single Binary Deployment — PASS

Both new loops are goroutines in the existing process, started and stopped alongside the existing two.

### Development Workflow / Quality Gates — PASS

Both migrations get down migrations (reversible, as required). Contract tests are written first per
Article III of the project's CLAUDE.md workflow. No hardcoded credentials; the two new settings are
plain durations.

## Project Structure

### Documentation (this feature)

```text
docs/specs/025-nova-gaps-client-discovery/
├── spec.md                  # Phase -1 (/cl.specify + /cl.clarify) — 80 FRs, 10 clarifications
├── plan.md                  # This file
├── research.md              # Phase 0 — 12 decisions
├── data-model.md            # Phase 1 — entities, migrations, config
├── quickstart.md            # Phase 1 — how to exercise each story
├── contracts/               # Phase 1 — 7 interface contracts
│   ├── discovery-and-selflinks.md
│   ├── identity-links.md
│   ├── server-boot-failure.md
│   ├── server-lifecycle-state.md
│   ├── server-action-addressing.md
│   ├── server-addresses.md
│   └── usage-and-action-history.md
├── checklists/
│   └── requirements.md      # spec quality — 16/16
└── tasks.md                 # Phase 2 (/cl.tasks — NOT created here)
```

### Source Code (repository root)

No new top-level directories. Existing layout, files this feature touches:

```text
internal/
├── common/
│   ├── baseurl.go              # US1: precedence reversal + port applied in both branches
│   ├── baseurl_test.go         # US1: existing test PINS the wrong behaviour — must be rewritten
│   └── config.go               # NovaConfig: + StateReconcileInterval, + InstanceActionRetention
├── nova/
│   ├── handlers.go             # US1 (GetVersion links), US3 (pre-flight + fault), US4 (persist +
│   │                           #   self-heal + statuses), US6 (addresses join), US7 (dead filter),
│   │                           #   US9 (BDM volume_size + volume creation), + VMSpec extraction
│   ├── advanced_actions.go     # US5: 17 handler signatures take the resolved id/project
│   ├── diagnostics.go          # US7: deleted-server action history + microversion gate
│   ├── server_update.go        # US1: scheme-less self-link
│   ├── console.go              # US1: deriveConsoleOrigin re-assessed after the shared fix
│   ├── state_reconciler.go     # US4: NEW — the periodic state sweep + history reclamation
│   ├── lifecycle_test.go       # US4: NEW — stop/start persistence, status mapping, reboot
│   ├── selfheal_test.go        # US4: NEW — re-create from stored state, and its failure path
│   ├── state_reconciler_test.go# US4: NEW
│   ├── boot_failure_test.go    # US3: NEW — fault, partial failure, deletability
│   ├── addresses_test.go       # US6: NEW — fixed + floating, admin cross-project
│   └── retention_test.go       # US7: NEW — reclaim by age, live server untouched
├── keystone/
│   ├── handlers.go             # US2: links on 2 reads + 6 collection sites; 5 scheme-less fixed
│   └── groups.go               # US1/US2: 4 scheme-less links
├── neutron/network.go          # US1: relative "/v2.0" version link
├── cinder/volumes.go           # US1: hardcoded http://lightstack:8776/v3/ ; US9: create-from-image reuse
├── placement/placement.go      # US1: hardcoded http:// scheme (2 sites)
└── middleware/logging.go       # US8: capture caller project before c.Next()

pkg/hypervisor/
├── libvirt.go                  # US4: distinguish not-found from fault; re-define support
└── xml_template.go             # US4: unchanged — GenerateVMXML is reused for the self-heal

migrations/
├── 067_instance_fault.{up,down}.sql
└── 068_instance_actions_time_index.{up,down}.sql

cmd/lightstack/main.go                 # US4/US7: start + stop the two new loops
charts/lightstack/                     # the two new settings as chart values
test/
├── contract/nova/              # version doc, addresses, action addressing, action history
├── contract/keystone/          # identity links
└── nova_gaps_test.sh           # NEW — integration pass for this feature
```

**Structure Decision**: the existing single-binary layout is kept unchanged. One new source file
(`internal/nova/state_reconciler.go`) because the state sweep is a distinct lifecycle object with its own
constructor and interval, mirroring `internal/console/reconciler.go`; the history reclamation is small
enough to live beside it rather than earning a second file. Everything else is an edit to a file that
already owns the behaviour.

## Complexity Tracking

| Violation | Why Needed | Simpler Alternative Rejected Because |
|-----------|------------|-------------------------------------|
| ~~Two background loops (Article II)~~ — **resolved, no longer a deviation** | FR-024b requires correcting drift the product did not cause; FR-042a requires bounding history growth. Neither has a request to hang off. | **Read-time hypervisor checks** were rejected in clarify Q3: a hypervisor call on every server read makes a listing slow in proportion to instance count (FR-024c forbids it). **Reclaim-on-write** was rejected because deletion is exactly when the history must *not* be touched. **Resolution**: constitution amended to v1.1.0 permitting stateless reconciliation loops, so this is now a supported pattern rather than an exception. |
| First microversion gate in the product (Article I nuance) | Upstream gates deleted-instance action history on 2.21. Serving it to a client that negotiated 2.1 is a fidelity break in the opposite direction. | **Serving it ungated** was rejected: Article I is non-negotiable and this feature is a compatibility feature; shipping a new incompatibility to fix an old one is incoherent. **Building general microversion negotiation** was rejected as disproportionate — it is a product-wide concern deserving its own ticket, not a rider on this one. |
| A new column pair for the boot fault (`instances.fault_*`) | FR-016 requires the reason be readable from the server's own record. Upstream exposes it as the `fault` object in server detail, and nothing in Lightstack emits `fault` today. | **Reusing `instance_actions.message`** was considered — it is already tenant-visible — and rejected: a user diagnosing a failed boot runs `server show`, not `server event list`, and Article I fixes the response shape. Recorded in research.md as R4. |

## Phase 0 — Outline & Research

**Status: complete.** See [research.md](research.md). Twelve decisions (R1–R12), covering:
the self-link precedence rule and its blast radius; the B12 root-cause investigation and why the chosen
fix does not depend on its outcome; the `fault` representation; the B14 refactor's true shape; the
reconciler pattern to follow; the microversion gate; the F16 service boundary; and the upstream
behaviours confirmed via Context7 against `/openstack/nova`.

**No `NEEDS CLARIFICATION` markers remain in Technical Context.** The one unknown that could not be
closed by reading — *why* a persistent libvirt domain disappears after a graceful shutdown — is
deliberately **not** a blocker: R2 records the ranked hypotheses and the diagnostic procedure, and shows
that FR-024a, FR-024b and FR-025 together produce correct behaviour whatever the cause turns out to be.

## Phase 1 — Design & Contracts

**Status: complete.** Artifacts: [data-model.md](data-model.md), [contracts/](contracts/) (7 files),
[quickstart.md](quickstart.md).

## Phase 2 — Task planning approach

Not executed here (`/cl.tasks` owns it). The intended decomposition, for continuity:

1. **The shared self-link rule first, as a foundational phase of its own — then US1.** `common.BaseURL`
   is the only change every other story's tests read through (self-links appear in server, flavor, image
   and network responses), so it is not story work at all: it belongs in the blocking phase, with its
   own test rewritten first (RED). US1 then adds the compute version document and the product-wide
   sweep on top. `tasks.md` implements exactly this split — Phase 2 for the rule, Phase 3 for US1.
2. **US2, US5, US6, US7's dead filter, US8** are independent of one another and of real mode — good
   parallel candidates once US1 has landed.
3. **US4 before US3.** The `VMSpec` extraction that US4 needs is also the cleanest place for US3's
   pre-flight to read the requested networks from, and US3's `fault` write is used by US4's
   `FR-025c` failure path.
4. **US9 last.** It depends on US3's failure semantics (FR-055) and touches the boot path US4 refactors.
5. **Migrations 067/068 land with the story that needs them**, not up front, so a partial merge never
   leaves an unused column.
6. **Real-mode host pass covers US3, US4, US6, US9 together** — one k3s stack, one router, one external
   network, one floating IP. Stub pass covers the rest.

## Progress

- [x] Constitution check (initial) — PASS with 3 tracked deviations
- [x] Phase 0 — research.md
- [x] Phase 1 — data-model.md, contracts/, quickstart.md
- [x] Constitution check (post-design) — unchanged; the design added no new deviation
- [x] Agent context updated
- [x] `/cl.analyze` cross-artifact pass (2026-08-31) — 1 CRITICAL, 4 HIGH, 9 MEDIUM/LOW, all remediated;
      constitution amended to v1.1.0, FR-052 predicate corrected, 6 verification tasks added
- [x] Constitution check (post-analyze) — **PASS**, no remaining principle conflict
- [ ] Phase 2 — tasks.md (`/cl.tasks`)
