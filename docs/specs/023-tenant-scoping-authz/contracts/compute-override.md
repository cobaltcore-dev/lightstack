# Contract: Compute administrative override

**Feature**: `023-tenant-scoping-authz` | Covers FR-001 – FR-008a, User Story 1

Actors used throughout:

| Actor | Holds | Token scoped to |
|---|---|---|
| `ADMIN` | `admin` | project **A** |
| `MEMBER_A` | `member` | project **A** |
| `MEMBER_B` | `member` | project **B** |

`SRV_B` is a server owned by project **B**.

---

## C1 — Point lookup, administrator

```
GET /v2.1/servers/{SRV_B}          X-Auth-Token: ADMIN
→ 200   body.server.id == SRV_B
        body.server.tenant_id == B          # not A
```

Applies identically to: `GET /servers/{id}`, `/servers/{id}/diagnostics`, `/servers/{id}/metadata`, `/servers/{id}/tags`, `/servers/{id}/os-instance-actions`, `/servers/{id}/os-volume_attachments`, `/servers/{id}/os-interface`, `POST /servers/{id}/remote-consoles`.

## C2 — Point lookup, non-administrator

```
GET /v2.1/servers/{SRV_B}          X-Auth-Token: MEMBER_A
→ 404
```

The body MUST be identical to the body returned for a server identifier that does not exist anywhere. A `403` is a contract violation: it confirms the server exists.

```
GET /v2.1/servers/00000000-0000-0000-0000-999999999999   X-Auth-Token: MEMBER_A
→ 404   same body shape as above
```

An unscoped token names no project, so it owns no server:

```
GET /v2.1/servers/{SRV_A}          X-Auth-Token: UNSCOPED
→ 404   never 500
```

## C3 — State-changing operations, administrator

```
DELETE /v2.1/servers/{SRV_B}                              X-Auth-Token: ADMIN  → 204
POST   /v2.1/servers/{SRV_B}/action  {"os-stop": null}    X-Auth-Token: ADMIN  → 202
POST   /v2.1/servers/{SRV_B}/action  {"os-start": null}   X-Auth-Token: ADMIN  → 202
POST   /v2.1/servers/{SRV_B}/action  {"reboot": {"type":"SOFT"}}  ADMIN        → 202
POST   /v2.1/servers/{SRV_B}/action  {"resize": {"flavorRef":"2"}}  ADMIN      → 202
PUT    /v2.1/servers/{SRV_B}/tags/{tag}                   X-Auth-Token: ADMIN  → 201
POST   /v2.1/servers/{SRV_B}/os-volume_attachments        X-Auth-Token: ADMIN  → 200
```

Each MUST return `404` for `MEMBER_A`.

## C4 — Administrator-only operations reach other projects (FR-008a)

The regression this repairs: both currently gate on `admin` and then scope the query to the **caller's** project, so they answer `404` for every instance the administrator does not own.

```
POST /v2.1/servers/{SRV_B}/action  {"os-resetState": {"state":"error"}}   ADMIN     → 202
POST /v2.1/servers/{SRV_B}/action  {"evacuate": {"host":"h1"}}            ADMIN     → 200
POST /v2.1/servers/{SRV_B}/action  {"os-resetState": {"state":"error"}}   MEMBER_A  → 404
POST /v2.1/servers/{SRV_A}/action  {"os-resetState": {"state":"error"}}   MEMBER_A  → 403
```

`evacuate` returns `200`, and may return a `4xx` when no target host exists; it MUST NOT answer `404`.

**The last two lines are the interesting pair, and the ordering that produces them is deliberate.**
`ServerAction` resolves its target *before* dispatching to an action, so:

- `MEMBER_A` addressing **`SRV_B`** — a server they cannot see — gets `404` for *any* action,
  including an administrator-only one. The administrative gate is never reached. This is the
  stronger answer: a `403` here would confirm `SRV_B` exists, which is exactly the disclosure
  FR-003 forbids.
- `MEMBER_A` addressing **`SRV_A`** — their own server — gets past the resolve, reaches the gate, and
  is refused `403`. There is nothing to disclose: they already know their own server exists.

So the status code distinguishes *why* the caller was refused, without ever revealing whether a
foreign resource exists.

## C5 — Creation is not overridable (FR-004a)

```
POST /v2.1/servers   {"server": {..., "tenant_id": "B"}}   X-Auth-Token: ADMIN
→ the server is created in project A, or the request is refused
→ NEVER a server owned by project B
```

## C6 — Name matching never spans projects (FR-005c, FR-005d)

Given project **A** and project **B** each own a server named `web-1`:

```
DELETE /v2.1/servers/web-1        X-Auth-Token: ADMIN   → deletes A's web-1, never B's
GET    /v2.1/servers/web-1        X-Auth-Token: ADMIN   → A's web-1
```

Given only project **B** owns a server named `only-b`:

```
GET    /v2.1/servers/only-b       X-Auth-Token: ADMIN   → 404
DELETE /v2.1/servers/only-b       X-Auth-Token: ADMIN   → 404
```

A name MUST NOT fall through to a cross-project search. Addressing `SRV_B` requires its identifier.

## C7 — Attribution (FR-007, FR-007a)

After `DELETE /v2.1/servers/{SRV_B}` as `ADMIN`, a row exists in the instance action record with:

- `project_id` == **B** (the owner), not A
- `user_id` == ADMIN's user id
- `action` == `delete`

and `MEMBER_B` can read it:

```
GET /v2.1/servers/{SRV_B}/os-instance-actions   X-Auth-Token: MEMBER_B
→ 200   contains an entry whose user_id is ADMIN's
```

## C8 — Refusal logging (FR-007b)

A refused cross-tenant attempt emits one `WARN` naming the caller's project, the target identifier and the owning project. A permitted access by the owner emits nothing.

```
GET /v2.1/servers/{SRV_B}   X-Auth-Token: MEMBER_A
→ 404 + one WARN log line containing caller project A, target SRV_B, owner B
```
