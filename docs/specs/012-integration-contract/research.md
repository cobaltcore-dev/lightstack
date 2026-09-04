# Phase 0 Research: Integration Contract

All decisions below are grounded in the existing chart (`charts/lightstack/`). No NEEDS CLARIFICATION
remains in the spec; these resolve the plan-level "how", not spec-level "what".

## R1 — Conditional requirement enforcement: render-time guards, not schema `if/then`

**Decision**: Cross-field / conditional requirements stay **render-time fail-fast template guards**;
`values.schema.json` does structural *per-field* validation only.

**Rationale**: The chart already enforces — with helm-unittest coverage in `tests/gates_test.yaml` —
that `jwtSecret`/`adminPassword`/`database.url` are required (unless `existingSecret`), the teardown
`kubectlImage` is required under `dataRetentionPolicy: delete`, and multi-node real rejects
`local` glance/cinder backends. These use `required`/`fail` in `_helpers.tpl` and surface clear
`errorPattern`s. Helm's bundled JSON-Schema validator has historically shaky `if/then/else` support,
and cross-field conditionals are awkward in draft-07. Keeping them render-time is proven, testable,
and avoids validator-version risk. Spec FR-007 explicitly leaves the mechanism to planning.

**Alternatives considered**: Schema `if/then/else` + `dependentRequired` — rejected (validator
support risk, duplicates existing proven guards, harder to give a friendly message).

## R2 — `additionalProperties: false` scope

**Decision**: `additionalProperties: false` on the **top level** and on **typed sub-objects**
(`image`, `database`, `secrets`, `service`, `tls`, `nova`, `neutron`, `cinder`, `glance`, `realMode`,
`podSecurity`, `multiNode`, `cache`, `teardown`, `logging`). Leave **free-form maps open** (no
`additionalProperties: false`): `service.annotations`, `ingress.annotations`, `resources`,
`nodeSelector`, `tolerations`, `affinity`, and `ingress` (className/annotations/hosts vary).

**Rationale**: Rejecting unknown/misspelled keys (e.g. `databse:`) is the "rejects invalid values
structurally" intent (FR-006). Kubernetes-native passthrough maps must stay open or operators can't
set arbitrary annotations/scheduling. Risk: the schema must enumerate every key the chart reads or
installs break — mitigated by the R5 coverage gate.

**Alternatives considered**: Strict everywhere (rejected — breaks k8s passthrough); open everywhere
(rejected — defeats the typo-rejection goal).

## R3 — JSON Schema draft + keyword set

**Decision**: Stay on **draft-07** (already declared in the existing schema). Use only widely
supported keywords: `type`, `enum`, `required`, `minimum`, `minLength`, `pattern`,
`additionalProperties`, `description`. Avoid `if/then/else` (see R1).

**Rationale**: Minimizes validator-compatibility risk across Helm versions; everything needed for
structural validation is expressible without conditionals.

## R4 — Status recipe field mapping

**Decision**: The documented recipe reads exactly these signals:

- **Helm**: `helm status <release> -o json` → `.info.status` ∈ {`pending-install`, `pending-upgrade`,
  `deployed`, `failed`, …}.
- **Workload readiness** (topology-appropriate):
  - single-node **Deployment**: ready when `.status.updatedReplicas == .spec.replicas` **and**
    `.status.availableReplicas == .spec.replicas` (equivalently `kubectl rollout status deploy/<rel>-lightstack`).
  - multi-node **DaemonSet**: ready when `.status.numberReady == .status.desiredNumberScheduled`
    **and** `.status.updatedNumberScheduled == .status.desiredNumberScheduled`.
- **Terminal failure** (fail-fast before the bound): any pod with
  `containerStatuses[].state.waiting.reason ∈ {CrashLoopBackOff, ImagePullBackOff, ErrImagePull}`
  or pod `.status.phase == Failed`.

Mapping (full table in `data-model.md`): **Ready** = `deployed` ∧ workload-ready; **Failed** =
`failed` ∨ terminal-pod ∨ readiness-bound-elapsed; **Provisioning** = otherwise.

**Grounding**: the pod **readinessProbe** is application-aware — `httpGet /v3` on the identity port
(`_helpers.tpl:226`) — so "pod ready" already implies the Keystone API is serving; no extra app-level
probe is needed for Ready. The orchestrator owns `helm install/upgrade --wait --timeout`.

**Alternatives considered**: Helm-status-only (rejected — misses pod-level terminal failures within
the wait); ship a status script (rejected by clarification — contract is documentation, not product code).

## R5 — Schema-rejection + drift testing

**Decision**: Two complementary gates.

1. **Rejection (bash, primary)**: add `charts/lightstack/ci/invalid-values/*.yaml` fixtures (unknown key,
   missing required, malformed DB ref, bad storageMode, out-of-range replicaCount) and assert in
   `ci/run-tests.sh` that `helm template r <chart> -f <fixture>` **fails** for each, and that a
   complete valid fixture renders.
2. **Drift/coverage**: a check that every top-level key in `values.yaml` is present in
   `values.schema.json` (and vice-versa) — so `additionalProperties: false` can't silently reject a
   real key, and no input goes undocumented (SC-001/SC-005).

helm-unittest `failedTemplate`/`errorPattern` continues to cover the *render-time* guards (R1).

**Rationale**: `helm template`/`install`/`lint` definitively run schema validation; bash assertions
on `helm template` are the robust way to prove rejection. helm-unittest's schema-validation behavior
is less certain, so it stays on template-level guards.

**Alternatives considered**: rely solely on helm-unittest (rejected — schema-rejection coverage
uncertain); no drift gate (rejected — `additionalProperties:false` makes silent key omission a
release-breaker).

## R6 — Endpoint resolution (identity-anchored)

**Decision**: The contract guarantees the **Keystone `/v3` endpoint** — in-cluster Service
`http://<release>-lightstack:35357/v3`, or advertised `<scheme>://<endpointHost>:35357/v3` when configured.
The other five services (compute, placement, network, volumev3, image) are discovered from the
**service catalog** in the Keystone token (`auth.go:334`/`BuildServiceCatalog`, fallback
`buildHardcodedCatalog`). Catalog URLs honor `LIGHTSTACK_ENDPOINT_HOST` + scheme; the six in-cluster ports
in `NOTES.txt` are marked **non-authoritative** debugging info. The node-local metadata endpoint is
not in the catalog and is excluded.

**Coupling**: catalog correctness depends on `endpointHost` being set to a consumer-reachable host
(unset → advertises `localhost`). The contract states this input↔output coupling (FR-012a).

**Alternatives considered**: enumerate all six per-service URLs (rejected by clarification — those
in-cluster URLs diverge from the catalog and duplicate what the token already returns).

## R7 — Contract versioning (resolves the deferred clarify item)

**Decision**: The contract **rides `Chart.version` (SemVer)**. `CONTRACT.md` states the compatibility
convention: a chart **MAJOR** bump may change the contract incompatibly; **MINOR/PATCH** are
backward-compatible. No separate contract-version field.

**Rationale**: Standard Helm practice; the chart already versions independently of the product
(`Chart.version` vs `appVersion`). A second version axis adds maintenance for no consumer gain.

## R8 — Packaging the contract

**Decision**: Place `CONTRACT.md` at the chart root. Verified `.helmignore` excludes only `.git/`,
junk files, `/tests/`, and `ci/` — **not** `*.md` — so `CONTRACT.md` (like `README.md`) ships in
`helm package`. Add an SPDX header (repo REUSE convention).

**Rationale**: "Published alongside the chart" = inside the packaged artifact and discoverable from
the README (FR-013/014/015). No `.helmignore` change required.
