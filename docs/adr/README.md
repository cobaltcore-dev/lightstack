# Architecture Decision Records

This directory holds Lightstack's Architecture Decision Records (ADRs) — short,
immutable documents that capture **why** a significant technical decision was made,
not just what it is.

An ADR is required when a decision meets the criteria in
[ADR-000](ADR-000-when-to-write-an-adr.md) (architectural/cross-cutting, hard to
reverse, a security/compliance posture change, or a dependency change). Decisions
that are local to one spec stay in that spec's `spec.md`.

## Format

ADRs are numbered `ADR-NNN` (next free number: **003**), one file each:
`ADR-NNN-short-title.md`, created from [`_TEMPLATE.md`](_TEMPLATE.md). Once an ADR
is Accepted its Decision section does not get rewritten — later changes are
recorded as amendments inside the document, or as a new ADR that supersedes it.

**Status** values:

- **Proposed** — written, not yet ratified; open for review.
- **Accepted** — ratified; the recorded decision is the project's direction.
- **Superseded** — replaced by a later ADR; the header names the successor.

## Index

| ADR | Title | Status | Date | One line |
|-----|-------|--------|------|----------|
| [ADR-000](ADR-000-when-to-write-an-adr.md) | When to write an ADR | Accepted | 2026-10-14 | The process ADR: what makes a decision ADR-worthy |
| [ADR-001](ADR-001-database-dependency-injection.md) | Database Dependency Injection | Accepted | 2026-04-20 | `*pgxpool.Pool` through service constructors instead of a global — implemented by [SPEC-027](../specs/027-database-pool-injection/spec.md) |
| [ADR-002](ADR-002-rate-limiting-architecture.md) | Rate Limiting Architecture | Accepted | 2026-04-20 | Two-tier in-process rate limiting (`golang.org/x/time/rate`, IP + project) — not yet implemented |

## Relationship to specs

ADRs record the *decision*; the spec that implements it records the *how* and the
task breakdown. The link is bidirectional: the ADR's **Spec** header points at the
implementing spec (or specs), and the spec's Input/Context cites the ADR. Examples:
ADR-001 ↔ SPEC-006 / SPEC-027.

See also the [specs registry](../specs/README.md) and the
[contract index](../specs/CONTRACT-INDEX.md).
