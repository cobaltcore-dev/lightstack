# Contract: Operator-only surfaces

**Feature**: `023-tenant-scoping-authz` | Covers FR-008e – FR-008g, User Story 6

Actors as in `compute-override.md`: `ADMIN` holds `admin`; `MEMBER_A` and `MEMBER_B` hold `member` in projects **A** and **B**.

These surfaces have **no owning project**, so the owner-or-administrator rule does not apply to them — access is administrative or nothing. The exception is the per-server migration set, which does have an owner and takes the normal override.

---

## O1 — Deployment-wide migration list (FR-008e)

```
GET /v2.1/os-migrations   X-Auth-Token: ADMIN     → 200, every project's migrations
GET /v2.1/os-migrations   X-Auth-Token: MEMBER_A  → 403
```

Today `MEMBER_A` receives `200` carrying every project's `server_uuid`, `source_node`, `dest_node` and flavour changes. The handler's own comment already records that this is "an admin endpoint in real OpenStack".

## O2 — Host aggregate reads (FR-008f)

```
GET /v2.1/os-aggregates        X-Auth-Token: ADMIN     → 200
GET /v2.1/os-aggregates/{id}   X-Auth-Token: ADMIN     → 200
GET /v2.1/os-aggregates        X-Auth-Token: MEMBER_A  → 403
GET /v2.1/os-aggregates/{id}   X-Auth-Token: MEMBER_A  → 403
```

Reads are gated too. Upstream gates the whole aggregate policy family, and the topology is operator information: which hypervisors are grouped together tells a tenant about the deployment's scheduling layout.

## O3 — Host aggregate writes (FR-008f) — the privilege escalation

Every one of these currently succeeds for `MEMBER_A`. Each MUST become `403`:

```
POST   /v2.1/os-aggregates              {"aggregate": {...}}                  MEMBER_A → 403
PUT    /v2.1/os-aggregates/{id}         {"aggregate": {"name": "renamed"}}    MEMBER_A → 403
DELETE /v2.1/os-aggregates/{id}                                               MEMBER_A → 403
POST   /v2.1/os-aggregates/{id}/action  {"add_host": {"host": "h1"}}          MEMBER_A → 403
POST   /v2.1/os-aggregates/{id}/action  {"remove_host": {"host": "h1"}}       MEMBER_A → 403
POST   /v2.1/os-aggregates/{id}/action  {"set_metadata": {"metadata": {...}}} MEMBER_A → 403
```

and MUST continue to succeed for `ADMIN`. The action route dispatches internally to three handlers; the gate MUST cover all three, including any action name the dispatcher does not recognise — a gate placed only on the recognised branches would leave the default path open.

## O4 — Per-server migrations (FR-008g)

These *do* have an owner, so they take the ordinary override. There is no ownership check on any of them today.

Given `SRV_B` owned by project **B** with a migration record `MIG_B`:

```
GET    /v2.1/servers/{SRV_B}/migrations                  MEMBER_A → 404
GET    /v2.1/servers/{SRV_B}/migrations/{MIG_B}          MEMBER_A → 404
DELETE /v2.1/servers/{SRV_B}/migrations/{MIG_B}          MEMBER_A → 404
POST   /v2.1/servers/{SRV_B}/migrations/{MIG_B}/action   MEMBER_A → 404
```

```
GET    /v2.1/servers/{SRV_B}/migrations                  MEMBER_B → 200
GET    /v2.1/servers/{SRV_B}/migrations                  ADMIN    → 200
DELETE /v2.1/servers/{SRV_B}/migrations/{MIG_B}          ADMIN    → 202
```

`404` rather than `403` for `MEMBER_A`, per the disclosure rule in R10 — these are owner-or-administrator, and distinguishing "absent" from "someone else's" would confirm the server exists.

**This is the one place in the feature where behaviour tightens for a non-administrator on a per-server path.** `MEMBER_A` can perform all four operations today.

## O5 — Administrator behaviour is unchanged throughout

Every operation in O1–O4 that succeeds for `ADMIN` today MUST succeed identically afterwards, with the same status and body. The existing contract tests (`migrations_list_test.go`, `aggregates_test.go`) authenticate through `setupNovaClient` as `OS_USERNAME`, defaulting to `admin`, and MUST pass unchanged.

## O6 — The gate is exercised from both directions (FR-019a)

For each surface above, the validation suite MUST contain both a refusal check as a non-administrator and a success check as an administrator. A gate added but never exercised as a *success* is indistinguishable from one that broke the endpoint outright.
