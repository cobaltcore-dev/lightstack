# Contract: block-storage quota enforcement (US3, FR-018 – FR-029a)

**Surface**: every path that adds to a project's block-storage consumption
**Today**: nothing on any path consults a quota. The limits and usage are reported and never enforced.

## Enforced resources

All six the quota views report, plus extension:

| Resource | Consumed by |
|---|---|
| `volumes` | Every volume-adding path |
| `gigabytes` | Volume size **and snapshot size** (FR-026), and the delta of an extension |
| `per_volume_gigabytes` | A single volume request, independently of the project's remaining total |
| `snapshots` | Snapshot creation |
| `backups` | Backup creation |
| `backup_gigabytes` | Backup creation |

## Enforcement points

| Path | Resources checked |
|---|---|
| `POST /v3/{project}/volumes` | `volumes`, `gigabytes`, `per_volume_gigabytes` |
| `POST /v2.1/servers` with a new root volume (compute) | same |
| Backup restore into a new volume | same |
| Managed-volume import | same |
| Transfer accept | `volumes`, `gigabytes` |
| `os-extend` volume action | `gigabytes`, for the size **added** |
| `POST /v3/{project}/snapshots` | `snapshots`, `gigabytes` |
| `POST /v3/{project}/backups` | `backups`, `backup_gigabytes` |

**A check on the volume API alone is insufficient** — four routes add a volume, and three of them do
not go through the volume-create handler.

## Refusal

```
POST /v3/{project}/volumes   {"volume": {"size": 10}}
→ 413 {"overLimit": {"message": "Quota exceeded for resource: gigabytes", …}}
```

- `413` with code `overLimit`, naming the exhausted resource — the same answer compute's quota
  refusals already give.
- **Nothing is left behind**: no volume record, no backing store, no server implying a volume.
- **A transfer refused for quota stays pending** (FR-019a) — the volume remains with its sender and
  the transfer can be accepted again once the limit is raised. "Leaves nothing behind" means no *new*
  state for a create and no *lost* state for a transfer.
- **A denial is recorded** as an operator-visible event naming project, resource, limit and requested
  amount (FR-029a). The client-error refusals elsewhere in this feature are not logged.

## Invariants

1. **Race-free.** Two concurrent creations against a single remaining allowance yield exactly one
   volume. The project's quota row is locked for the duration of the check.
2. **Enforcement and reporting agree.** Both read one limit-and-usage resolution, including the code
   defaults used when a project has no configured row (FR-025, FR-026).
3. **Immediate effect.** A limit raised by an administrator applies to the next request, with no
   restart.
4. **Under-limit behaviour is unchanged** from today.
5. **Existing overage is not corrected.** A project already past its allowance keeps what it has;
   further additions are refused and nothing is deleted.

## Evidence required

Refusal at the limit and success below it **for each enforced resource**, plus refusal through the
compute boot path — the one no handler-level test can see — plus a concurrency test (FR-029).
