# Implementation Plan: Helm Charts for Kubernetes Deployment (Single-Node and Multi-Node)

**Branch**: `008-helm-charts` | **Date**: 2026-06-11 | **Spec**: [spec.md](./spec.md)
**Input**: Feature specification from `docs/specs/008-helm-charts/spec.md`

## Summary

A single in-repo Helm chart (`charts/o3k/`) that deploys the O3K single binary (all seven HTTP services — identity, compute, placement, network, block storage, image, metadata) to Kubernetes in **two modes selected by one value** (`deploymentMode: single-node | multi-node`, default `single-node`). The chart-shaping prerequisites (SPEC-007: admin-password bootstrap + configurable catalog scheme) have landed.

Per the **2026-06-11 clarification (round 8)**, the validated done-line for SPEC-008 is **single-node stub mode (+ optional TLS)**. Real-mode and multi-node templates are authored and `helm template`/lint-clean now, but their end-to-end success criteria (SC-003, SC-004, real-VM login/floating-IP/restart) are **deferred** until the product post-conditions (WS-1/4/5/6, issue [#52](https://github.com/cobaltcore-dev/o3k/issues/52)) ship.

**Technical approach**: the chart renders the full `o3k.yaml` from values into a **Secret** mounted at the binary's `--config` path — one file carries all settings, including credentials, because the binary honors only four env overrides (`O3K_DB_URL`, `O3K_JWT_SECRET`, `O3K_ENDPOINT_HOST`, `O3K_ENDPOINT_SCHEME`), used for `existingSecret`/TLS wiring. Single-node → Deployment (stub may scale; real pins `replicas=1` with `Recreate`); multi-node → DaemonSet on label-designated nodes. ClusterIP by default; LoadBalancer/NodePort/Ingress selectable; optional per-service-port TLS. External PostgreSQL is required (the chart deploys no database); Redis is optional and off. Install fails fast when a required secret or the image reference is missing.

## Technical Context

**Language/Version**: Helm 3 chart (Go templating); targets the unmodified O3K Go 1.26 binary
**Primary Dependencies**: Helm ≥3.12; Kubernetes ≥1.25 (PodSecurity Admission GA — `restricted` for stub, `privileged` for real); an operator-supplied O3K container image (required, no default — FR-018)
**Storage**: external PostgreSQL 14+ (operator-provided, hard prerequisite — FR-006); node-local `hostPath` for real-mode VM/local data (FR-012a); optional Ceph RBD / S3 for shared volume/image storage
**Testing**: `helm lint`, `helm template` (render/snapshot), helm-unittest (value→manifest + fail-fast gate assertions), `helm test` hook (identity check — FR-015); single-node-stub smoke install on kind/k3s in CI; existing bash suites (`test/quick_test.sh`, `test/vxlan_multinode_test.sh`) reused for deeper/deferred validation
**Target Platform**: vanilla Kubernetes + k3s (OpenShift documented via SCC); Linux worker nodes required for real mode
**Project Type**: Deployment artifact (Helm chart) over an existing single-binary modular monolith — **no application source changes** except the reference image (FR-012f: add `dnsmasq`)
**Performance Goals**: SC-001 nothing → working single-node cloud in <10 min, all endpoints healthy <5 min; SC-003 (multi-node, N active <60s) — *deferred*
**Constraints**: chart deploys no DB and no Redis; stub default (unprivileged, passes `restricted` PSA); fail-fast install on missing required secret/image; 100% OpenStack CLI/Terraform compatibility preserved (no API change); in-repo install only (no registry — FR-019)
**Scale/Scope**: one chart, two modes, seven services, ~40–50 documented values, ~15–25 templates

## Constitution Check

*GATE: Must pass before Phase 0 research. Re-checked after Phase 1 design.*

| Principle | Assessment |
|-----------|------------|
| **I. Flamingo API compatibility (NON-NEGOTIABLE)** | PASS — the chart changes no API surface; catalog host/scheme are driven by the landed SPEC-007 knobs. Verified by the reused CLI/Terraform/contract suites. |
| **II. Synchronous operations** | PASS — no queues/async added; relies on the product's startup migration serialized by golang-migrate's lock — no migration Job (FR-012b). |
| **III. Fail-fast design** | PASS — extends fail-fast to install time (missing JWT/admin/DB/image → Helm `fail`); DB/Redis reachability remain boot-time fail-fast in the product. |
| **IV. Multi-mode architecture** | PASS — stub/real per service exposed via values; stub is the default and runs unprivileged (satisfies `restricted` PSA). |
| **V. Project isolation** | PASS — unchanged; per-project namespaces remain a runtime product behavior. |
| **VI. Single binary deployment** | PASS — one container = the unmodified single binary; no microservice split. Kubernetes is an *optional* substrate; the binary still runs standalone / in Compose. |
| Tech constraint: *"no container orchestration required"* | PASS (additive) — the chart offers k8s as an option; it does not make k8s a product requirement. |
| DB migrations reversible | N/A — the chart adds no migrations. |
| Packaging note (FR-012f) | The reference `build/package/Dockerfile` must add `dnsmasq` (alongside iptables/iproute2/ipset) for real-mode DHCP — a packaging change, no binary/API change. |

**Result**: All gates PASS. No violations requiring justification beyond the minimal Complexity Tracking below.

## Project Structure

### Documentation (this feature)

```text
docs/specs/008-helm-charts/
├── spec.md              # Feature spec (8 clarification rounds)
├── plan.md              # This file
├── research.md          # Phase 0 output
├── data-model.md        # Phase 1 output (the values.yaml schema)
├── quickstart.md        # Phase 1 output
├── contracts/           # Phase 1 output (values / rendered-resource / helm-test contracts)
└── checklists/
    └── requirements.md  # Spec quality checklist
```

### Source Code (repository root)

```text
charts/o3k/
├── Chart.yaml
├── values.yaml                  # documented defaults (FR-017); stub + single-node
├── values.schema.json           # input validation + required-value gates (FR-007/007a/018)
├── templates/
│   ├── _helpers.tpl             # names, labels, mode guards, secret resolution, storage validation
│   ├── secret.yaml              # full o3k.yaml (incl. credentials) rendered from values, unless existingSecret
│   ├── deployment.yaml          # single-node (stub scalable; real replicas=1 / Recreate)
│   ├── daemonset.yaml           # multi-node (designation-labeled nodes)
│   ├── service.yaml             # seven ports; ClusterIP default
│   ├── ingress.yaml             # optional; per-service-port TLS
│   ├── psa-namespace.yaml       # privileged PodSecurity label (real-mode toggle)
│   ├── NOTES.txt
│   └── tests/
│       └── identity-check.yaml  # helm test hook (FR-015)
└── README.md                    # generated values reference (FR-017)

build/package/Dockerfile         # FR-012f: add dnsmasq (+ keep iptables/iproute2/ipset)
```

**Structure Decision**: One chart with a mode selector (FR-002). Templates guard on `deploymentMode` and per-service modes via `_helpers.tpl`. No application Go changes; the only product-repo file touched is the reference Dockerfile (FR-012f).

## Complexity Tracking

| Item | Why needed | Simpler alternative rejected because |
|------|------------|--------------------------------------|
| Conditional templates for two modes in one chart | FR-002 (single chart, one selector) keeps config/docs/versioning unified | Two separate charts → duplicated values/docs and drift |
| Real-mode + multi-node templates authored now but not yet e2e-validated | Keeps the chart structurally complete; honest gating per the 2026-06-11 clarification | Cutting them to a later spec → chart-structure churn when the post-conditions land |
