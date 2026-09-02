# Contract: image patch operations (US2, FR-010 – FR-017)

**Surface**: `PATCH /v2/images/{image_id}`,
media type `application/openstack-images-v2.1-json-patch` (RFC 6902)
**Today**: only `replace` is applied; everything else is discarded and answered `200` with the
unchanged image.

## Writable paths

`/name`, `/visibility`, `/min_disk`, `/min_ram` — unchanged by this feature. All four are **base**
properties carrying a schema-defined default, which is why none can be removed.

## Operations

| Document | Answer | Effect |
|---|---|---|
| `[{"op":"replace","path":"/min_disk","value":10}]` | `200` | `min_disk` becomes 10 |
| `[{"op":"add","path":"/min_disk","value":7}]` | `200` | `min_disk` becomes 7 — same write as `replace` |
| `[{"op":"remove","path":"/min_disk"}]` | `403` | Nothing changes; the response names the path |
| `[{"op":"replace","path":"/status","value":"active"}]` | `403` | Service-managed property; names the path |
| `[{"op":"move","from":"/a","to":"/b"}]` | `400` | Operation not supported; names the operation |
| `[{"op":"replace","path":"/min_disk","value":"ten"}]` | `400` | Value type is wrong for the property |
| Not an array of operations | `400` | Malformed document |
| Valid document, image not found or not owned | `404` | Affected-row count is checked |

## Invariants

1. **Atomic.** If any operation in the document cannot be applied, none is applied and the image is
   unchanged.
2. **No silent success.** No request is answered `200` without writing what it accepted. In
   particular, an `UPDATE` affecting zero rows is `404`, not `200`.
3. **The response reflects the applied state** of the image.
4. **`add` and `replace` are the same write** for these four scalar properties.

## Evidence required

Each operation pinned by a test that **reads the property back**. A test asserting only the response
status passes against today's discard behaviour and is not acceptable evidence (FR-017).
