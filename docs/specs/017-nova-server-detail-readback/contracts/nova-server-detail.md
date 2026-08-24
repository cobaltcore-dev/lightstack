# Contract: Nova server-detail `key_name` + `security_groups`

**Feature**: `017-nova-server-detail-readback` | Nova API **v2.1** (OpenStack Flamingo 2025.2)

Wire shapes below are the authoritative acceptance target. All were verified against the upstream
Nova api-ref during `/cl.clarify`. Existing fields are unchanged; only `key_name` and
`security_groups` are added to the two detail responses, and `security_groups` is added to the
create request.

---

## C1 — `GET /v2.1/servers/{id}` (show) response — added fields

```jsonc
{
  "server": {
    "id": "…", "name": "…", "status": "ACTIVE",
    // …existing fields unchanged…
    "key_name": "my-key",                       // NEW: string, or null when no keypair
    "security_groups": [ { "name": "default" } ] // NEW: array of {name}; never empty
  }
}
```

- `key_name`: `string` when the instance was booted with a keypair; **`null`** otherwise (field
  always present).
- `security_groups`: array of objects each with a single `name` key. Reflects the instance's
  current group membership; **`[{"name":"default"}]`** when the instance has none.

## C2 — `GET /v2.1/servers/detail` (list-detail) response — added fields

Each element of `servers[]` carries the **same** `key_name` and `security_groups` fields, with
identical semantics to C1.

## C3 — `POST /v2.1/servers` (create) request — accepted field

```jsonc
{
  "server": {
    "name": "…", "flavorRef": "…", "imageRef": "…",
    "key_name": "my-key",                          // already parsed today
    "security_groups": [ { "name": "web" } ]        // NEW: now parsed + persisted
  }
}
```

- `security_groups`: optional array of `{name}` (Nova shape). When omitted, the project's `default`
  group is ensured (created if absent) and the association is **persisted**, so the instance reads
  back `[{"name":"default"}]` from a real row (C1). Read-side synthesis of `[{"name":"default"}]`
  applies only to instances with no surviving association — created before migration `063`, or
  whose groups were all deleted since (the join drops orphans).

## C4 — Create-time validation errors (Nova envelope)

```jsonc
// unknown key_name  → HTTP 400, no instance created
{ "badRequest": { "message": "Invalid key_name provided.", "code": 400 } }

// unknown NON-DEFAULT security group → HTTP 400, no instance created
{ "badRequest": { "message": "Security group <name> not found.", "code": 400 } }
// NOTE: the name "default" always resolves — it is ensured (auto-created) per project if absent,
// matching OpenStack's guarantee that every project has a default security group.
```

## C5 — Behavioral contract (the F1 no-op)

Given an unchanged Terraform config that sets `key_pair` and `security_groups`:
`terraform apply` (success) → `terraform plan` **reports 0 changes** (no instance replacement).

---

## Contract tests (Constitution I — real clients)

- **gophercloud** (`test/contract/`): create a server with `KeyName` / `SecurityGroups`, then
  `servers.Get` and assert `server.KeyName` and `server.SecurityGroups` read back through a real
  client (`server_readback_test.go`). List-detail and the two `400` paths are asserted by the Go
  unit suite below, not here.
- **Go unit** (`internal/nova/*_test.go`, DB-gated by `LIGHTSTACK_TEST_DB_URL`): assert the raw JSON of
  `GetServer` and `ListServersDetail` for the with/without-keypair and with/none/unknown-SG matrix,
  plus the two 400 paths.
- **Terraform smoke** (`test/terraform/compat/`): apply → re-plan = 0 changes.
