# Contract — Neutron quotas & extensions (F2, F3)

**Service**: Networking (Neutron) v2.0, port 9696 | **Base**: catalog advertises `http://<host>:9696`
**Requirements**: FR-001, FR-002, FR-002a, FR-004, FR-005, FR-009, FR-013

> Neutron has **no quota table** in Lightstack (only `quotas` and `cinder_quotas` exist) and does not gain one.
> Limits are code defaults from **one shared map**; usage is counted live. This is sufficient for UI
> enablement (spec Assumption: read surface, not enforcement).

---

## C1. `GET /v2.0/quotas/{project_id}` — quota (existing, **shape changes**)

Horizon reads this when gating the Project-panel **Create Network** action.

**Response `200`**

```json
{"quota": {
  "network": 100, "subnet": 100, "port": 500, "router": 10,
  "floatingip": 50, "security_group": 100, "security_group_rule": 500,
  "project_id": "<project_id>", "tenant_id": "<project_id>"
}}
```

**Changes**
- **Emit `project_id` *and* `tenant_id`** (both present upstream — verified via the api-ref). Today the
  handler ignores `c.Param("id")` entirely and returns a bare hardcoded map.
- **Resolve and honor the target project** per C5 (today: no scoping of any kind).
- Limits come from the **single shared defaults map** so C1/C2/C3 cannot drift.

---

## C2. `GET /v2.0/quotas/{project_id}/details` **and** `…/details.json` — details (existing + **new alias**)

**Response `200`**

```json
{"quota": {
  "network":             {"limit": 100, "used": 2, "reserved": 0},
  "subnet":              {"limit": 100, "used": 2, "reserved": 0},
  "port":                {"limit": 500, "used": 3, "reserved": 0},
  "router":              {"limit": 10,  "used": 0, "reserved": 0},
  "floatingip":          {"limit": 50,  "used": 0, "reserved": 0},
  "security_group":      {"limit": 100, "used": 1, "reserved": 0},
  "security_group_rule": {"limit": 500, "used": 2, "reserved": 0}
}}
```

**Changes**
- **Register the `.json` form as an alias to the same handler** (FR-002). Only `/details` exists today;
  a client that appends `.json` gets a 404.
- Add `reserved: 0` to each entry (upstream shape is `{limit, used, reserved}`; Lightstack emits `{limit, used}`).
- `security_group_rule.used` is currently hardcoded `0` → count it for real (FR-004).
- Target project per C5.

---

## C3. `GET /v2.0/quotas/{project_id}/default` (+ `.json`) — **NEW** (FR-002)

Currently **404**.

**Response `200`** — same shape as C1 (code defaults; no usage), from the shared defaults map.

**Routing note**: `/quotas/:id/default` is a static child of the existing `:id` param — safe. A bare
`/quotas/default` would be ambiguous with `:id`, so if ever needed it is handled *inside* the `:id` handler
(the idiom now shared by all three services in `common.QuotaTargetProject`, `internal/common/project.go:88`).

---

## C4. `GET /v2.0/extensions` — advertise `quota_details` (FR-013)

**Change**: add one entry to the existing six.

```json
{"alias": "quota_details",
 "name": "Quota details management support",
 "description": "Expose functions for quotas usage statistics per project",
 "updated": "2017-02-10T10:00:00-00:00",
 "links": []}
```

Also add the upstream `links: []` field to the six existing entries.

**Why**: Horizon reads network usage via the quota **detail** call when this alias is advertised, and
otherwise falls back to counting resources across several list calls. Advertising it puts network usage on
**one** endpoint Lightstack fully controls — fewer failure modes for the Overview widget (F2) and the Create-Network
gate (F3). Confirmed via Context7 `/openstack/neutron`: the Quota Detail extension exposes per-project
`used`/`limit`/`reserved`.

**Only genuinely-supported aliases are advertised** — Lightstack does not claim `external-net`, `dvr`,
`subnet_allocation`, etc., which it does not implement.

---

## C5. Authorization & project resolution (FR-002a) — applies to C1, C2, C3

| Caller | URL project id | Result |
|---|---|---|
| non-admin | own project | `200`, own quota/usage |
| non-admin | **another project** | `403` on show, default and details, refused before any lookup |
| `admin` role | any project | `200`, that project's quota/usage |

Implemented with `common.HasRole(c, "admin")`. **Neutron has no role check anywhere today**, so this helper
is introduced to the package fresh (rather than hand-rolling a loop).

---

## Not in this contract

- `PUT` / `DELETE /v2.0/quotas/{id}` and `GET /v2.0/quotas` (admin list) — absent today; no create UI needs
  them. Out of scope unless the trace proves otherwise.
- A Neutron quota **table** — out of scope (no migration in this feature).

## Test assertions (FR-015)

1. C1 returns `200` including `project_id` **and** `tenant_id` (fails today: both absent).
2. C3 (`/default`) returns `200` with the default limits (fails today: 404).
3. `…/details.json` returns the same body as `…/details` (fails today: 404).
4. C1, C2, C3 report the **same** limit per resource (drift guard).
5. C4 lists `quota_details` among the advertised aliases (fails today).
6. C2's `security_group_rule.used` reflects seeded rules, not `0`.
7. C5: a non-admin is not served another project's usage; an admin is.
8. C1/C2/C3 resolve their target project, and C2 counts usage, so these tests need `LIGHTSTACK_TEST_DB_URL`;
   only the C4 extension listing runs without a database.
