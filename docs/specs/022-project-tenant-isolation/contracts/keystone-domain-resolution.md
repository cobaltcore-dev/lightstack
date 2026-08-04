# Contract: Keystone domain reference resolution (B5 / P1)

**Feature**: `022-project-tenant-isolation` | **Requirements**: FR-001, FR-001a, FR-002 – FR-008
**Service**: Identity (Keystone) v3, port 35357

## The shared resolver

One function, four call sites (FR-008). Signature is the contract; the callers differ only in the
`state` they pass and in how they translate its outcome.

```
resolveDomainRef(ctx, pool, ref string, state domainState) → (id string, domainOutcome)

  ref == ""          → SELECT id FROM domains WHERE name = 'Default' [AND enabled = true]
  ref == "default"   → same query (the alias)
  uuid.Validate(ref) → SELECT id FROM domains WHERE id = $1 [AND enabled = true]
  otherwise          → notFound, without a query

  [AND enabled = true] only when state == enabledDomainsOnly (project, user and group creation);
  anyDomainState (domain retrieval) resolves a disabled domain too
  outcome ∈ { resolved, notFound, queryFailed }
```

`queryFailed` exists so callers can answer `500` for an unreachable database while answering `400`
(creation) or `404` (retrieval) for a domain that genuinely is not there — the split `CreateUser`
made by hand before this feature (`handlers.go:1007-1021`). `common.ResolveProjectRef` draws the same
line but returns a ready-made `404`/`500` error, which suits its callers and would not suit these:
they disagree on the not-found status.

**The alias is case-sensitive and exact**: `default` resolves; `Default` and `DEFAULT` do not. A domain
*name* is not an accepted reference, so both are refused without a query — `400` on creation, `404` on
retrieval. `--domain Default` still works in clients, because they resolve the name themselves through
`GET /v3/domains?name=Default` and send the UUID.

---

## `POST /v3/projects`

**Request** (unchanged shape):

```json
{"project": {"name": "probe", "domain_id": "default", "description": "", "enabled": true}}
```

| `domain_id` | Outcome | Status | Body |
|---|---|---|---|
| `"default"` | resolved to the Default domain | `201` | `project.domain_id` = the stored **UUID** (FR-007) |
| omitted / `""` | resolved to the Default domain | `201` | as above |
| a UUID of an existing enabled domain | resolved | `201` | that UUID |
| a UUID of a **disabled** domain | not found | `400` | `badRequest`, message names the domain field |
| a UUID naming no domain | not found | `400` | as above |
| any other string | not found, no query issued | `400` | as above |
| valid, but `name` already taken | — | `409` | `conflict`, message names the duplicate project name |
| valid, database unreachable | query failed | `500` | `computeFault` (unchanged) |

**Ordering guarantee**: the domain is resolved *before* the `INSERT`. A request refused with `400`
creates no project (FR-002, US1 scenario 3).

**Response body is unchanged in every field**, including the `links.self` form. Only the status codes
for the failure cases change, plus the fact that `"default"` now succeeds.

**Duplicate detection**: SQLSTATE `23505` on `projects_name_key` via `common.IsUniqueViolation`, not string
matching (research R3).

**Not changed**: no role gate is added (A6/FR-023). Any authenticated caller may still create a
project, as today.

---

## `POST /v3/users`

Behaviour is unchanged except that the alias now resolves.

| `domain_id` | Before | After |
|---|---|---|
| `"default"` | `400 "specified domain not found or disabled"` | `201` |
| omitted | `201` (default domain, resolved by name) | `201`, unchanged |
| unknown UUID | `400 "specified domain not found or disabled"` | `400`, unchanged |
| disabled domain | `400`, same message | `400`, unchanged |

**Preserved deliberately**:
- the administrator-only gate (`handlers.go:972`) runs first, so a non-admin still gets `403` before any
  domain lookup;
- the existing `400` message text, so no client or test that matches on it breaks;
- the existing duplicate-user `409` path, including its string-matching implementation — out of scope
  for this feature (research R3).

---

## `POST /v3/groups`

Before this feature the handler stored whatever reference it was given: `groups.domain_id` was a
`VARCHAR(255) DEFAULT 'default'` with no foreign key (migration `010`), so the alias was kept verbatim
(`201`), a string naming no domain was accepted (`201`), and one domain could hold the same group name
twice — once under the alias, once under its id. It adopts the resolver with the same contract as
`POST /v3/projects`, including the duplicate-name clause — scoped to the domain: `groups` carries
`UNIQUE(name, domain_id)` (migration `010`), so the same name in another domain is legitimate, and a
collision within one is detected via `common.IsUniqueViolation` instead of surfacing as a `500`.
Migration `072` turns the column into a `UUID` referencing `domains(id)` and normalises the alias rows,
so the database enforces the reference too.

| `domain_id` | Status |
|---|---|
| `"default"` / omitted / valid UUID | `201` |
| unknown, disabled, or malformed | `400`, message names the domain field |
| valid, but `name` already taken in that domain | `409`, `conflict`, message names the duplicate group name |

---

## `POST /v3/roles` — deliberately unchanged

`CreateRole` (`handlers.go:658`) parses `domain_id` and discards it — the `roles` table has no such
column. There is no failure mode, so there is nothing to fix. Stated here so the omission reads as a
decision rather than an oversight (A3).

---

## `GET /v3/domains/{domain_id}`

**New**: the alias resolves (FR-001a).

| Path | Before | After |
|---|---|---|
| `/v3/domains/default` | `404 "Could not find domain: default."` (from `uuidParamMiddleware`) | `200`, the Default domain |
| `/v3/domains/<uuid of existing>` | `200` | `200`, unchanged |
| `/v3/domains/<uuid, no such domain>` | `404` Keystone-verbatim | `404`, unchanged |
| `/v3/domains/not-a-uuid` | `404` from the middleware | `404` from the **handler**, same verbatim body |

**Mechanism** (research R4): the route moves from the `val` group to the `mgmt` group, so
`uuidParamMiddleware` no longer pre-empts it, and `GetDomain` resolves the alias then validates the id
itself. The verbatim 404 body — `Could not find domain: <id>.` — is preserved for every non-alias
malformed value, so #515's contract holds for everything except the one alias.

**Unchanged and still on the validating group**: `PATCH /v3/domains/:id`, `DELETE /v3/domains/:id`,
`GET /v3/domains/:id/config`. The alias is accepted only where a client needs it to resolve a
`--domain` argument.

**Response body unchanged**: `{"domain": {"id": "<stored UUID>", "name": "Default", "enabled": true,
"description": …}}`. The alias is never echoed back (A2, FR-007).

---

## `GET /v3/domains`

**New**: the `name` query parameter filters (FR-001a). Today the handler issues an unfiltered
`SELECT … ORDER BY name ASC` and ignores every query parameter, so a client's name lookup silently
receives *all* domains.

| Request | Before | After |
|---|---|---|
| `/v3/domains` | all domains | all domains, unchanged |
| `/v3/domains?name=Default` | **all domains** | exactly the one domain named `Default` |
| `/v3/domains?name=default` | all domains | **empty list** — names are case-sensitive |
| `/v3/domains?name=nope` | all domains | empty list |
| `/v3/domains?enabled=true` | all domains | all domains — other filters remain unsupported (out of scope) |

**Response shape unchanged**: `{"domains": [ … ]}`, always a list, `[]` when empty (never `null`).

---

## Why this matters for the standard client

The ticket's acceptance command is `openstack project create --domain default probe`. The client
resolves `--domain` **before** it sends the creation request: it retrieves the domain by the given
reference, then falls back to a name lookup. Today the retrieval `404`s and the name lookup is
unfiltered, so the command works only because the client ultimately posts the literal string — which is
the path that produces the observed `500`.

After this contract, all three legs work: retrieval by alias resolves, the name filter filters, and the
creation request accepts the alias. The command stops depending on a fallback surviving future client
versions (SC-001a).

---

## Test obligations

| Case | Level |
|---|---|
| Resolver: alias, empty, valid UUID, unknown UUID, disabled, malformed, database error | unit (`domain_resolve_test.go`) |
| `POST /v3/projects`: alias → `201`; unknown → `400`; disabled → `400`; duplicate → `409`; omitted → default domain | handler unit, DB-gated |
| `POST /v3/users`: alias → `201`; admin gate still precedes (non-admin → `403`) | handler unit, DB-gated |
| `POST /v3/groups`: alias → `201`; unknown → `400` | handler unit, DB-gated |
| `GET /v3/domains/default` → `200`; `GET /v3/domains/not-a-uuid` → verbatim `404` from the handler | handler unit + route-wiring test (no DB for the wiring case) |
| `GET /v3/domains?name=Default` → one; `?name=default` → none | handler unit, DB-gated |
| Responses still carry the stored UUID | `test/contract/keystone/` (existing, must stay green) |
| `openstack project create --domain default` and `openstack domain show default` | host pass (quickstart) |
