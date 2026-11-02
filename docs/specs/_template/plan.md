# Implementation Plan: <TITLE>

**Branch**: `<branch>` | **Date**: <YYYY-MM-DD> | **Spec**: [spec.md](./spec.md)

## Summary

<!-- Two or three sentences: what is built and the technical approach.
     Name the dominant finding from research (the shape that already exists
     in the codebase vs. what must be created). -->

## Technical Context

<!-- Language/versions, primary dependencies, storage, testing strategy,
     target platform, constraints (API compatibility, sync operations,
     modes), scale/scope (packages touched, migrations, new deps). -->

**Project Type**: Single Go binary — all services constructed in
`cmd/lightstack/main.go`; no microservices, no new services unless the spec says so.

## Constitution Check

<!-- Evaluate the plan against the project principles (OpenStack API compat,
     synchronous operations, fail-fast, multi-mode, project isolation, ...).
     One row per principle: PASS / PASS (with a fix) / FAIL + notes. -->

## Phases / Task Sequencing

<!-- Phase 1 (setup), Phase 2 (foundational blockers), then one phase per user
     story in priority order. Each phase: purpose, what unblocks, independent
     test. Task ids live in tasks.md. -->

## Risks & Mitigations

| Risk | Mitigation |
|------|------------|
| ... | ... |
