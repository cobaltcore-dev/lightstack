# Contract — Neutron network list filters (F4)

**Service**: Network (Neutron) v2.0. **Clients**: Horizon Project → Networks (`network_list_for_tenant`),
python-openstackclient, gophercloud.

## Endpoint

- `GET /v2.0/networks` — List networks (with query filters)

## Contract

The handler MUST honor these query filters (in addition to the existing `id`, `name`, pagination):

| Filter | Behavior |
|---|---|
| `shared=true` | return only networks with `shared=true` |
| `shared=false` | return only networks with `shared=false` (exclude shared) |
| `router:external=true` | return only external networks — Lightstack models none, so an **empty** list |
| `router:external=false` / absent | no external constraint |
| `project_id=<id>` / `tenant_id=<id>` | narrow to that project, **intersected** with the caller's authorized scope (never widens: a non-admin cannot see another project's networks) |

Every network object in list/show/create responses MUST include `"router:external": false` (constant; Lightstack
models no external networks). `shared` continues to reflect the stored value.

## Behavioral guarantees

- Horizon's three sub-queries — `tenant_id=<self>&shared=false`, `shared=true`, `router:external=true` — return
  **disjoint** sets, so each network appears **exactly once** in the merged Project panel.
- Unfiltered `GET /v2.0/networks` returns each network exactly once (unchanged ordering/pagination).
- Project isolation preserved (Constitution Art. V).

## Contract-test assertions

Given one project-owned, non-shared network `N`:

1. `?shared=true` → does **not** contain `N`; `?shared=false` → contains `N`.
2. `?router:external=true` → empty; each returned network has `router:external == false`.
3. `?tenant_id=<caller>` → contains `N`; `?tenant_id=<other>` as a non-admin → does not expose other projects.
4. Merge of the three Horizon sub-queries → `N` appears once (no duplicate).
5. Unfiltered list → `N` appears once.
