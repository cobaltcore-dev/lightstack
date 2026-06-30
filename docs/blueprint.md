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
| Keystone | 60 | **59** | −1 discovery (`GET /v3`); `GET /` on identity port lives in `main.go` |
| Nova | 74 | **71** | −2 discovery, −1 PUT/PATCH alias |
| Neutron | 100 | **94** | −2 discovery, −4 PUT/PATCH aliases |
| Cinder | 116 | **67** | −2 discovery, −2 PUT/PATCH, −45 project-scoped aliases |
| Glance | 38 | **38** | discovery roots live in `main.go` |
| **Five-service total** | **388** | **329** | |
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

Per service, each canonical OpenStack **api-ref** resource group (2025.2) is mapped to o3k's
implementation status, derived from the route registrations in `internal/<service>/`. Status legend:

- ✅ **Implemented** — operation(s) present, exercised by tests or clients
- ⚠️ **Partial** — group present but some operations or semantics missing
- ❌ **Missing** — not implemented
- 🅢 **Stub** — route exists but returns canned/empty data

Contract-test figures come from `test/contract/<service>/` (gophercloud) and the
[OPENSTACK_COMPATIBILITY_REPORT](compatibility/OPENSTACK_COMPATIBILITY_REPORT.md) appendix.
**Caveat:** contract tests are *not* CI-gated (they need a live instance), so pass counts are
point-in-time, and a few report claims overstate the current code — flagged inline.

### 3.1 Keystone — Identity v3 (59 functional operations)

Canonical group list from the OpenStack Identity v3 api-ref (core operations + CURRENT extensions).

| Canonical group (Identity v3) | o3k | Notes |
|---|:--:|---|
| Authentication & token mgmt | ⚠️ | `POST`/`GET`/`DELETE /v3/auth/tokens` + `GET /v3/auth/projects`. Missing `HEAD /auth/tokens` (check), `GET /auth/catalog`, `GET /auth/system`/`domains`, MFA auth-receipts |
| Users | ✅ | CRUD + `password` + `/users/:id/projects` + `/users/:id/groups` |
| Application credentials | ✅ | create / list / get / delete (+ `GET /application_credentials/:id`) |
| Credentials | ✅ | full CRUD |
| Groups | ✅ | CRUD + membership (`/groups/:id/users`, add/remove) |
| Projects | ✅ | CRUD |
| Project tags | ❌ | — |
| Domains | ✅ | CRUD |
| Domain configuration | ⚠️ | `GET /domains/:id/config` only (no set/delete, no group/option granularity) |
| Roles | ✅ | CRUD |
| Role assignments | ⚠️ | project↔user assign/unassign/list/check + `GET /role_assignments`. Missing group & domain-scoped assignments, role inferences/implied roles |
| System role assignments | ❌ | — |
| Service catalog & endpoints | ⚠️ | services CRUD; endpoints list/create/delete (no `GET`/`PATCH /endpoints/:id`). **Known bug:** catalog URL-template substitution (3 contract-test failures) |
| Policies | ❌ | — |
| Regions | ❌ | — |
| Unified limits (registered_limits / limits) | ❌ | — |
| OS-INHERIT | ❌ | — |
| OS-FEDERATION / OS-OAUTH1 / OS-OAUTH2 / OS-TRUST | ❌ | enterprise SSO / delegation |
| OS-EP-FILTER / OS-ENDPOINT-POLICY / OS-REVOKE | ❌ | — |
| OS-EC2 (ec2 credentials) | ❌ | **Doc drift:** the compat report lists 5 EC2-credential endpoints, but none are registered in `internal/keystone/handlers.go` |
| OS-PKI / OS-SIMPLE-CERT | ❌ | deprecated upstream |

**Contract tests** (`test/contract/keystone/`, 11 files): report records **52 pass / 3 fail (94.5%)**; the
3 failures are the catalog URL-substitution bug. Not CI-gated.

**Doc-vs-code discrepancies found:** the compat report also lists `HEAD /v3/auth/tokens` and
`GET /v3/auth/catalog` as implemented; neither is registered in the current code. The status column
above reflects the code, not the report.

**Validation verdict:** o3k implements the full **core IAM surface** that Terraform, Horizon, and the
OpenStack CLI exercise — token auth, users, projects, domains, groups, roles, project role-assignments,
service catalog, credentials, application credentials. Every ❌ above is an enterprise
SSO / delegation / policy / limits feature **outside the sci-in-a-box validation scope** — the one
in-scope defect is the catalog URL-substitution bug (see [§8](#8-known-gaps--limitations)).

### 3.2 Nova — Compute v2.1

*Next step.*

### 3.3 Neutron — Network v2.0

*To do.*

### 3.4 Cinder — Block Storage v3

*To do.*

### 3.5 Glance — Image v2

*To do.*

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

This section collects (a) cross-cutting limitations and (b) per-service lists of canonical OpenStack
endpoints o3k does not implement.

*Cross-cutting gaps — to do:* Swift/object-store absent; Cinder contract coverage; Ceph build-tag
packaging; multi-node Helm deferred (#52); contract/Terraform tests not CI-gated; Placement stubbed;
reconciliation of "104%" vs "~70%".

### 8.1 Keystone — not-implemented endpoints

> **Scope note.** None of the endpoints below are on the sci-in-a-box / Terraform / Horizon validation
> path — they are enterprise SSO, delegation, policy, quota-limit, and tagging features. They are listed
> for completeness against the OpenStack Identity v3 api-ref (2025.2). The one *in-scope* Keystone defect
> is a **bug**, not a missing endpoint: catalog URL-template substitution (see §3.1).

The table lists every canonical Identity v3 operation o3k does not implement, with **Impl.**
(⚠️ group partially implemented · ❌ group absent) and the reason it is unimplemented. Multiple
endpoints per group are separated by a line break (`<br>`).

| Identity v3 group | Impl. | Not-implemented endpoint(s) | Justification |
|---|:--:|---|---|
| Authentication & token mgmt | ⚠️ | `HEAD /v3/auth/tokens`<br>`GET /v3/auth/catalog`<br>`GET /v3/auth/domains`<br>`GET /v3/auth/system`<br>`GET, HEAD /v3/auth/receipts` | o3k validates tokens via `GET /v3/auth/tokens` and embeds the catalog in the token-issue response; the separate check / catalog / domains / system discovery calls aren't required by Horizon, Terraform, or the CLI against o3k. MFA receipts assume a multi-step auth-plugin chain the single HMAC-JWT issuer doesn't model. |
| Service catalog & endpoints | ⚠️ | `GET /v3/endpoints/{endpoint_id}`<br>`PATCH /v3/endpoints/{endpoint_id}` | The catalog is seeded at bootstrap and read through the token; per-endpoint show/update is an admin catalog-editing convenience the validation clients never call. |
| Domain configuration | ⚠️ | `PUT, PATCH, DELETE /v3/domains/{id}/config`<br>`GET /v3/domains/config/default`<br>`…/config/{group}` (GET / PATCH / DELETE)<br>`…/config/{group}/{option}` (GET / PATCH / DELETE) | Per-domain config drives multi-backend Keystone (per-domain LDAP / identity drivers). o3k is single-backend (PostgreSQL + HMAC-JWT), so writable domain config has no effect; the read-only `GET …/config` exists only for Horizon's identity panel. |
| Role assignments (advanced) | ⚠️ | `HEAD /v3/projects/{pid}/users/{uid}/roles/{rid}` (check)<br>group-on-project, user/group-on-domain (assign / list / check)<br>implied roles `…/roles/{id}/implies/{id}`, `GET /v3/role_inferences` | o3k's RBAC is the project-scoped user→role model Terraform / Horizon / CLI use. Group- and domain-scoped grants and role-inference trees are advanced RBAC the validation scenarios don't exercise. |
| Project tags | ❌ | `GET, PUT, DELETE /v3/projects/{pid}/tags`<br>`GET, PUT, DELETE /v3/projects/{pid}/tags/{value}` | Project tagging is a metadata / search convenience; no validation scenario filters projects by tag. |
| Regions | ❌ | `GET, POST /v3/regions`<br>`GET, PUT, PATCH, DELETE /v3/regions/{id}` | o3k is a single-region deployment; multi-region catalog topology is out of scope. |
| Policies | ❌ | `GET, POST /v3/policies`<br>`GET, PATCH, DELETE /v3/policies/{id}` | The policy API stores / distributes RBAC policy blobs; o3k enforces authorization in-code (role checks in middleware), so there is no policy document to serve. De-emphasized upstream. |
| System role assignments | ❌ | `GET, PUT, HEAD, DELETE /v3/system/users/{uid}/roles/{rid}` (+ list)<br>`…/system/groups/{gid}/roles/{rid}` (+ list) | System-scoped roles govern deployment-wide cloud-admin operations; o3k has no system scope — it uses project / admin role checks. |
| Unified limits | ❌ | `GET, POST /v3/registered_limits`, `GET, PATCH, DELETE /v3/registered_limits/{id}`<br>`GET, POST /v3/limits`, `GET, PATCH, DELETE /v3/limits/{id}`, `GET /v3/limits/model` | o3k enforces quotas per-service (Nova / Cinder / Neutron `quota-sets`), not via Keystone's centralized unified-limits model; the validation path uses the per-service quota APIs. |
| OS-INHERIT | ❌ | `PUT, HEAD, DELETE …/roles/{rid}/inherited_to_projects` (+ `GET …/roles/inherited_to_projects`; domain- & project-scoped, user & group) | Inherited (domain→project) role assignments are an advanced RBAC convenience; o3k's flat project-scoped model has no inheritance. |
| OS-EC2 credentials | ❌ | `GET, POST /v3/users/{uid}/credentials/OS-EC2`<br>`GET, DELETE …/OS-EC2/{credential_id}` | EC2-style HMAC credentials target the EC2 / S3 path; o3k exposes no EC2 / object surface and the validation clients use Keystone tokens / app-creds. **The compat report wrongly lists these as implemented.** |
| OS-TRUST | ❌ | `GET, POST /v3/OS-TRUST/trusts`<br>`GET, DELETE /v3/OS-TRUST/trusts/{id}` (+ trust-roles) | Trusts delegate roles for unattended / Heat-style workflows; not used by the validation scenarios. |
| OS-FEDERATION | ❌ | identity providers · protocols · mappings · service providers · `GET /v3/OS-FEDERATION/projects` · `/domains` · SAML2/ECP auth (entire extension) | Federated identity (external IdPs / SAML / OIDC) is enterprise SSO; o3k authenticates locally against its own user store. |
| OS-OAUTH1 / OS-OAUTH2 | ❌ | consumers, request / access tokens, authorization; OAuth2 token + discovery (entire extensions) | OAuth delegation flows are enterprise integrations outside the validation scope. |
| OS-EP-FILTER | ❌ | endpoint groups CRUD + project↔endpoint / project↔endpoint-group associations | Endpoint filtering tailors the catalog per project; o3k serves a single flat catalog. |
| OS-ENDPOINT-POLICY | ❌ | policy↔endpoint / policy↔service associations | Associates policy blobs with endpoints; depends on the (absent) policy API. |
| OS-REVOKE | ❌ | `GET /v3/OS-REVOKE/events` | Revocation-event lists invalidate cached tokens; o3k tokens are stateless short-TTL JWTs validated by signature, so there is no revocation list to publish (`DELETE /v3/auth/tokens` still works locally). |
| OS-SIMPLE-CERT *(deprecated)* | ❌ | `GET /v3/OS-SIMPLE-CERT/ca`<br>`GET /v3/OS-SIMPLE-CERT/certificates` | Served signing certs for the deprecated PKI token format; obsolete — o3k uses HMAC-JWT. |
| OS-PKI *(deprecated)* | ❌ | revoked-token list | Deprecated PKI token revocation list; obsolete. |
