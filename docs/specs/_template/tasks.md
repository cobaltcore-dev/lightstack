# Tasks: <TITLE>

**Input**: Design documents from `docs/specs/NNN-short-slug/`
**Prerequisites**: spec.md, plan.md, research.md, contracts/

**Tests**: REQUIRED. Write the test tasks first and confirm they FAIL before
implementing (test-first). Contract tests (gophercloud / OpenStack CLI) run in CI;
real-mode host tests are authored now and executed on the host per the
`HOST_TESTING*.md` runbooks.

## Format: `[ID] [P?] [Story] Description with file path`

- **[P]**: parallelizable (different files, no dependency on an incomplete task)
- **[Story]**: US1, US2, ... mapping to the user stories in spec.md

---

## Phase 1: Setup (Shared Infrastructure)

- [ ] T001 [P] ... (`path/to/file`)

## Phase 2: Foundational (Blocking Prerequisites)

- [ ] T002 ... (`path/to/file`)

## Phase 3: User Story 1 — <title> 🎯 MVP

### Tests (write first, MUST fail) ⚠️

- [ ] T003 [P] [US1] Contract test `test/contract/...` covering C-... from `contracts/`
- [ ] T004 [US1] Host test `test/...` per `HOST_TESTING.md`

### Implementation

- [ ] T005 [US1] ... (`path/to/file`) *(cite the FR id, e.g. FR-001, in the task)*

<!-- Tick tasks off with [X] and append a short parenthetical of what was done
     and how it was verified. Do not delete open items — leave host-gated
     validation tasks open until the host pass runs. -->
