# Specification Quality Checklist: Nova Gaps & Client Discovery

**Purpose**: Validate specification completeness and quality before proceeding to planning
**Created**: 2026-08-28
**Feature**: [spec.md](../spec.md)

## Content Quality

- [x] No implementation details (languages, frameworks, APIs)
- [x] Focused on user value and business needs
- [x] Written for non-technical stakeholders
- [x] All mandatory sections completed

## Requirement Completeness

- [x] No [NEEDS CLARIFICATION] markers remain
- [x] Requirements are testable and unambiguous
- [x] Success criteria are measurable
- [x] Success criteria are technology-agnostic (no implementation details)
- [x] All acceptance scenarios are defined
- [x] Edge cases are identified
- [x] Scope is clearly bounded
- [x] Dependencies and assumptions identified

## Feature Readiness

- [x] All functional requirements have clear acceptance criteria
- [x] User scenarios cover primary flows
- [x] Feature meets measurable outcomes defined in Success Criteria
- [x] No implementation details leak into specification

## Notes

### Validation iterations

**Iteration 1** — three items failed and were fixed:

1. *No implementation details*: the first draft named a client-side language ("Python traceback"), a
   transport ("over HTTP"), and storage internals ("append-only table", "the database", "usage
   query"). All were reworded to observable behaviour.
2. *Success criteria technology-agnostic*: SC-012 required verification "with no cross-referencing of
   the database" → "without consulting stored records".
3. *Requirements testable*: FR-016, FR-025 and FR-042 pointed at "*Clarifications*" for their
   substance before that section existed. The section was added and each requirement carried its own
   marker with the options spelled out.

**Iteration 2** — all items pass except the three clarification markers, held open for the user.

**Iteration 3 (2026-08-31)** — all three answered; **every item now passes.**

### The three decisions, and what they added

| Q | Answer | Effect on the spec |
|---|---|---|
| Q1 (US3) | Pre-flight refusable faults; land the rest in a failed state | FR-016a added — the split is now explicit, with both paths required to agree on what a user sees. A15 records that two code paths are deliberate. |
| Q2 (US7) | Retain past the server, reclaim by age | FR-042 inverted from "stated rule" to "MUST NOT remove", plus FR-042a/b/c for the age bound, the deployment setting and safe reclamation. A18 sets the default at 90 days; A19 explains age rather than count. |
| Q3 (US4) | Re-create the guest from stored state and start it | FR-025 rewritten, plus FR-025a (fidelity to the recorded server), FR-025b (the repair must be visible) and FR-025c (what happens when re-creation fails). A16 records that the data is all already stored; A17 that self-heal does not retire FR-024. |

Q3's answer was checked against the code before being written in: every field of the guest definition
the boot path assembles is derivable from stored state — flavor for processors/memory/disk, the
server's recorded ports for network attachments, the server's own storage for the disk and boot
configuration. What is missing is not data but a way to assemble it outside the creation path.

Q2's answer introduces the feature's only new **configuration** surface (a retention age) and a
periodic reclamation activity. It needs no new stored structure — the timestamps it filters on already
exist.

### Assumptions rather than markers

Everything else that could have been asked was resolved as a documented assumption (A1–A19). The two
with the widest reach:

- **A2** reverses today's precedence between the request host and the deployment-configured hostname.
  This is the behavioural change US1 turns on, and it changes what an existing deployment advertises.
- **A12** removes a filter on a server state nothing writes, rather than introducing the state.

### Audience note

"Non-technical stakeholders" is read as *operators and integrators* — the people who run the
deployment and the clients against it. Domain vocabulary the product's own users employ (floating IP,
hypervisor, endpoint override, version document) is retained; nothing about how Lightstack is built is.

### Clarify pass (2026-08-31)

An ambiguity scan found **seven** further material ambiguities beyond the ticket's three — a reminder
that "no markers left" is not the same as "no ambiguity left". All seven were asked and answered;
FRs went 63 → 77, then to 79 after the `/cl.analyze` pass added FR-052a and FR-025e.

| # | Ambiguity | Answer | Why it mattered |
|---|---|---|---|
| 4 | How wide is the self-link work? | Product-wide | Three out-of-ticket sites carry the *same* defect as B4: a hardcoded host, a relative path, a hardcoded scheme |
| 5 | Which host wins? | Request host; configured is fallback | Confirms A2, which changes what every existing deployment advertises |
| 6 | Divergence detection mechanism | Persist on action + periodic sweep | **The spec contradicted itself**: SC-006 claimed "0 divergences", which only a read-time check can deliver |
| 7 | Can a deleted server's history be read? | Owner and administrators | **The retention answer created this**: records were retained that the read path would never serve |
| 8 | Default boot-volume size | Flavor disk, floored at image minimum | `min_disk_gb` defaults to `0`, so it cannot be the sole source |
| 9 | Lifecycle failure statuses | Upstream's mapping | FR-026 was untestable without a mapping; a `503` would have been a compatibility risk |
| 10 | How is a self-heal recorded? | On the existing action record + a warning log | One option would have added a tenant-facing API surface |

Two of the seven were **self-inflicted** and worth noting: the divergence mechanism contradicted a
success criterion I had already written, and the retention decision produced records that the existing
read path answers `404` for even to an administrator (`internal/nova/diagnostics.go:68`).

Two answers added structure beyond the requirements: a `## Constraints & Tradeoffs` section recording
three rejected alternatives, and two new deployment settings (a reconciliation interval alongside the
retention age).

### Deferred, with rationale

- **Quota enforcement on the volume US9 creates.** Resolved against the code rather than asked:
  `CheckQuota` appears **zero** times in `internal/cinder/`, so volume creation is unmetered today and
  an implicit volume bypasses no existing check. Adding the product's first volume-quota enforcement
  inside a compute ticket would be inconsistent. Now an explicit Out of Scope entry.
- **The network-usability rule for US3's pre-flight** — reuses the network service's attach rule, the
  one the port allocation applies, rather than defining a second one. The visibility rule first named
  here admits external networks the allocation refuses.
- **Query shape for the floating-IP join on server listings** — an N+1 risk worth watching, but a
  planning concern with no bearing on observable behaviour.

### Readiness

No open questions. Ready for planning.
