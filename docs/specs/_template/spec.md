# Feature Specification: <TITLE>

**Feature Branch**: `<branch-name>`
**Created**: <YYYY-MM-DD>
**Status**: Draft
**Input**: <issue link / user description — the exact ask, unedited>

## Clarifications

<!-- Every question that changed the design, with the answer and the date.
     Format: "Q: ... → A: ..." grouped per clarification session/round. -->

### Session <YYYY-MM-DD>

- Q: ... → A: ...

## Problem

<!-- What is broken or missing today, concretely. Cite code (file:line) and
     upstream behavior. A reader who has not seen the issue must understand
     the defect from this section alone. -->

## Goals / Non-Goals

<!-- Goals: what this spec delivers, each verifiable.
     Non-Goals: what is explicitly out of scope — name adjacent work that
     someone might mistake for this spec's job. -->

**Goals**:

1. ...

**Non-Goals**:

1. ...

## User Scenarios & Testing *(mandatory)*

<!-- How a user (CLI, SDK, Terraform, Horizon, operator) experiences the fix.
     One user story per scenario, each with priority and the test that proves
     it. Include edge cases. -->

### User Story 1 - <title> (Priority: P1)

...

### Edge Cases

- ...

## Requirements *(mandatory)*

### Functional Requirements

<!-- Every requirement gets a stable FR id: FR-NNN, with sub-ids FR-NNNa when a
     requirement refines another. These ids are how code comments, commit
     messages, and contract tests cite this spec — do not renumber after review.
     Use RFC 2119 language (MUST/SHOULD/MAY) and reference the exact upstream
     OpenStack API behavior each requirement mirrors. -->

- **FR-001**: The system MUST ... (matches upstream <service> behavior for
  `<endpoint>` — see api-ref: <link>).
- **FR-001a**: ... MUST also ... .

### OpenStack API References

<!-- The upstream api-ref / client behavior each requirement is grounded in.
     If a requirement deliberately deviates from upstream, say so here and why. -->

- `<endpoint / field / error>` — <api-ref link or client source>, verified <date>.

### Key Entities

<!-- New or changed database entities/fields, if any. Full schema in data-model.md. -->

## Success Criteria *(mandatory)*

### Measurable Outcomes

<!-- SC-NNN: observable, measurable pass conditions — what a reviewer runs to
     declare the spec done. Map each back to the FRs and user stories. -->

- **SC-001**: ...

## Assumptions

- ...

## Dependencies

<!-- Other specs, ADRs, migrations, or issues this work depends on or unblocks. -->

## Out of Scope

- ...
