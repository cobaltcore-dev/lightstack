# Specification Quality Checklist: Server security-group membership reaches the ports

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

- Iteration 1 (2026-11-17): two markers — the groups of an interface attached later (and add/remove on a server with no port), and Networking-API port updates in or out of scope.
- Iteration 2 (2026-11-17): both answered (port API in scope; attach and report as upstream) and encoded in Clarifications, User Stories 3 and 6, FR-003, FR-013, FR-014 and FR-017–FR-019. One requirement was inferred from the two answers rather than asked: `POST /v2.0/ports` with `security_groups: []` gives no group (FR-019); it is flagged as inferred in Clarifications. All items pass.
- Iteration 3 (2026-11-17, clarification session): seven questions, all answered. They added the group listing's `id` and `name` filters (FR-019a), atomic filter replacement (FR-007a), refusing and undoing a change whose filter cannot be rebuilt (FR-008), group resolution bound to its project for administrators too, as constitution register entry 5 (FR-004, FR-018, FR-021a), eBPF re-programming moved out of scope (FR-009 withdrawn), confirmation of the two inferred additions plus `null` = `[]` (FR-004, FR-017, FR-019), and the test strategy (FR-022). FR-022's constitution citation was corrected: Test-First Development is not Article III in `.specify/memory/constitution.md`. All items still pass.
- Iteration 4 (2026-11-17, analysis remediation): seventeen findings fixed.
  - **Constitution.** Two Article II departures were amended, not justified (constitution v1.5.0, research R19); register entry 5 was recorded in the same amendment. US5's add case moved into US1's tests-first batch.
  - **Spec.** FR-019a no longer mandates the silently ignored listing filters. The undo is compare-and-set on every path (FR-008). FR-007b bounds every firewall command and lock wait at one second. A stale edge-case sentence went. FR-016 and SC-010 were made exact.
  - **Plan and tasks.** Plan and tasks were brought in line, and the task list regenerated (43 tasks). All items still pass.
- Iteration 5 (2026-11-18, second analysis remediation): three findings fixed, with the real fix asked for.
  - **The constitution.** FR-008 exempted floating-IP association and rule changes, against v1.5.0's rule for requests. They are now in scope: FR-008 is generalised, FR-008a–c were added, FR-010 removes as well as re-asserts, and User Story 7 and SC-013–SC-015 were added. Making it work showed v1.5.0's first draft too narrow and silent in places, and it was refined in place (research R19). The design is research R20–R24.
  - **A test's wording.** The port API's lock-deadline test was rewritten the way the refresh test states it.
  - **The merge-time version task.** It now finds every v1.5.0 citation with a command instead of a list.
  - The task list was regenerated (54 tasks). All items still pass.
- Iteration 6 (2026-11-18, third analysis remediation): eleven findings fixed, again with the real fix asked for.
  - **The constitution's limits.** Deleting a floating IP stored its deletion after the node work, in a second transaction. It is now one transaction, and a failed withdrawal restores the floating IP with the port forwardings deleted with it, as the foreign keys would have left it (FR-008, research R20). The creates' checks moved into the transaction that inserts (R20, R21).
  - **Safety and regressions.** A filter now stays while any rule of the shape, marked or not, still reaches its fixed IP (FR-008a, FR-010, R23, R24). Reconciliation compares the floating addresses too, and re-adds a missing one (R24). A missing uplink in real mode is a failed change (R9).
  - **Tests.** The record read has a seam for partial reads (R24).
  - **Recorded departures.** The LS-SG chain removal that this feature moves stays a recorded departure (R22). A gap found on the way was recorded with it: a floating IP's deletion leaves the NAT of the port forwardings deleted with it.
  - **Version.** Another amendment numbered v1.5.0 reaches `main` first, so this one becomes v1.6.0 at the rebase (T057).
  - **Smaller fixes.** The command counts and the constitution's wording were corrected, and the runbook checks iptables' `comment` match.
  - Tasks were rewritten in place (54). All items still pass.
- Iteration 7 (2026-11-18, fourth analysis remediation): seven findings fixed.
  - **Locks.** The third analysis's locks used `FOR SHARE`, which the shared SQL subset does not have. Locks are now `FOR UPDATE`, taken only where a check reads state another request can change (research R20, R21).
  - **Port and server deletion.** A deleted port's floating IPs are disassociated in the deleting transaction and withdrawn before the answer (FR-008d, research R25, task T046). A failed withdrawal stays a recorded departure.
  - **Records.** A record is any floating IP's row, and the pass reads them all (FR-010, R24).
  - **Smaller fixes.** T016 takes the uplink from the service; T033 places its checks in the update's transaction; the foreign-key answer is limited to the first transaction; and "a rule of the shape" is defined once, in Key Entities.
  - 55 tasks. All items still pass.
- Iteration 8 (2026-11-18, fifth analysis remediation): four findings fixed.
  - **Server deletion.** R25 had taken `ReleaseInstancePorts`, which runs only when a boot fails, for server deletion. Nova deletes a server's ports in its own transaction. All three deleting paths now share one helper on the caller's transaction, and Nova converges through a new `NeutronService` method (research R25; tasks T041, T046, T047).
  - **Lock order.** The deleting transactions lock the port, or the instance and then its ports, before reading the floating IPs. Server deletion takes the instance first, which removes an existing inversion with membership changes and volume attach.
  - **Upgrade notes.** T049 now lists the port and server deletion change.
  - **Wording.** FR-008d no longer credits upstream with setting `DOWN` in the deletion; its L3 agent reports it afterwards.
  - 57 tasks. All items still pass.
- Iteration 9 (2026-11-18, sixth analysis remediation): four findings fixed.
  - **`forceDelete`.** It deleted only the instance row, so SC-014 did not hold for it. It now shares `DeleteServer`'s deletion, as upstream's `force_delete` does (FR-008e, research R25, T041, T047).
  - **Smaller fixes.** The instance locks' missing-row cases are stated (T018, T046, T047), and `ConvergeFloatingIPs` is placed in `floatingip.go`. R25 now calls volume attach against server deletion a race, not a deadlock.
  - 57 tasks. All items still pass.
- "No implementation details": the spec names OpenStack API routes, request fields and status codes. They are the product's public contract — the WHAT of a drop-in replacement — not implementation. File and line references sit only in the Context section, as the project's specs do (SPEC-031), to anchor the defect; no requirement names a function, table or package.
- "Technology-agnostic success criteria": SC-005 and SC-006 name Terraform and Horizon. They are the clients whose behaviour defines success for a drop-in replacement, not implementation choices. SC-008 names the two database backends, which are a deployment option users choose.
- "Written for non-technical stakeholders": the audience is OpenStack operators and tenants; port, floating IP and security group are their vocabulary.
- Items marked incomplete require spec updates before `/cl.clarify` or `/cl.plan`.
