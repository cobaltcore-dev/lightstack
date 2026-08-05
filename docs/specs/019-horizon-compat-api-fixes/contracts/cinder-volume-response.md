# Contract — Cinder volume response (F9)

**Service**: Block Storage (Cinder) v3. **Clients**: Horizon volume table, python-openstackclient, gophercloud.

## Endpoints

- `GET /v3/{project_id}/volumes/{id}` — Show volume details
- `GET /v3/{project_id}/volumes/detail` — List volumes with details
- `POST /v3/{project_id}/volumes` — Create volume (response uses the same shape)
- `GET /v3/{project_id}/types/default` — Show default volume type

## Contract

Every volume object in the show, list-detail, and create responses MUST contain the following keys, each
**present and non-null** with the given type (matching the Flamingo Cinder v3 volume schema):

```
id, name, description, status, size, bootable(string "true"/"false"),
created_at, updated_at, attachments(array), metadata(object),
volume_type(string), volume_type_id(string), availability_zone(string), encrypted(bool),
multiattach(bool), consumes_quota(bool), shared_targets(bool),
user_id(string), "os-vol-tenant-attr:tenant_id"(string),
"os-vol-host-attr:host"(string)
```

- `volume_type` = the type the volume was created with, else `__DEFAULT__`; `volume_type_id` agrees
  with it (O3K does not keep a separate type UUID).
- `description` comes from the `volumes.description` column (migration 012).
- `availability_zone` = `"nova"`; `encrypted`=false; `multiattach`=false; `consumes_quota`=true;
  `shared_targets`=false; `metadata`={} when none.
- Defaults for unmodeled fields are constants (O3K does not model AZ/encryption/multiattach/host).
- `GET /v3/{project_id}/types/default` MUST return `name`=`id`=`__DEFAULT__` (agrees with `volume_type`).

Additionally, these keys MUST be **present with a `null` value**, matching what upstream Cinder itself
returns (verified against the v3.65 api-ref volume response). O3K does not model them, but a client that
dereferences a missing key raises where `null` merely renders empty:

```
consistencygroup_id, migration_status, replication_status, snapshot_id, source_volid
```

## Behavioral guarantees

- The three responses are byte-consistent for the shared fields (one serializer).
- Rendering: a Horizon Volumes panel with ≥1 volume returns HTTP 200 (no `TemplateSyntaxError`); an empty
  panel still renders.

## Contract-test assertions

1. `volume show <id>` (created with `--type foo`) → `volume_type == "foo"`; created with no type →
   `volume_type == "__DEFAULT__"`.
2. `volume show` and `volume list --long` → all keys above present and non-null.
3. `types/default` → `{"id":"__DEFAULT__","name":"__DEFAULT__"}`.
4. Regression: existing keys (`id/name/size/status/bootable/attachments`) unchanged in value/shape.
