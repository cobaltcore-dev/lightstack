# ADR-000: When to Write an ADR

**Status**: Accepted
**Date**: 2026-10-14
**Deciders**: Lightstack Core Team

## Context

Lightstack records feature work in specs (`docs/specs/`, see the
[registry](../specs/README.md)) and cross-cutting decisions in ADRs. The boundary
between the two must be explicit, or every design choice inside a spec becomes an
unversioned argument in a tasks file, and the decisions that shaped the whole
binary are lost.

## Decision

Write an ADR when a decision is **any** of:

1. **Architectural or cross-cutting** — it affects multiple services or the shape
   of the binary itself (startup, wiring, process model, deployment topology).
2. **Hard to reverse** — changing it later costs a migration, a large refactor, or
   an API break.
3. **A security or compliance posture change** — it alters how the system defends
   itself or what it promises to clients and operators.
4. **Introduces or removes a dependency** — anything that changes what ships in
   the single binary or what it requires at runtime.

Decisions that are local to one spec — naming, internal structure, which handler
does what — stay in the spec's `spec.md` and its Clarifications. A spec whose
scope *contains* an ADR-worthy decision still needs the ADR; the spec implements
the decision, it does not own it (see ADR-001 ↔ SPEC-027).

## Worked examples

- **ADR-001 (database dependency injection)**: replacing the process-wide
  `database.DB` global with constructor-injected pools touched 661 references
  across 53 files and every service — cross-cutting and costly to reverse (criteria
  1 and 2). It surfaced in a code review (finding H-3), not inside any spec, and
  was later implemented by a dedicated spec ([SPEC-027](../specs/027-database-pool-injection/spec.md)).
- **ADR-002 (rate limiting architecture)**: choosing two-tier, in-process,
  standard-library rate limiting changed the system's abuse-resistance posture
  (criterion 3) and ruled out whole design families (Redis-backed, per-endpoint)
  for the single-binary architecture (criterion 1).

## Consequences

- Specs can evolve task-by-task while the decisions beneath them stay citable and
  stable; code comments and specs link decisions via `ADR-NNN`.
- If a later change reverses an ADR, write a new ADR that supersedes it — never
  edit the old decision in place.
