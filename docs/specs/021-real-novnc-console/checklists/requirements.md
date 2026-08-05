# Specification Quality Checklist: Horizon compat — a real console / noVNC (F1)

**Purpose**: Validate specification completeness and quality before proceeding to planning
**Created**: 2026-07-30
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

- **Validation run**: 2026-07-30, one iteration, all items pass.
- **On "no implementation details"**: two categories of concrete naming are deliberate and were judged in-bounds, matching the precedent set by SPEC-018/019/020:
  - *Compatibility contracts are the requirement.* Lightstack's product promise is drop-in OpenStack compatibility, so naming the console-auth-token API (FR-015), the remote-console and legacy VNC-console request paths (FR-005), and `openstack console url show` (US1 scenario 3) states **what** must be compatible, not how to build it.
  - *Defect grounding.* The Context section cites the four confirmed code-level causes (fabricated address, meaningless token, guessed port, absent proxy) and the documented-but-ignored `console_proxy_base_url` setting. These are the evidence that the finding is real and correctly scoped; the requirements themselves stay behavioral.
- **Zero `[NEEDS CLARIFICATION]` markers by design.** Four genuine forks existed but all concerned delivery mechanism rather than user-visible outcome, so they were recorded under *Assumptions → Deferred design decisions* rather than blocking the spec. **All four were resolved in the 2026-07-30 clarification session**, which asked 12 questions in total — the four forks plus eight ambiguities the coverage scan surfaced (guest exposure and multi-node scope, chart ownership, endpoint readback timing, lifecycle token invalidation, admin cross-project access, config surface shape, same-instance concurrency, and the dead console columns). Two answers changed scope: multi-node console access is out for v1, and the dashboard chart is relocated to the product chart directory.
- **Two defects were found while clarifying, not while implementing**, and are now recorded in the spec: a token whose frozen `host:port` could resolve to a *different* instance after console-port reuse (closed by FR-014's widened invalidation), and FR-018's original same-instance "isolation" wording, which was incoherent (corrected by FR-018a).
- **FR-007's "~1 second"** inherits the project-wide fail-fast budget for external dependencies and is pinned to a hard, measurable bound by SC-010 (under 2 seconds observed).
- **Security requirements are not optional extras.** FR-011, FR-012, FR-013 and FR-017 exist because a working relay without them would be a worse outcome than today's broken console — an unauthenticated path into every guest. They ship with US1.
