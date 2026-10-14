# Spec Template

Copy this directory to `docs/specs/NNN-short-slug/` (next free number — see the
[specs registry](../README.md#numbering-rules)) and fill in each file. Delete the
files you genuinely don't need, but keep the naming and the workflow.

## Workflow: specify → plan → tasks → implement → validate

1. **Specify** — write `spec.md`. Ground it in the codebase and in upstream OpenStack
   behavior (`research.md` if external investigation is needed). Resolve open
   questions in the spec's *Clarifications* section, not in the implementation.
   Every requirement gets an **FR-NNN id** — code comments and commit messages cite
   specs by these ids (e.g. `SPEC-021 FR-023c`), so they are mandatory.
2. **Plan** — write `plan.md`: the technical approach, phased sequencing, risks, and
   a constitution check against the project principles.
3. **Tasks** — write `tasks.md`: concrete `Tnnn` tasks, grouped in phases. Test tasks
   come first and must fail before implementation (test-first).
4. **Implement** — work the tasks in order, ticking them off in `tasks.md` as they
   complete. Cite FR ids in code comments where a requirement shapes the code.
5. **Validate** — run the contract tests for your `contracts/`, the unit tests, and
   (for real-mode features) the host passes recorded in `HOST_TESTING*.md` and
   `checklists/`. The PR that merges the implementation moves the spec to
   **implemented** in the registry.

## Files

| File | Purpose | Required |
|------|---------|----------|
| `spec.md` | The specification: problem, goals/non-goals, FR-NNN requirements, success criteria | yes |
| `contracts/` | The API contracts this spec must satisfy — Given/When/Then tables that become contract tests | yes |
| `plan.md` | Implementation plan: approach, phases, risks | yes |
| `tasks.md` | Task checklist, test tasks first | yes |
| `research.md` | External research / codebase findings | if research was needed |
| `data-model.md` | Schema and entity changes | if the schema changes |
| `checklists/` | Per-requirement validation checklists | recommended |
| `quickstart.md` | Operator quickstart for the feature | recommended |
| `HOST_TESTING.md` / `HOST_TESTING_REAL_MODE.md` | Host-validation runbooks (what cannot run in CI) | for real-mode features |
