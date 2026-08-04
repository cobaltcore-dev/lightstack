# Contract: Cinder project-scoped routes (B2)

**Feature**: `022-project-tenant-isolation` | **Requirements**: FR-014, FR-015, FR-016, FR-016a
**Service**: Block Storage (Cinder) v3, port 8776 | **File**: `internal/cinder/volumes.go`

Cinder exposes every resource twice: a flat form (`/v3/volumes`), which is what O3K's service catalog
advertises, and the project-in-path form (`/v3/{project_id}/volumes`), which is upstream Cinder's
canonical URL and which clients that build their own URLs still reach. The project-scoped group is
missing 19 of the flat form's routes; four are in scope.

## Routes added

| Method | Path | Handler | Today | After |
|---|---|---|---|---|
| `GET` | `/v3/:project_id/volumes` | `ListVolumes` (`volumes.go:412`) | **`405`** — `POST` is registered, `GET` is not | `200` |
| `GET` | `/v3/:project_id/volumes/detail` | `ListVolumesDetail` (`:495`) | matches `GET /volumes/:id` → a lookup for a volume named `detail` | the detailed collection |
| `GET` | `/v3/:project_id/snapshots/detail` | `ListSnapshotsDetail` (`:1153`) | matches `GET /snapshots/:id` → a lookup for a snapshot named `detail` | the detailed collection |
| `GET` | `/v3/:project_id/types/default` | `GetDefaultVolumeType` (`:1204`) | matches `GET /types/:id` → a lookup for a type named `default` | the default volume type |

Four lines. No handler is written, modified, or wrapped — each is the same function the flat route
already uses.

## Why these four and not others: the shadowing test

A missing route and a *shadowed* route fail differently, and only the second answers with the wrong
thing:

- **Absent** — no route matches the path at all → an honest `404`. Unhelpful, but truthful.
- **Shadowed** — a by-identifier sibling is registered at the same level, so the literal name matches
  *it* and the request is served as a lookup for a resource with that name → a wrong answer, or a `500`.

Shadowing therefore requires a sibling `GET /<resource>/:id` **in the same group**. Census of the
`/v3/:project_id` group:

| Sub-resource | `GET /<res>/:id` present? | Total `<res>` routes in the group | Verdict |
|---|---|---|---|
| `volumes/detail` | ✅ | 10 | **shadowed** → in scope |
| `snapshots/detail` | ✅ | 11 | **shadowed** → in scope |
| `types/default` | ✅ | 12 | **shadowed** → in scope |
| `backups/detail` | ❌ | **0** | absent → out of scope |
| `qos-specs/*` | ❌ | 0 | absent → out of scope |
| `os-volume-transfer/detail` | ❌ | 0 | absent → out of scope |

**This corrected the specification.** The specify pass counted four shadowed routes by diffing the flat
and project-scoped route sets — which identifies what is *missing*, not what is *wrong*. `backups` has
no route whatsoever in the project group, so nothing swallows `backups/detail`; it already `404`s
honestly. Consequently the deliberately-deferred count is **15**, not 14.

This is precisely the failure mode #533 F8 cost a debugging cycle for: `GET /v3/groups/detail` matched
`GET /v3/groups/:id`, `GetGroup` returned `500`, and Horizon's Create Volume modal aborted before
rendering — so the button looked inert with nothing in the logs pointing at routing.

## gin registration order is irrelevant

Documented at `volumes.go:112` and verified against the vendored gin v1.12.0: gin's tree splices a
static child ahead of an existing wildcard at the same level, whichever order they are registered in.
Only *presence* matters.

What **does** panic is reusing a different param name at a position already claimed by one — `:tenant_id`
beside an existing `:id`. Every route in this group therefore uses `:id`, and the added routes introduce
no new parameter.

## Scoping guarantee (FR-015)

The `:project_id` path segment is **decoration**. Every handler in that group — all 43 of them today —
reads the caller's project from the token via `c.GetString("project_id")`; not one calls
`c.Param("project_id")`.

| Request | Result |
|---|---|
| `GET /v3/<own project>/volumes` | the caller's volumes |
| `GET /v3/<another project>/volumes` | **still the caller's volumes** |
| `GET /v3/not-a-uuid/volumes` | the caller's volumes — the segment is never parsed |
| `GET /v3/volumes` (flat) | the caller's volumes — identical |

So the added route cannot widen visibility, and it is behaviourally identical to the flat route rather
than a second code path that could drift from it.

## Response bodies

Identical to the flat form's, because the handler is the same object:

| Route | Body |
|---|---|
| `…/volumes` | `{"volumes": [{id, name, links}, …]}` — `[]` when empty, never `null` |
| `…/volumes/detail` | `{"volumes": [ …full attribute set… ]}` |
| `…/snapshots/detail` | `{"snapshots": [ … ]}` |
| `…/types/default` | `{"volume_type": {…}}` |

## The 15 routes recorded as a deliberate gap (FR-016a)

Present on the flat form, absent from the project-scoped group, **not** fixed here — they are absent
rather than wrong, and adding them would ship 15 routes whose only coverage is a parity guard.
Clarification Q2 rejected that (option C).

```
GET    /backups                        GET    /os-availability-zone
GET    /backups/detail                 GET    /os-volume-transfer
GET    /backups/:id                    GET    /os-volume-transfer/detail
POST   /backups                        GET    /qos-specs
POST   /backups/:id/action             GET    /qos-specs/:id
DELETE /backups/:id                    POST   /qos-specs
PUT    /volumes/:id                    PUT    /qos-specs/:id
                                       DELETE /qos-specs/:id
```

To be recorded in the code comment above the group so a later ticket closes them deliberately rather
than by accident.

## Comment maintenance

The comment at `volumes.go:~139-146` currently reads that the one gap in this group — `GET
/v3/{project}/volumes` — *"is filed as a real defect (cloud-storage#536 B2, a 405 that surprised a
client)"*. That sentence becomes false once this lands and must be rewritten to state what is now true:
the ticketed route and the four shadowed sub-resources are registered; 14 routes remain absent by
decision, listed above.

*(Census corrected in the o3k#82 review: `os-volume-transfer/detail` is shadowed by the group's own
`GET /os-volume-transfer/:id`, and had been filed as merely absent — so four, not three, and 14 absent,
not 15. See `research.md` R6.)*

## Test obligations

| Case | Level |
|---|---|
| All four added routes resolve to a handler, not a `404` | extend `TestRouteRegistration_NoWildcardConflict` (`internal/cinder/quotas_test.go:349`) — it guards only flat routes today |
| `RegisterRoutes` still registers without panicking | same test (it calls the real `RegisterRoutes`) |
| `GET /v3/:project_id/volumes` returns the caller's volumes, `[]` when none | handler unit via `newCinderTestRouter` (`volumes_db_test.go:63`), DB-gated |
| A foreign `:project_id` returns only the caller's volumes | handler unit — `X-Test-Project` header |
| The four shadowed paths return their view, not a by-identifier lookup | handler unit |
| `GET /backups/detail` still `404`s on the project form (proving it is absent, not shadowed) | handler unit — guards the R6 finding |
| Live project-scoped volume list with two tenants | `test/multi_tenant_test.sh` (FR-019) |
| Static: `GET /volumes` present in the group | `awk '/v3 := r.Group\("\/v3\/:project_id"\)/,0' internal/cinder/volumes.go \| grep -c 'v3.GET("/volumes",'` → `1` |
