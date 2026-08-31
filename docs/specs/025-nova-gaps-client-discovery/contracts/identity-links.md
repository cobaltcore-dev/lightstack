# Contract: identity self-links (US2 / B16)

**Covers**: FR-010 – FR-014
**Applies to**: `GET /v3/projects/{id}`, `/v3/users/{id}`, `/v3/roles/{id}` and the collections behind
`project show` / `user show` / `role show` **by name**

## Why the collections matter as much as the reads

`openstack <resource> show <name>` resolves the name through a **collection** query first. The 6.6.0
client then does an unconditional `pop('links')` on the resulting object. So a missing link in a *list
entry* is what breaks `role show <name>` even though `GetRole` already emits one.

## Shape

```jsonc
{"project": {
  "id": "...", "name": "...", "description": "...", "enabled": true, "domain_id": "...",
  "links": {"self": "http://<host>:35357/v3/projects/<id>"}    // absolute, with scheme
}}
```

## Sites

| Handler | File:line | Change |
|---|---|---|
| `GetProject` | `keystone/handlers.go:495` | **add** links |
| `GetUser` | `keystone/handlers.go:421` | **add** links |
| `ListUsers` entries | `:404` | **add** |
| `ListProjects` entries | `:477` | **add** |
| `ListRoles` entries | `:706` | **add** |
| `ListAuthProjects` entries | `:354` | **add** — keystoneauth reads this during scope discovery |
| `ListUserProjectRoles` entries | `:926` | **add** |
| `GetUserProjects` entries | `:1393` | **add** |
| `CreateProject`, `UpdateProject`, `GetRole`, +2 | `:588, :656, :739, :778, :825` | **fix** — scheme-less |
| `groups.go` | `:47, :121, :156, :221` | **fix** — scheme-less |

## Constraints

- FR-012: derived by the **same** rule as US1. No second implementation in the identity service.
- FR-013: no other field changes. `description` and `domain_id` are nullable and already rendered via
  `.String` so the response shape stays stable — adding a link must not disturb that.

## Test obligations

- Unit per handler: `links.self` present, absolute, has a scheme.
- Contract (`test/contract/keystone`): a name-based show goes through the collection and finds a link.
- Integration on **6.6.0** (`which -a openstack` first): all three `show` commands exit 0. Then re-run
  SPEC-023's real-mode fixture block and confirm `PROJ_A`/`PROJ_B`/`USER_A`/`USER_B` are non-empty —
  that silent degradation is the reason B16 bites the project's own runbooks.
