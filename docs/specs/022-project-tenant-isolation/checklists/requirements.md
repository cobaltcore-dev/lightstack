# Specification Quality Checklist: Project/Tenant Lifecycle & Isolation Fixes

**Purpose**: Validate specification completeness and quality before proceeding to planning
**Created**: 2026-08-04
**Feature**: [spec.md](../spec.md) — GitHub issue [cloud-storage#545](https://github.com/cobaltcore-dev/cloud-storage/issues/545)

## Content Quality

- [x] No implementation details (languages, frameworks, APIs) — *see Note 1*
- [x] Focused on user value and business needs
- [x] Written for non-technical stakeholders
- [x] All mandatory sections completed

## Requirement Completeness

- [x] No [NEEDS CLARIFICATION] markers remain — both resolved 2026-08-04, zero remain
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
- [x] No implementation details leak into specification — *see Note 1*

## Validation Log

**Iteration 1** (2026-08-04) — issues found and fixed:

| Item | Issue | Fix |
|---|---|---|
| Success criteria technology-agnostic | SC-005 said *"neither returns 'method not allowed'"* — a protocol status phrase | Reworded to *"neither rejects the request as an unsupported operation"* |
| Written for non-technical stakeholders | A5 used *"predicate"* | Reworded to *"the rule itself (whether 'shared' counts as entitlement)"* |

**Iteration 2** (2026-08-04) — both clarifications answered by the user; markers replaced with
concrete requirements and the rejected options preserved as the decision record:

| Question | Decision | Encoded as |
|---|---|---|
| Q1 — shared networks and the refusal code | Owner or administrator only; shared grants nothing. `404` for a network the caller cannot see, `403` for a shared one they can see but do not own | FR-010, FR-010a, SC-004a, US2 scenario 6, A5 |
| Q2 — width of the project-scoped URL repair | The ticketed volume list **plus** the shadowed sub-resources; the merely-absent routes become a recorded gap. *(Counted as four/14 here; "corrected" to three/15 in Iteration 5; back to **four/14** after the o3k#82 review found `os-volume-transfer/detail` shadowed. The decision never changed, only the census — and the original tally was right by coincidence, having counted `backups/detail` (not shadowed) and missed `os-volume-transfer/detail` (shadowed))* | FR-016, FR-016a, SC-005a, US3 scenario 4, A10 |

**Iteration 3** (2026-08-04) — re-validated: **all items pass**. 25 functional requirements, 10
success criteria, 4 prioritised user stories, 8 edge cases, 10 assumptions, zero open markers.

**Iteration 4** (2026-08-04, clarification pass) — a taxonomy scan found four further material
ambiguities, all asked and answered. Two of them were gaps the specification pass had missed rather
than open choices:

| Question | Decision | Encoded as |
|---|---|---|
| Does the `default` alias need to resolve on the **read** path, not just on creation? | Yes — retrieval by alias, plus a working `name` filter on the domain list (it ignores every filter today). Names stay case-sensitive | FR-001a, FR-008, SC-001a, US1 scenario 6, A2, A11, two edge cases, Out of Scope |
| FR-004 (duplicate name → conflict) + FR-017 (setup exits early) would make one crashed run block every later run | Teardown on exit via a trap registered before the first fixture is created | FR-017a, SC-006a, US4 scenario 5 |
| Which client error does a **malformed** `network_id` get? | Bad request naming the field — it is a body attribute, so #515's not-found rule for path ids does not reach it; well-formed-but-unknown stays not found | FR-011, FR-011a, edge case |
| Is a refused cross-tenant write auditable? | Yes — one warning naming caller, target network and owner; nothing on the success path | FR-011b, SC-004b |

**Iteration 5** (2026-08-04, planning pass) — one factual correction. FR-016 claimed **four** shadowed
sub-resources on the project-scoped URL form; verifying against the route group rather than the
flat/project-scoped difference showed only **three** qualify. `backups/detail` has no by-identifier
sibling registered in that group — no backup route is registered there at all — so it answers an honest
"not found" and belongs with the merely-absent routes, which rise from 14 to 15. Corrected in FR-016,
FR-016a, SC-005a, US3 scenario 4, A10, the shadowing edge case, and the Q2 decision record.

**Iteration 6** (2026-08-04, cross-artifact analysis pass) — 12 findings across spec/plan/tasks, all
addressed. No constitution violations, no CRITICAL findings. The two that changed what gets built:

| ID | Sev | Finding | Fix |
|---|---|---|---|
| G1 | HIGH | FR-017 names "a role assignment" among the things setup must guard, but the two `PUT …/roles/…` calls in `test/multi_tenant_test.sh:81-86` pipe to `/dev/null` with no status check — `MEMBER_ROLE_ID` is the role *lookup*. A silently failed assignment yields role-less tokens, every isolation check then refuses for the wrong reason, and the suite reports passes for isolation it never exercised | New task T029a: capture the status, require `204` (`handlers.go:796`) |
| I2 | MED | plan.md called the new test a "parity guard" — parity is Q2's **rejected** option C, so the wording invited an implementer to build the 19-route sweep the decision refused | Renamed to "no-shadow guard" in plan.md and tasks.md, with the rejection noted inline |
| I1 | MED | plan.md's sequence table had no wave for the RED-evidence capture that tasks.md calls blocking and irreversible | Added wave W0 |
| I3 | MED | US4 acceptance scenario 3 required checks that tasks correctly place in US2/US3 — a check asserting a refusal cannot precede the refusal | Scenario 3 reworded to arrive *with* the fixes it guards; US4's Independent Test is unaffected |
| G2 | MED | SC-002 claimed "zero generic server faults for **any** well-formed request" with no task substantiating a universal claim | SC-002 narrowed to an enumerated payload set; new task T012a adds a bounded table-driven no-`5xx` test over exactly that set |
| U1 | MED | T031 said "or stub the create call to fail" — no such seam exists in a bash suite, and after US1 lands creation works | Concrete mechanism specified (invalid admin token, preferred over a closed socket because it exercises the *new* guards) |
| I4, I5, D1, U2, A1, G3 | LOW | `EVIDENCE.md` missing from plan's doc tree; W2 mislabelled "T1" though T1 is already fixed; FR-009 subsumed by FR-010; FR-013's "same response shape" unasserted; SC-008's "documented workflow" unbounded; 9 FRs uncited in tasks | All corrected; FR-009 is now purely the ordering requirement, T034a asserts the response shape, SC-008 enumerates its suites, and FR citation coverage is 29/29 |

**Discovery during the pass**: `test/horizon_compat_test.sh` **already fails** on B2 — it calls
`GET /v3/${PROJECT_ID}/volumes` (line 131, the `405`) and `GET /v3/${PROJECT_ID}/volumes/detail`
(line 210, the shadowed route). A pre-existing failing suite this feature repairs is free RED→GREEN
evidence, so it is now recorded in SC-008, tasks T007a/T054a/T057 and quickstart §3 and §6.

Final counts: **29** functional requirements, **13** success criteria, **11** assumptions, 4 user
stories, 11 edge cases, **67** tasks, zero open markers. Artifacts are consistent and ready for
implementation.

## Notes

- **Note 1 — response classes in the Context section.** The Context table quotes the defect report's
  observed status codes (`500`, `405`, `400`). This is deliberate and is not an implementation-detail
  leak: O3K's product *is* an OpenStack-compatible API, so the response class a client receives is
  the user-visible outcome, not an internal choice. All **Functional Requirements** are nonetheless
  worded behaviourally — *"client error naming the domain field"*, *"conflict"*, *"not found"* —
  so none of them constrains an implementation.
- **Note 2 — both clarifications are resolved** (see Iteration 2). They were scope/security decisions
  rather than gaps: the ticket itself hedged on Q1 (*"Expected 404/403"*) and the in-repo port-create
  precedent disagreed with upstream policy, while on Q2 the ticket said one route and the code comment
  beside the route group argued for parity.
- **Note 3 — one requirement asks for a comment in the code, unusually for a spec.** FR-010 makes
  subnet creation refuse shared networks while its neighbour, port creation, admits them. Two adjacent
  handlers that look alike but decide differently is precisely the shape that gets "simplified" back
  into a defect, so A5 requires the divergence be stated where a future reader will hit it.
- All checklist items pass. The spec is ready for planning; no further spec updates are required
  first.
