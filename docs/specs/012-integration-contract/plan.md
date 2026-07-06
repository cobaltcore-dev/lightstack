# Implementation Plan: Integration Contract (Values Schema + Status Conventions)

**Branch**: `012-integration-contract` | **Date**: 2026-06-29 | **Spec**: [spec.md](./spec.md)
**Input**: Feature specification from `docs/specs/012-integration-contract/spec.md`

## Summary

Formalize the Helm interface **sci-in-a-box** uses to drive o3k's lifecycle:

1. **Input contract** — complete `charts/o3k/values.schema.json` so it documents *and* structurally
   validates every input, rejecting invalid configuration before execution.
2. **Output/status contract** — define a deterministic, documented recipe mapping `helm status` +
   pod readiness → Provisioning/Ready/Failed (Failed = timeout **or** terminal failure), and
   **identity-anchored** endpoint resolution (guarantee the Keystone `/v3` endpoint; the other five
   services come from the OpenStack service catalog).
3. **Publication** — a single `charts/o3k/CONTRACT.md` tying inputs to outputs, linked from the
   README and shipped in the chart package.

This is **finish-and-formalize** over the existing chart (SPEC-007/008/009/010/011): no Go/runtime
changes, no new dependencies, no schema/DB migration. The cross-field fail-fast guards already exist
as render-time template guards with helm-unittest coverage; the new work is schema completion, the
status/endpoint documentation, the published contract, and rejection/drift tests. Test-first:
schema-rejection tests go RED before the schema is completed.

## Technical Context

**Language/Version**: Helm 3.12+ chart (Go `text/template`); JSON Schema **draft-07**; Markdown docs. No Go code changes.
**Primary Dependencies**: Helm's built-in `values.schema.json` validation + release-status/pod-readiness reporting; the **helm-unittest** plugin (existing harness). No new Go modules, no chart dependencies.
**Storage**: N/A — the external PostgreSQL is the product's and is untouched; this feature adds no schema or migration.
**Testing**: helm-unittest (`charts/o3k/tests/*_test.yaml`) + `charts/o3k/ci/run-tests.sh` (helm lint / template permutations / unittest); optional kind/k3s smoke (`test/helm_*`).
**Target Platform**: Kubernetes ≥1.25 / k3s; consumed by the sci-in-a-box orchestrator via Helm.
**Project Type**: Helm chart + documentation — single deliverable area (`charts/o3k/` + this spec dir).
**Performance Goals**: Validation is install-time (negligible). Status detection is bounded by the orchestrator's `helm --wait --timeout` (operator-set). No runtime performance impact.
**Constraints**: Contract-only — MUST NOT change runtime behavior. `additionalProperties: false` requires the schema to cover every key the chart reads, or installs break → guarded by a drift/coverage test. Must render in both **stub** and **real** modes. Multi-node cross-node behavior stays deferred (#52), documented as a caveat.
**Scale/Scope**: One chart; ~22 top-level value groups; 6 services; 2 topologies (Deployment / DaemonSet); 1 published contract document.

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-checked after Phase 1 design (unchanged — PASS).*

| Principle | Assessment |
|-----------|-----------|
| I. OpenStack Flamingo API Compatibility (NON-NEGOTIABLE) | **PASS / reinforces** — endpoint contract uses the standard Keystone-auth + service-catalog discovery flow (no bespoke endpoint scheme); the contract cites the Flamingo API versions (Identity v3, Compute v2.1, Network v2.0, Block v3, Image v2, Placement v1.0). |
| II. Synchronous Operations | **PASS** — no async/state-machine added; the status recipe observes Helm's synchronous `--wait`. |
| III. Fail-Fast Design | **PASS / reinforces** — the input contract *is* fail-fast (reject invalid values before workloads start). |
| IV. Multi-Mode Architecture | **PASS** — contract is mode-agnostic; the schema validates mode inputs; docs/tests cover stub (default) and real. |
| V. Project Isolation | **N/A** — no resource handling. |
| VI. Single Binary Deployment | **PASS** — documents the single front-door Service (6 API ports; metadata node-local). |
| Forbidden patterns (queues/async/state-machines/service-discovery) | **PASS** — status derivation is pure observation of Helm + k8s state; no new infra. "No container orchestration required" stays additive (the binary still runs standalone). |
| Test-First | **PASS (planned)** — schema-rejection tests written RED before schema completion, mirroring `tests/gates_test.yaml`. |

No violations → **Complexity Tracking not required.**

## Project Structure

### Documentation (this feature)

```text
docs/specs/012-integration-contract/
├── plan.md                      # this file
├── research.md                  # Phase 0 — decisions (R1..R8)
├── data-model.md                # Phase 1 — input inventory, status state model, endpoint model
├── quickstart.md                # Phase 1 — sci-in-a-box walkthrough
├── contracts/
│   ├── input-contract.md        # the values-schema contract (what must validate)
│   └── status-contract.md       # the status + endpoint conventions (the recipe)
├── checklists/requirements.md   # from /cl.specify (green)
└── tasks.md                     # later — /cl.tasks
```

### Source Code (repository root)

```text
charts/o3k/
├── values.schema.json     # COMPLETE: description on every property; model database, secrets,
│                          #   cinder/glance (storageMode enums), ingress, multiNode, realMode,
│                          #   cache, endpointHost, podSecurity, service, logging; image.tag = the
│                          #   "version" input (typed string, no enum); additionalProperties:false
│                          #   on the top level + typed objects, free-form maps left open
├── values.yaml            # unchanged — the source-of-truth key set the schema must cover
├── CONTRACT.md            # NEW — the published contract (inputs↔outputs), SPDX header; ships
├── README.md              # add a link to CONTRACT.md
├── templates/NOTES.txt    # mark the six in-cluster ports non-authoritative; point at Keystone + catalog
├── templates/_helpers.tpl # only if a NEW render-time guard is needed (existing guards already
│                          #   cover required secrets/DB + multi-node-real backend rejection)
├── tests/                 # helm-unittest — NEW schema-rejection + key-coverage (drift) specs
└── ci/
    ├── run-tests.sh        # add bash schema-rejection assertions (helm template with bad fixtures must fail)
    └── invalid-values/*.yaml  # NEW invalid fixtures (helmignored — test-only)
```

No Go source changes; no migrations; no new dependencies.

**Structure Decision**: Single deliverable area — the existing `charts/o3k/` chart plus this feature's
`docs/specs/012-integration-contract/`. The published contract is the chart's `CONTRACT.md`; the
schema completion and tests live in the chart; the planning/definition artifacts live under the spec
directory. The two `contracts/*.md` files are the *definition* of the interface; `CONTRACT.md` is the
consumer-facing rendering shipped with the chart.

## Phase 2 note

`/cl.tasks` will turn `contracts/` + `data-model.md` into a dependency-ordered `tasks.md`. Expected
task spine: (1) RED rejection/coverage tests → (2) complete `values.schema.json` GREEN → (3) status +
endpoint docs → (4) `CONTRACT.md` + README link + NOTES touch-up → (5) `ci/run-tests.sh` + drift gate
→ (6) lint/template/unittest green; optional kind/k3s smoke.
