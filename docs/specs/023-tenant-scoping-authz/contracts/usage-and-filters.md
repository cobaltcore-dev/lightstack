# Contract: Usage confidentiality & identity filtering

**Feature**: `023-tenant-scoping-authz` | Covers FR-008b – FR-008d (User Story 3) and FR-024 – FR-027 (User Story 5)

Actors as in `compute-override.md`.

---

## Part 1 — Usage reporting (User Story 3)

### U1 — The aggregate form is administrator-only (FR-008b)

```
GET /v2.1/os-simple-tenant-usage    X-Auth-Token: ADMIN     → 200, entries for every project
GET /v2.1/os-simple-tenant-usage    X-Auth-Token: MEMBER_A  → 403
```

Today `MEMBER_A` receives `200` with every project's instance count, vCPU, memory and disk usage. That is the defect.

The existing contract test `test/contract/nova/tenant_usage_test.go` authenticates as `admin` and asserts `200`; it MUST continue to pass unchanged.

### U2 — The per-project form is scoped to the caller (FR-008c)

```
GET /v2.1/os-simple-tenant-usage/{B}   X-Auth-Token: MEMBER_A
→ 403 — NEVER project B, and never project A in its place
```

```
GET /v2.1/os-simple-tenant-usage/{A}   X-Auth-Token: MEMBER_A → 200, project A
GET /v2.1/os-simple-tenant-usage/{B}   X-Auth-Token: ADMIN    → 200, project B
GET /v2.1/os-simple-tenant-usage/{A}   X-Auth-Token: ADMIN    → 200, project A
```

A non-administrator naming any project but their own — another project's, one that does not exist, or a malformed identifier — gets the same `403`, before anything is resolved, so the endpoint cannot be used to probe which projects exist. Upstream's `os-simple-tenant-usage:show` policy refuses the same way.

### U3 — Bad targets are refused, not zeroed (FR-008d)

```
GET /v2.1/os-simple-tenant-usage/not-a-uuid                                X-Auth-Token: ADMIN → 4xx
GET /v2.1/os-simple-tenant-usage/00000000-0000-0000-0000-999999999999      X-Auth-Token: ADMIN → 404
```

Today both return `200` with every figure at zero, which a caller cannot distinguish from a real project owning no instances. A real project that owns no instances still returns `200` with zeroes — that case is legitimate and unchanged.

### U4 — No information leaks through timing or shape

`MEMBER_A`'s answer for `{B}` MUST NOT depend on whether project B exists or how many instances it owns.

---

## Part 2 — Identity collection filtering (User Story 5)

### F1 — Filters are applied (FR-024)

Given seeded roles `admin`, `member`, `reader`:

```
GET /v3/roles                  → 200, 3 roles
GET /v3/roles?name=member      → 200, exactly 1 role, named "member"
```

Today the filtered form returns all 3 — and a caller reading `.roles[0]` silently gets `admin`, because the list is ordered by name.

Equivalent for the remaining five collections:

```
GET /v3/users?name=<name>            → only matching users
GET /v3/users?domain_id=<id>         → only users in that domain
GET /v3/projects?name=<name>         → only matching projects
GET /v3/projects?domain_id=<id>      → only projects in that domain
GET /v3/projects?parent_id=<id>      → only projects in that domain; a project id → []
GET /v3/groups?name=<name>           → only matching groups
GET /v3/services?type=compute        → only compute services
GET /v3/endpoints?interface=public   → only public endpoints
```

`parent_id` is a domain because there is no project hierarchy: every project is top-level, and upstream gives a top-level project its domain as parent.

### F2 — A filter matching nothing yields an empty collection (FR-025)

```
GET /v3/roles?name=no-such-role                → 200 {"roles": []}
GET /v3/users?domain_id=no-such-domain         → 200 {"users": []}
GET /v3/endpoints?service_id=no-such-service   → 200 {"endpoints": []}
```

MUST NOT be the unfiltered collection. This is the specific failure being fixed.

A value that is not a UUID, filtering a `uuid` column (`domain_id`, `service_id`), names nothing either. It MUST NOT reach the column, where Postgres refuses the cast and the caller gets a `500`.

### F3 — Unfiltered requests are unchanged (FR-026)

```
GET /v3/roles       → identical to today, in content and ordering
GET /v3/users       → identical to today
GET /v3/projects    → identical to today
GET /v3/groups      → identical to today
GET /v3/services    → identical to today
GET /v3/endpoints   → identical to today
```

### F4 — Unknown filters are ignored, not refused

```
GET /v3/roles?bogus=1   → 200, the full collection
```

Consistent with the existing domain collection, which ignores parameters it does not implement.

### F5 — The network subnet collection is untouched (FR-027)

```
GET /v2.0/subnets?network_id=<id>   → unchanged by this feature
```

Same bug class, owned by #546's B6. Fixing it here would produce a conflicting change in two branches.

### F6 — The client-side workaround becomes unnecessary

`test/multi_tenant_test.sh` currently resolves the member role with `jq '.roles[] | select(.name=="member")'`. After this change, `GET /v3/roles?name=member` returns exactly that role server-side, and removing the client-side selection MUST NOT change the suite's result.
