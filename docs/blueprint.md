# o3k Blueprint & OpenStack Compatibility Design

**Status**: Draft (in progress — cloud-storage#420)
**Created**: 2026-06-30
**Reference release**: OpenStack **2025.2 (Flamingo)**
**o3k version**: v0.6.1 (2026-04-22)
**Tracking**: [cloud-storage#420](https://github.com/cobaltcore-dev/cloud-storage/issues/420) (epic #413 — o3k for validation)

This is the **authoritative top-level** compatibility reference for o3k. It serves two audiences:

1. **Engineers working on o3k** — an accurate map of what is implemented, how, and where the edges are.
2. **Teams evaluating o3k as a devstack replacement** — an honest compatibility surface and an explicit list of gaps.

Deep per-endpoint detail and contract-test results live in
[`compatibility/OPENSTACK_COMPATIBILITY_REPORT.md`](compatibility/OPENSTACK_COMPATIBILITY_REPORT.md)
(linked appendix). Architecture rationale lives in [`ARCHITECTURE.md`](ARCHITECTURE.md). This document
is the single source of truth for *"what does o3k implement, and what doesn't it?"*

> **A note on numbers.** Earlier docs report o3k coverage as "104% (342/330 endpoints)." That figure
> counts raw HTTP routes (including URL aliases and version-discovery) against a hand-picked baseline.
> This blueprint counts **functional OpenStack API operations** under one stated rule (below) and reports
> honest per-resource coverage instead of a single percentage. The external epic (#413) assesses o3k at
> "~70% complete"; both views are reconciled in [§8 Known gaps](#8-known-gaps--limitations).

---

## Table of contents

1. [Service map (o3k ↔ OpenStack)](#1-service-map)
2. [API surface & counting methodology](#2-api-surface--counting-methodology)
3. [Compatibility matrix (per service)](#3-compatibility-matrix) — *in progress*
4. [Storage backends](#4-storage-backends) — *to do*
5. [Synchronous architecture & trade-offs](#5-synchronous-architecture--trade-offs) — *to do*
6. [Terraform coverage](#6-terraform-coverage) — *to do*
7. [sci-in-a-box integration surface](#7-sci-in-a-box-integration-surface) — *to do*
8. [Known gaps & limitations](#8-known-gaps--limitations) — *to do*

---

## 1. Service map

o3k runs as a single Go binary (~35 MB) that starts independent HTTP servers, one per OpenStack service,
plus a metadata server and Kubernetes health endpoints. All share one PostgreSQL pool and one stateless
JWT auth path.

| o3k service | OpenStack counterpart | API version | Port | Catalog type | Status |
|---|---|---|---|---|---|
| Keystone | Identity | v3 | 35357 | `identity` | Core |
| Nova | Compute | v2.1 (microversions 2.1–2.90) | 8774 | `compute` | Core |
| Neutron | Network | v2.0 | 9696 | `network` | Core |
| Cinder | Block Storage | v3 | 8776 | `volumev3` | Core |
| Glance | Image | v2 | 9292 | `image` | Core |
| Placement | Placement | v1.0 | 8778 | `placement` | **Stub** (empty lists, for Horizon) |
| Metadata | Nova metadata / EC2 | — | 8775 | — (no auth) | Core |
| Health | (k8s probes) | — | on identity port | — | `/livez` `/readyz` `/healthcheck` |

There is **no `object-store` (Swift) service** in the catalog — see [§8](#8-known-gaps--limitations).

---

## 2. API surface & counting methodology

Endpoint counts in prior docs disagreed (README: 342; OPENSTACK_COMPATIBILITY_REPORT: per-service tallies
summing differently; raw route greps: higher still) because none stated *what they counted*. This blueprint
fixes one rule and re-derives everything from the route registrations in `cmd/o3k/main.go` and
`internal/*/`.

**Counting rule.** A *functional operation* is a unique (HTTP method × canonical resource path). Excluded
from the functional count:

- **Version-discovery roots** (`GET /`, `GET /v3`, `GET /v2.1`, …) — informational, not resource operations.
- **Cinder project-scoped URL aliases** — Cinder registers most operations twice: a token-scoped form
  (`/v3/volumes`) and a project-in-path form (`/v3/{project_id}/volumes`). These are the *same* operation
  at two URLs; counted once.
- **PUT/PATCH duplicate registrations** bound to the same handler (o3k accepts both for client tolerance;
  e.g. Neutron `networks`/`subnets`/`ports`/`security-groups`). Counted once, by the OpenStack-canonical verb.

Reported **separately** (not part of the five-service OpenStack API count): Placement (stub), Metadata, Health.

### Per-service counts (derived from source, v0.6.1)

| Service | Raw routes registered | Functional operations | Notes |
|---|---:|---:|---|
| Keystone | 60 | **60** | discovery roots live in `main.go` |
| Nova | 74 | **71** | −2 discovery, −1 PUT/PATCH alias |
| Neutron | 100 | **94** | −2 discovery, −4 PUT/PATCH aliases |
| Cinder | 116 | **67** | −2 discovery, −2 PUT/PATCH, −45 project-scoped aliases |
| Glance | 38 | **38** | discovery roots live in `main.go` |
| **Five-service total** | **388** | **330** | |
| Placement (stub) | 5 | 3 | resource_providers / resource_classes / traits — empty lists |
| Metadata | 7 | 7 | EC2 + OpenStack formats, unauthenticated |
| Health | 4 | 3 | `/livez`, `/readyz`, `/healthcheck` (GET+HEAD) |
| **Total routes served** | **≈407** | | |

**Result:** o3k implements **~330 functional operations** across the five core OpenStack services. That is
the honest figure behind the "330 baseline"; the previous "342 / 104%" came from counting raw routes
(aliases + discovery) against it. Coverage is substantial — the per-resource detail is in
[§3](#3-compatibility-matrix), and what's genuinely *missing* is in [§8](#8-known-gaps--limitations).

> Cinder and Neutron functional counts are ±1–2 pending the full per-operation enumeration in §3.

---

## 3. Compatibility matrix

*In progress — next section.* Per service: each OpenStack api-ref resource group mapped to o3k status
(✅ implemented · ⚠️ partial · ❌ missing · 🅢 stub), with contract-test pass rates from
`test/contract/` and the OPENSTACK_COMPATIBILITY_REPORT appendix.

## 4. Storage backends

*To do.* Per-service backend support (local / Ceph RBD / S3-RGW-MinIO), the `//go:build ceph` packaging
caveat, hybrid failover semantics.

## 5. Synchronous architecture & trade-offs

*To do.* Single binary, no message queue, fail-fast timeouts, stateless JWT, PostgreSQL-only coordination;
trade-offs vs. RabbitMQ/oslo.messaging OpenStack.

## 6. Terraform coverage

*To do.* Per-resource matrix from `test/terraform/sc007/`; the port-id deviation; what is / isn't CI-gated.

## 7. sci-in-a-box integration surface

*To do.* References `charts/o3k/CONTRACT.md` + SPEC-012 (Helm lifecycle contract) and SPEC-013 health
probes; remaining integration gaps.

## 8. Known gaps & limitations

*To do.* Swift/object-store absent; Cinder contract coverage; Ceph build-tag packaging; multi-node Helm
deferred (#52); contract/Terraform tests not CI-gated; Placement stubbed; reconciliation of "104%" vs "~70%".
