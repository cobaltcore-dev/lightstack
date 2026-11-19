# Specification Quality Checklist: Squash the migrations, restart at 001, one migration per release

**Purpose**: Validate specification completeness and quality before proceeding to planning
**Created**: 2026-11-17
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

- Validation run 1 (2026-11-17). Three [NEEDS CLARIFICATION] markers remained: FR-001 (surviving
  databases and the squash point), FR-014 (fragments or one shared file) and FR-020 (whether
  recreating databases is acceptable). These are the ticket's three open questions.
- Validation run 2 (2026-11-17). All three are answered and recorded under Clarifications:
  - nothing deployed has to survive, and the squash point is the newest migration on `main`;
  - fragments, ordered by their UTC creation time (a follow-up on the order);
  - recreating dev, CI and staging databases is acceptable.

  All items pass.
- Clarify session (2026-11-17): seven more answers integrated.
  - Restart at `001` kept.
  - Re-stamp checks the exact schema.
  - Numbered migrations are immutable once on `main`.
  - A tag-push check enforces "no fragments in a release".
  - The squash lands before v0.7.0.
  - The upgrade script refuses a rollback across the squash.
  - Every `v*` tag freezes, and releases come from `main` only.

  The terms are normalised: "frozen" means a numbered migration on `main`. All items still pass.
- Planning (2026-11-17): one more answer integrated.
  - SQLite's `079`/`080` NOT NULL triggers become real NOT NULL in the baseline, a narrow FR-005 exception.
  - Re-stamp is PostgreSQL-only (FR-009, FR-010, User Story 2 scenario 8).
  - The replay hazard is corrected: an old binary duplicates two seed rows and goes dirty at `003`, not `074`.

  All items still pass.
- Follow-up (2026-11-18): `scripts/upgrade-lightstack.sh` is deleted, which supersedes the
  rollback-guard answer above. FR-013, SC-003, the Edge Cases and Out of Scope are amended.

  All items still pass.
- Follow-up (2026-11-19): re-stamp is deleted, which supersedes the re-stamp answers above:
  v0.7.0 is a fresh install. FR-009, FR-010 and SC-004 are removed and keep their IDs. FR-011,
  FR-013, User Story 2, the Edge Cases, Key Entities, Assumptions, Out of Scope and Dependencies are
  amended.

  All items still pass.
- "Implementation details": the feature is migration infrastructure, so PostgreSQL, SQLite,
  golang-migrate, migration versions and the `lightstack-migrate` command are the problem domain
  and the operator's interface. They are not design choices. No source paths, functions or code
  structure appear.
- "Non-technical stakeholders": the stakeholders are maintainers and operators. The spec states
  what they see and do, not how the code does it.
- Requirement to acceptance mapping: FR-001–008 → US1; FR-011–013 → US2 (FR-009 and FR-010 removed); FR-014–018 → US3;
  FR-019–022 → US4; FR-023–027 → US5; FR-028 is checked in review; FR-029 → SC-007.
- Items marked incomplete require spec updates before `/cl.clarify` or `/cl.plan`.
