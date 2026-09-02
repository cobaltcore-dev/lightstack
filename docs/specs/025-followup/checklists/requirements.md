# Specification Quality Checklist: SPEC-025 Follow-ups

**Purpose**: Validate specification completeness and quality before proceeding to planning
**Created**: 2026-09-02
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

**All items pass.** The clarify session of 2026-09-02 resolved every marker: 11 questions asked, 11
answered, 0 outstanding. Every user story is now plan-ready.

**What the clarify pass changed beyond the three markers.** Three of the eleven questions were the
markers this spec was written with. The other eight came out of the ambiguity scan, and two of them
were **contradictions this spec had created itself**:

- FR-004 instructed that an unhonourable server-list filter be answered "the way upstream answers
  it" — but the case that motivated it, `host`, is one upstream *honours*, so the instruction
  resolved to "do the impossible". Settled as an explicit refusal, which is a deliberate divergence
  from upstream rather than a reading of it.
- FR-041 instructed that an out-of-range microversion be answered as upstream answers it, which
  would have reversed a decision SPEC-025 took deliberately and documented as A-006 in the code —
  and would have made requests that succeed today fail. Settled by separating out-of-range (refuse)
  from unparseable (tolerate), which is how upstream itself treats them.

Two more widened the work rather than bounding it, and both came from following an answer into the
code rather than from the spec text:

- Enforcing all six block-storage resources surfaced that FR-019's "every path that provisions a
  volume" reaches **five** routes, not the two this spec identified — a backup restored into a new
  volume and a managed-volume import each insert one, and a transfer accept adds one without
  provisioning anything at all. FR-019's wording changed from "provisions" to "adds a volume to a
  project" as a result.
- Counting snapshot size toward the gigabytes allowance means correcting the shared usage counter,
  so a **number users can see** changes — reported usage rises for any project holding snapshots.
  That is now a release-note item rather than a silent consequence.

One claim in this spec was withdrawn: the catalog's cache is no longer out of scope. A per-request
host and a cache key that ignores the host cannot both be correct, and because the request host is
client-controlled, a shared key would let one caller choose the endpoints another is handed.

**On the two items that read as borderline passes:**

- *No implementation details / technology-agnostic success criteria.* The spec names client commands
  (`openstack image set --min-disk`), query parameters (`?name=`) and response shapes. For this
  product that is not leakage: Lightstack's whole purpose is that a stock OpenStack client works unchanged,
  so the client's observable behaviour **is** the user-facing requirement, and a requirement that
  avoided naming it would not be testable. Same convention as the sibling specs on this branch. No
  Go types, packages, SQL or file paths appear in the requirements.
- *Requirements are testable and unambiguous.* Two requirements still defer a specific value to
  planning: the exact honoured filter set and `name`'s matching semantics (FR-003), and the image
  patch media type's operation set (FR-010 – FR-013). Both are **upstream** behaviours with a named
  resolution path, not open questions — the same treatment SPEC-024 gave its upstream decisions. The
  two that were *not* resolvable that way (FR-004, FR-041) were pulled into the clarify session
  instead of left to planning, which is what caught them as contradictions.

**On the testability of the feature as a whole.** Every defect here is silent, so the checklist item
that matters most is not in the template: *can the test fail against today's code?* FR-009, FR-017,
FR-029 and FR-048 each state the negative assertion explicitly, and SC-013 requires the new tests be
demonstrated RED before the change rather than asserted to be. This is recorded here because it is
the most likely way this feature gets delivered wrong — nine of SPEC-025's verification steps had
checks that could not fail, and they were only caught during the host pass.
