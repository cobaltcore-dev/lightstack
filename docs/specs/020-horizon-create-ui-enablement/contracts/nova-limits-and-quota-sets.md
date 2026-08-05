# Contract — Nova limits & quota sets (F2, F7)

**Service**: Compute (Nova) v2.1, port 8774 | **Base**: catalog advertises `http://<host>:8774/v2.1` (no project suffix)
**Requirements**: FR-001, FR-002, FR-002a, FR-003, FR-004, FR-006

---

## C1. `GET /v2.1/limits` — absolute limits (existing, **values change**)

Horizon's Overview widget and the Launch Instance action gating read this.

**Response `200`** — `{"limits": {"rate": [], "absolute": { … }}}`

| Key | Source | Change |
|---|---|---|
| `maxTotalInstances` | limits helper → `quotas.instances` | **10** (was 100) |
| `maxTotalCores` | limits helper → `quotas.cores` | **20** (was 200) |
| `maxTotalRAMSize` | limits helper → `quotas.ram` | **51200** (was 512000) |
| `maxTotalKeypairs` | helper → `keypairs` | unchanged (100) |
| `maxServerGroups`, `maxServerGroupMembers` | helper | unchanged (10, 10) |
| `maxTotalFloatingIps`, `maxSecurityGroupRules` | helper | unchanged (10, 100) |
| `maxSecurityGroups` | helper → `quotas.security_groups` | **10** (was 50) |
| `maxServerMeta`, `maxImageMeta`, `maxPersonality`, `maxPersonalitySize` | constants | unchanged |
| `totalInstancesUsed`, `totalCoresUsed`, `totalRAMUsed` | live query, every instance row | **was `status != 'DELETED'`** → every row, as C2 counts |
| `totalFloatingIpsUsed` | `floating_ips` count | **was hardcoded `0`** → real (FR-004) |
| `totalSecurityGroupsUsed` | `security_groups` count | **was hardcoded `0`** → real (FR-004) |
| `totalServerGroupsUsed` | `server_groups` count | **was hardcoded `0`** → real (FR-004) |

**Rules**
- Limits resolve through the **single package helper** (defaults ⊕ `quotas` rows). The local map at
  `internal/nova/handlers.go:1957` that shadows the package `quotaDefaults` with 10× values is deleted.
- Target project = the caller's JWT `project_id`. *(Upstream also accepts `?tenant_id=` for admins; adding it
  is optional and only if the trace shows Horizon sends it.)*
- A usage query that fails answers `500` — never zero usage (`TestUsageFailureIs500`).

---

## C2. `GET /v2.1/os-quota-sets/{project_id}` — quota set (existing, **values change**)

**Response `200`** — `{"quota_set": {"id": "<project>", "<resource>": <limit>, …, "<resource>_used": <n>, …}}`

**Rules**
- Limits come from the same helper as C1 → **C1 and C2 report identical limits** (the FR-003 invariant).
- Usage keys align with C1: add the missing `snapshots_used` and `security_group_rules_used`; C1 and C2 both
  count every instance row (`TestLimitsCountEveryInstanceRow`).
- Target-project resolution per C5.

---

## C3. `GET /v2.1/os-quota-sets/{project_id}/detail` — **NEW** (FR-002)

Currently **404**. Upstream reports per-resource objects.

**Response `200`**

```json
{"quota_set": {
  "id": "<project_id>",
  "instances": {"in_use": 2, "limit": 10, "reserved": 0},
  "cores":     {"in_use": 4, "limit": 20, "reserved": 0},
  "ram":       {"in_use": 4096, "limit": 51200, "reserved": 0}
}}
```

**Rules**
- Same resource set and same limit values as C2 (helper-sourced); `in_use` from the usage queries;
  `reserved` is always `0` (Lightstack has no reservation model — Art. II, synchronous).
- Route must reuse the `:id` param name (gin panics otherwise).

---

## C4. `GET /v2.1/os-quota-sets/{project_id}/defaults` — defaults (existing, **values change**)

**Response `200`** — same shape as C2, **code defaults only**: no `quotas` table read, no usage keys.
Now emitted from the shared default map instead of a hardcoded literal, so it cannot drift from C1–C3.

---

## C5. Authorization (FR-002a) — applies to C2, C3, C4

| Caller | URL project id | Result |
|---|---|---|
| non-admin | own project (or `default`) | `200`, own quota |
| non-admin | **another project** | `403`, refused before any lookup. `defaults` excepted: `200`, as upstream's `RULE_ANY` allows |
| `admin` role | any project | `200`, that project's quota |

Implemented with `common.QuotaTargetProject` (`internal/common/project.go:84`), which checks the shared
`common.HasRole` helper (`internal/common/roles.go:20`) — **not** a fourth copy of Nova's hand-rolled role
loop. The `default` → caller's-project alias is preserved and now lives in that helper (`project.go:88`).

---

## C6. `PUT /v2.1/os-quota-sets/{project_id}` — check ordering (FR-020)

Already admin-only, but the handler **binds the request body before checking the role**
(`internal/nova/quotas.go:156-160` precede `:162-174`), so a non-admin sending a malformed body gets `400`
instead of `403` — the authorization outcome leaks a request-validation result.

**Required**: the admin check runs **first**.

| Caller | Body | Today | Required |
|---|---|---|---|
| non-admin | malformed | `400` | **`403`** |
| non-admin | valid | `403` | `403` (unchanged) |
| admin | malformed | `400` | `400` (unchanged) |

Deliberate behavior change (exception to FR-016), tested. While here, the hand-rolled loop at `:162-174` is
replaced by `common.HasRole` per FR-019.

---

## Not in this contract

- **`GET /v2.1/extensions` is deliberately NOT added** — upstream Nova removed the extensions API at
  microversion 2.36, so serving it would diverge from Flamingo (Art. I).
- `DELETE /os-quota-sets/{id}` and quota-class sets — absent upstream-wise but not needed by any create UI;
  out of scope unless the trace proves otherwise.
- Quota **enforcement** on create paths — unchanged (`CheckQuota` untouched); this feature is read-surface only.

## Test assertions (FR-015)

1. C1 and C2 report the **same** limit for `instances`, `cores`, `ram` (fails today: 100 vs 10).
2. C1 reports upstream defaults 10 / 20 / 51200 for a project with no `quotas` rows.
3. A `quotas` row override for `instances` is reflected in **both** C1 and C2.
4. C3 returns `200` with `{in_use, limit, reserved}` per resource (fails today: 404).
5. C4 returns `200` with defaults and **no** usage keys.
6. C1's `totalFloatingIpsUsed` / `totalSecurityGroupsUsed` / `totalServerGroupsUsed` reflect seeded rows, not `0`.
7. C5: a non-admin requesting another project's id is not served that project's data; an admin is.
8. C6: a non-admin `PUT` with a **malformed** body returns **403**, not 400 (fails today).
