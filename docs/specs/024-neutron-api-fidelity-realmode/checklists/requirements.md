# Specification Quality Checklist: Neutron API Fidelity & Real-Mode Networking

**Purpose**: Validate specification completeness and quality before proceeding to planning
**Created**: 2026-08-11
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

### Iteration 1 (2026-08-11) — 2 items failed

- **No [NEEDS CLARIFICATION] markers remain** — FAILED with 2 markers, both scope decisions the
  source ticket explicitly leaves open:
  1. US3 / FR-012 — a network-creation request naming another project: honour it for administrators
     (upstream Neutron behaviour) or refuse it for everyone (the choice #549 made for compute server
     creation)? The ticket says "Either behaviour is defensible; silence is not."
  2. US6 — model external networks, or document the gap as intended and close the story without
     code? The ticket offers both outcomes verbatim ("external nets listed, **or** the gap
     documented as intended").
- **No implementation details** — FAILED. First draft named source files and line numbers in the
  user stories. Removed; that grounding now lives in the ticket's task checklist, where it belongs.

### Iteration 2 (2026-08-11) — resolved

- **US6 → model external networks.** Persisted attribute, real filter, administrator-gated write,
  gateway and floating-IP validation. The largest slice in the feature and the only one that adds a
  concept rather than correcting one; it stays at P3 so it can be dropped to a follow-up without
  disturbing the other five.

- **US3 → honour for administrators, refuse others as `400`.** The user asked what upstream does
  rather than picking from the options, so it was read out of upstream's implementation (its API
  reference documents none of this). The chain: the two spellings are normalised into each other and
  a disagreement between them is a `400`; a project differing from the authenticated one is a `400`
  unless the caller may set it, which administrators and service accounts may. Lightstack has no
  service-account concept, so administrator is the whole privileged set.

  The recommendation offered before that check ("refuse for everyone, `403`") was **wrong on both
  counts** and was not taken: it read #549's compute refusal as a project-wide rule. It is not —
  upstream compute has no project attribute on server create to honour, while upstream network does,
  so the two services legitimately differ. Recorded as A10/A11 in the spec so the asymmetry is not
  later "corrected" into a regression.

  The same source settled FR-033 for free: marking a network external is an administrator-only
  attribute upstream, so US6's role gate is fidelity rather than a local invention. It also surfaced
  an adjacent defect — the `shared` attribute is administrator-only upstream and Lightstack accepts it from
  anyone — which is listed in *Out of Scope* rather than folded in.

Re-validation: all items pass. Zero markers.

### Clarify session (2026-08-11) — 7 questions, all answered

Re-validated after each integration; all items still pass. The scan found seven material ambiguities,
five of which added requirements rather than merely sharpening them:

- **Error type vocabulary** → upstream's name where one exists, one documented generic for Lightstack-internal
  faults, enumerated in one place (FR-007, FR-007a). This is what keeps B10 from becoming 244 separate
  naming decisions.
- **External-network visibility** → a third disjunct alongside owner and shared, read-only
  (FR-033a/b). **This was a hole, not an ambiguity**: A7 asserted cross-project visibility and no
  requirement implemented it, so the tenant-facing gateway selection would have stayed empty and F11
  would have been fixed for administrators only.
- **Gatewayless subnet vs. metadata** → split the two DHCP options (FR-023a, FR-004). Also a genuine
  conflict rather than a gap: FR-023 as written would have broken FR-004, because the metadata route
  and the default route are guarded on the same value.
- **Unreadable subnet row** → log and keep serving (FR-024, FR-024a). FR-024 previously said "surfaced
  to the client **or** recorded in a log", which is not testable.
- **Subnet filter contract** → the seven that the network listing already supports; unknown parameters
  never narrow the result (FR-020, FR-020a).
- **Pre-existing router gateways** → validate new requests only, grandfather stored rows, log them
  (FR-035a/b). Without this, migration 066 defaulting to false would have silently withdrawn working
  egress on upgrade.
- **Absent named project** → bad request, same status as the other two rejections of that attribute
  (FR-016).

Also fixed while scanning: US3's acceptance scenarios were numbered 1,2,3,4,5,4,5.

Two candidates were considered and **not** asked, deliberately:

- *Whether `make test-all` is a realistic acceptance bar (SC-004).* Checked instead of asked — the
  target composes unit, chart, contract, integration, multi-tenant, errors, horizon-UI and performance
  suites, all of which run against the compose stack with no cluster required. The bar stands.
- *Whether the per-project network namespace becomes dead once ports move to the root namespace.*
  Planning-level: it is a consequence of A1, not a separate decision, and the namespace still has
  other callers (topology auto-allocation, the overlay coordinator, routers).

### Standing note on the "no APIs" item

This feature's deliverable *is* an API contract — Lightstack exists to be wire-compatible with OpenStack, so
the wire behaviour (status codes, response fields, query filters) is user-facing product surface, not
implementation detail. The spec names endpoint *behaviours* in that sense and deliberately names no
file, function, package, table, column or line. That reading matches the convention in this repo's
preceding specs (019, 020, 022, 023).

### Deferred to planning, not clarification

Recorded here so they are not re-litigated as spec gaps:

- **How** US1's namespace mismatch is fixed — assumption A1 states the direction (move the device to
  the bridge, not the bridge to the device) and gives the reason. Choosing the mechanism is planning
  work.
- **How** the error envelope conversion is structured across the network service's failing paths.
  FR-008 fixes the requirement (all of them, not a wrapper around the old body); the structure is
  planning work. Note the scale: ~244 error-return sites across 14 files in the network package.
- **Whether** US4's absent-gateway handling needs a stored-value change or only a reading change.
  A5 records that the two halves are coupled; which side moves is planning work.
- **Whether** US5's port bounds already distinguish unset from zero. A6 makes it explicit that if
  they do not, making them so is in scope.
