# Specification Quality Checklist: Tenant Scoping & Authorization Model

**Purpose**: Validate specification completeness and quality before proceeding to planning  
**Created**: 2026-08-10  
**Updated**: 2026-08-10 — re-validated after rebase onto `feature/545` @ `9992ebf`, after clarification (7 decisions), and after planning folded in a further scope decision (8 total)  
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

### Clarification outcome

All three original markers are resolved. Clarification surfaced four further material
ambiguities the specification had not flagged, and planning surfaced a fifth. 8 decisions
recorded in `## Clarifications → Session 2026-08-10`.

| # | Decision | Effect on scope |
|---|----------|-----------------|
| 1 | Domain scope is **refused** with a bad request, not honoured | Removed the largest unknown. No schema change, no migration, no domain authorization layer. |
| 2 | Override covers **every server operation except creation** (upstream `admin_or_owner`) | Bounded as a rule rather than a list. Added FR-004a. |
| 3 | Listings widen on **explicit opt-in** only, with upstream's parsing and silent-ignore | Added FR-005a/FR-005b. Verified against the Nova API reference. |
| 4 | Name matching **never spans projects**; cross-tenant addressing is by identifier | New. Closes a path where an operator could destroy the wrong tenant's server. Added FR-005c/FR-005d. |
| 5 | Usage reporting confidentiality **fixed here** | New. Added User Story 3 (P2) + FR-008b–FR-008d. Was a live cross-tenant leak. |
| 6 | Cross-project actions recorded against the **owning** project; refusals logged | New. Added FR-007/FR-007a/FR-007b, SC-002a/SC-002b. |
| 7 | Identity collection filtering **in scope**, network subnet filtering is not | New. Added User Story 5 (P3) + FR-024–FR-027, after checking #546/#547/#548. |
| 8 | The two ungated administrative surfaces found during planning are **in scope** | New. Added User Story 6 (P1) + FR-008e–FR-008g. Includes the feature's only privilege escalation. |

### Scope grew during clarification, then again during planning

The feature went from 3 user stories to **6**, and from 21 to **40** functional
requirements. Three of the six stories were not in the ticket at all:

- **User Story 3** is a confidentiality defect — the usage endpoints have no ownership
  check whatsoever, so any authenticated tenant can enumerate every project's consumption
  and read any named project's. Included because the specification would otherwise
  establish a rule that a live endpoint contradicts.
- **User Story 5** is the identity list-filter family (6 collections). Included by
  elimination: #546, #547 and #548 were read and none covers it, and #546's B6 is the
  same bug class scoped to the network service, which stays there (FR-027).

- **User Story 6** is the two remaining ungated administrative surfaces, found while
  surveying the code for the plan: the deployment-wide migration list (a read leak of the
  same shape as User Story 3) and host aggregates — **nine handlers with no role check at
  all, six of them writes**. This is the feature's only privilege escalation: everywhere
  else a tenant can read what is not theirs; here a tenant can *change* the deployment.

All three are defensible, and the last was folded in by explicit decision rather than by
implication. But this is real growth — roughly double the original ticket. If the branch
needs trimming, **User Story 5 is the cleanest thing to split out**: it has no
confidentiality or capability consequence, #545 already removed the sharp edge from the
one place it had bitten, and it is the only addition touching a service outside compute.
User Story 6 should not be trimmed — it is the highest-severity item in the feature.

### One root cause behind three of the defects

Every handler in `internal/nova` written against a table carrying a `project_id` column
got a scoping predicate, because a predicate was the obvious thing to write. The tables
behind host aggregates and the deployment-wide migration listing have no such column — so
no predicate was written, and **nothing was written in its place**. The usage endpoints
are the same story: usage is derived by grouping instances and has no owner row of its own.

This is why the ticket's own verification method — counting `HasRole` call sites — could
not have found them. They are not places where the wrong check was made; they are places
where the question never came up.

### Verification during clarification

Two decisions were checked against upstream documentation rather than asserted from
memory, and both checks changed the answer's basis:

- `all_tenants` is documented as an administrator **opt-in**, defaults to true when passed
  with no value, and is **ignored** rather than refused for non-administrators. Upstream's
  `project_id` filter additionally requires `all_tenants`, so Option C in that question was
  upstream's fuller form rather than a divergence — deferred as additive.
- The `server_id` path parameter is documented as a **UUID**. O3K's acceptance of a name
  there is a local extension, which is what made confining it to the caller's project the
  conservative choice rather than a restriction.

### Consequence surfaced, not smuggled

FR-007a exists because only *creation* is currently written to the instance action record.
Recording cross-project actions against the owning project — the point of decision 6 — is
vacuous unless the write path is extended to the operations the override newly covers.
That is real work implied by the decision, called out explicitly rather than left for
planning to discover.

### Cross-artifact analysis (2026-08-10)

Ran after task breakdown. **0 critical, 2 high, 5 medium, 4 low — all 11 resolved.**

| ID | Severity | Finding | Resolution |
|---|---|---|---|
| F1 | HIGH | `FR-022a`–`FR-022d` sat under *Identity filtering* while `FR-022` sat under *Validation* — the suffix implied a sub-requirement relationship that did not exist | Renumbered to **FR-024–FR-027** across 5 files |
| C1 | HIGH | FR-017 (pre-existing tokens keep working) had no task; contract S8 was orphaned | New task **T050** |
| F2 | MEDIUM | plan.md scheduled US6 at W5 while calling it the highest-severity item; spec says P1 | Wave table restructured — **US6 is now W3**, and wave order matches story priority |
| D1 | MEDIUM | Constitution's "Horizon dashboard tested" merge gate unaddressed, though US6 gates endpoints the Admin panel drives | New task **T086** |
| F3 | MEDIUM | research R4 wrongly classified `GetLimits` as a quota route using `QuotaTargetProject` | Corrected — it is caller-scoped by design and untouched |
| U1 | MEDIUM | SC-004's "across all five services" had no verification task | SC narrowed to what is actually verified + new task **T087** |
| T1 | MEDIUM | 13 FRs cited by no task, including FR-009 and FR-015 | FR IDs added to 14 high-value tasks; **every FR is now cited** |
| A1 | LOW | FR-018 absent from the sequence, reading as a lost requirement | Explanatory note added at FR-017 |
| C2 | LOW | Contract S9 had no task | Folded into S5 as **S5b** |
| D2 | LOW | FR-009 is a specific instance of FR-011 | Relationship stated in FR-011 |
| C3 | LOW | Constitution's "get approval on test strategy" step had no checkpoint | Added to the Phase 2 checkpoint |

Post-remediation sweep: **40/40 FRs cited**, 28/28 contract scenarios covered, zero dangling
requirement references, 89 tasks sequential with no gaps or duplicates, no `[P]` file collisions.

**Status**: Specification complete, planned, task-broken-down and analysed. Constitution PASS.
Ready to implement.
