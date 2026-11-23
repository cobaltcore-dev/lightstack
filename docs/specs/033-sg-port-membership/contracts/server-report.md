# Contract: a server's `security_groups`

`GET /v2.1/servers/{id}` and `GET /v2.1/servers/detail`. The brief listing (`GET /v2.1/servers`) has no
`security_groups` and is unchanged.

> **Round 2** (2026-11-20): group names may repeat within a project (FR-026, `network-api-round2.md`), so two groups of
> one name on a port contribute an entry each, as upstream. A name several of the caller's groups share answers `409`
> (`NoUniqueMatch`) where a server action resolves a group by name (`compute-security-groups.md`).

## Shape

```json
"security_groups": [{"name": "default"}, {"name": "web"}]
```

- Each port of the server contributes one entry per group it carries. Ports are taken in creation order, then by id; a
  port's groups are taken by name.
- An entry is `{"name": <the group's name>}`, or the group's id when the name is empty.
- Duplicates stay: a server with two ports in `default` reports `default` twice, as upstream.
- The key is **absent** when no port carries a group: a server without ports, a server whose last group was removed, or
  a server from a release up to v0.6.1.
- `[{"name": "default"}]` is reported only when a port carries `default`. It is never invented.

## Before and after

| Server | Before | After |
|---|---|---|
| Created with `web` | `[{"name":"web"}]` from the server's list | `[{"name":"web"}]` from its port |
| Created by v0.6.1 or earlier: ports without a group, nothing recorded | `[{"name":"default"}]`, invented | no `security_groups` key |
| Last group removed | not possible (`400`) | no `security_groups` key |
| Two ports, both in `default` | `[{"name":"default"}]` | `[{"name":"default"},{"name":"default"}]` |
| An interface attached on a network to a server in `web` | `[{"name":"web"}]`, while the new port had no group | `[{"name":"web"},{"name":"default"}]` |
| A port's groups changed through `PUT /v2.0/ports` | not reflected | reflected |

## `GET /v2.0/ports?device_id=<server>`

Each port's `security_groups` lists the ids its filter is built from. This answer is unchanged; it is what the tests
compare the server's report against.
