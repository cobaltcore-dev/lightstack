# Contract: block storage under reservations

API-visible behaviour of volume create, boot from a new volume and snapshot create after FR-032 – FR-036 (research R13). Same on both backends.

## Creates — unchanged responses

| Request | Response (unchanged) | Database during the request | Database after |
|---|---|---|---|
| `POST /v3/{project}/volumes` (blank or from image) | today's status code and body | a `volumes` row with `status = creating`, `reserved_by = <node>`, visible to `GET`/list | `available` (+ `bootable`, `source_image_id` for an image-backed volume) |
| `POST /servers` with a new boot volume | `202` (Nova) | the boot volume as above | `available`, then attached as today |
| `POST /v3/{project}/snapshots` | `202`, body `status: creating` | a `snapshots` row `creating` | `available` **before** the response (the 100 ms deferred flip is gone) |

Quota: rows in `creating` count towards `volumes`, `gigabytes` and `snapshots` usage, so concurrent creates never take a project past a limit (SC-005). A create whose backing fails removes its row and any partial backing and answers today's error.

## Requests against a `creating` resource — `400`, upstream's messages

| Request | Response |
|---|---|
| `DELETE` a volume in `creating` (non-forced) | `400` `Invalid volume: Volume status must be available or error or error_restoring or error_extending or error_managing and must not be migrating, attached, belong to a group, have snapshots, awaiting a transfer, or be disassociated from snapshots after volume transfer.` |
| `os-extend` a volume in `creating` | `400` `Invalid volume: Volume <id> status must be 'available' to extend, currently creating.` |
| `DELETE` a snapshot in `creating` | `400` `Invalid snapshot: Snapshot status must be available or error and must not be part of a group.` |
| attach a volume in `creating` | `400`, today's message (attach already requires `available`) |
| `os-detach` / `os-force_detach` a volume in `creating` | `400` `Invalid volume: Volume status must be in-use to detach, currently creating.` |
| `os-unmanage` a volume in `creating` | `400`, the volume-delete message above |
| snapshot (`POST /snapshots`) of a volume in `creating` | `400` `Invalid volume: Volume <id> status must be available, but current status is: creating.` |
| back up (`POST /backups`) a volume in `creating` | `400` `Invalid volume: Volume to be backed up must be available or in-use, but the current status is "creating".` |
| manage a snapshot (`POST /os-snapshot-manage`) of a volume in `creating` | `400`, the snapshot message above |

A reservation is visible while its backing is built, so an operation that moved it out of `creating`
would make the create finalise against a changed row and delete its backing from under the surviving
row. The reserving request keeps the backing if, on failure, a row with its id survives in another
status.

Bodies use the existing `badRequest` envelope (`common.NewBadRequestError`).

## After a restart

At start, before its listeners open, Lightstack sets every volume and snapshot that is still `creating` **and** was reserved under its own node name (`LIGHTSTACK_NODE_NAME`, else the host name) to `error`, and reclaims the backing where one exists. A reclaim failure is logged and does not stop startup; deleting the `error` volume later reclaims it. Rows reserved under another node name — another node of a multi-node PostgreSQL deployment — are never touched (SC-010). Rows without a node name (written before migration 074) are never swept.
