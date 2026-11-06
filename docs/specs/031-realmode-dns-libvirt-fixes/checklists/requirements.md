# Specification Quality Checklist: Real mode — cluster DNS under host networking, the libvirt socket, and a capabilities read that is never retried

**Purpose**: Validate specification completeness and quality before proceeding to planning
**Created**: 2026-11-06
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

- Iteration 1 (2026-11-06): two markers open — FR-017 (whether the preparation state reaches readiness or the hypervisor API, against SPEC-013 FR-001) and User Story 6 (whether the fake-`ACTIVE` server on an unreachable libvirt is in scope).
- Iteration 2 (2026-11-06): both answered and recorded under Clarifications. Readiness waits for the one-time preparation in real mode (FR-017, FR-017a, FR-018a, FR-023, FR-024; User Story 3 scenarios 6–8; new edge cases on a preparation that never succeeds, single node, multi-node, Compose/bare metal; SC-004a). User Story 6 is in scope as written (FR-019, FR-020). Zero markers remain; every FR/SC referenced is defined.
- Iteration 3 (2026-11-06, clarify): five more answers, all integrated — a server action without a libvirt connection answers as a lost connection does (FR-019); ownership of the per-instance directory is ensured before every guest start, self-heal included (FR-014, new Key Entity "Guest start"); host validation forces the cold and unreachable paths and adds a monolithic step (FR-010, FR-021 a–e); `lightstack cleanup` makes one attempt and fails closed (FR-020a); the chart docs and `docs/UPGRADING.md` cover the DNS change and the retired workarounds (FR-003a, FR-024). Also fixed: FR-016's "promptly" is now bounded, SC-001 and SC-003 no longer over-claim, "schema" in Assumptions now says database schema. Zero markers; every FR/SC referenced is defined. All items still pass.
- Iteration 4 (2026-11-06, analysis remediation): all 17 findings fixed across spec, plan, research, data model, contracts, quickstart and tasks. The spec gained a third clarification session (analysis): `lightstack cleanup`'s attempt is bounded at 1 s (FR-020a); a guest start takes the result of an attempt already running (FR-013, FR-016); every real-mode create that ends `ERROR` records a fault (FR-019, SC-007; the out-of-scope line removed). FR-005 now holds the `libvirtSocketPath` documentation sentence FR-022 used to repeat; FR-009 names every entry point. Zero markers; every FR/SC referenced is defined. All items still pass.
- Iteration 5 (2026-11-06, second analysis): 6 findings fixed. FR-019 and US6 scenario 2 now say each handler's own lost-connection message (a fourth analysis-session entry records it; the clarify record points to it); the contract and research list the per-step fault messages; T015 boots from an existing volume under a temporary state path, adds `TestCreateFault`, and seeds the `os-stop` target `ACTIVE`; T024 checks the `go mod tidy` diff; the plan's dependency row is exact. Zero markers; every FR/SC referenced is defined. All items still pass.
- Implementation detail, read against this project's convention: chart values (`realMode.*`, `dnsPolicy`, `probes.*`), config keys (`nova.libvirt_uri`) and socket paths are the operator's interface, so they stay in requirements. Code locations appear only in Context, as the evidence for each defect. FR-014's ordering clause and FR-017's "from the recorded state, never from a libvirt call" are behaviour (no start against a root-owned directory; readiness that cannot flap with libvirt), not design choices.
- Audience: operators of a single-binary OpenStack. "Non-technical" is read as "no code knowledge needed to verify a requirement": every FR is checkable by rendering the chart, running the binary, asking `/readyz`, or using the OpenStack CLI.
- Success criteria name `ACTIVE`, Ready, a chart install and libvirt: domain terms an operator verifies directly, not technology choices.
