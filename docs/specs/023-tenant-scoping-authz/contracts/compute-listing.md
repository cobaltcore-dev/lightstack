# Contract: Cross-tenant listing

**Feature**: `023-tenant-scoping-authz` | Covers FR-005 – FR-006, User Story 1

Actors as in `compute-override.md`. Project **A** owns 2 servers, project **B** owns 3.

---

## L1 — Default view is unchanged

```
GET /v2.1/servers            X-Auth-Token: ADMIN    → 200, exactly A's 2 servers
GET /v2.1/servers/detail     X-Auth-Token: ADMIN    → 200, exactly A's 2 servers
GET /v2.1/servers            X-Auth-Token: MEMBER_A → 200, exactly A's 2 servers
GET /v2.1/servers            X-Auth-Token: UNSCOPED → 200, no servers (never 500)
```

Holding `admin` MUST NOT widen a listing on its own. This is the compatibility guarantee: every existing client sees exactly what it sees today.

## L2 — Explicit opt-in widens, for an administrator

```
GET /v2.1/servers?all_tenants=1        X-Auth-Token: ADMIN → 200, all 5 servers
GET /v2.1/servers?all_tenants=true     X-Auth-Token: ADMIN → 200, all 5
GET /v2.1/servers?all_tenants=True     X-Auth-Token: ADMIN → 200, all 5
GET /v2.1/servers?all_tenants=yes      X-Auth-Token: ADMIN → 200, all 5
GET /v2.1/servers?all_tenants=on       X-Auth-Token: ADMIN → 200, all 5
GET /v2.1/servers?all_tenants=y        X-Auth-Token: ADMIN → 200, all 5
GET /v2.1/servers?all_tenants          X-Auth-Token: ADMIN → 200, all 5   ← bare form
GET /v2.1/servers?all_tenants=         X-Auth-Token: ADMIN → 200, all 5   ← empty value
```

The bare and empty-value forms are the ones Horizon sends; `strconv.ParseBool` handles neither.

```
GET /v2.1/servers?all_tenants=0        X-Auth-Token: ADMIN → 200, exactly A's 2
GET /v2.1/servers?all_tenants=false    X-Auth-Token: ADMIN → 200, exactly A's 2
GET /v2.1/servers?all_tenants=no       X-Auth-Token: ADMIN → 200, exactly A's 2
GET /v2.1/servers?all_tenants=off      X-Auth-Token: ADMIN → 200, exactly A's 2
GET /v2.1/servers?all_tenants=banana   X-Auth-Token: ADMIN → 200, exactly A's 2   ← unrecognised
```

## L3 — Non-administrator opt-in is ignored, not refused (FR-005b)

```
GET /v2.1/servers?all_tenants=1   X-Auth-Token: MEMBER_A → 200, exactly A's 2 servers
```

The response MUST be byte-identical to `GET /v2.1/servers` with the same token. A `403` is a contract violation: it reveals the parameter exists and is gated. The answer MUST NOT vary with whether project B has any servers.

## L4 — Entries identify their owner (FR-006)

```
GET /v2.1/servers/detail?all_tenants=1   X-Auth-Token: ADMIN
→ every entry carries tenant_id; the set of distinct tenant_id values is {A, B}
```

## L5 — Paging is stable across projects

With 5 servers and `limit=2`:

```
GET /v2.1/servers?all_tenants=1&limit=2                     → page 1: [s1, s2]
GET /v2.1/servers?all_tenants=1&limit=2&marker={s2}         → page 2: [s3, s4]
GET /v2.1/servers?all_tenants=1&limit=2&marker={s4}         → page 3: [s5]
```

The union of the three pages MUST be all 5 servers, with no repeats and no omissions — including when several servers share a `created_at` value, which is the case this guards.

A marker naming a server in another project MUST resolve under `all_tenants` rather than silently restarting at page 1.

## L6 — Marker outside the visible set

```
GET /v2.1/servers?limit=2&marker={SRV_B}   X-Auth-Token: MEMBER_A
```

MEMBER_A cannot see `SRV_B`. The answer MUST NOT depend on `SRV_B` existing, and MUST NOT silently return page 1 as though no marker were given.

```
→ 400   marker [{SRV_B}] not found
```

The same `400` answers a marker that names no server at all, or is not a server id, as upstream does for an unknown marker. The detail listing behaves identically.
