# Contract: `o3k cleanup` command

**Feature**: SPEC-010 | **Surface**: product CLI (the existing single binary) | **Refs**: FR-003/004/005/006/008/010/012/013, R2/R3/R5/R9

The blocking pre-delete hook's purge Job invokes this. It also runs standalone (Compose/bare-metal) for host+DB teardown.

## Invocation

```
o3k cleanup [--config <path>] [--migrations <path>] [--dry-run]
```

- Dispatched in `cmd/o3k/main.go` from `os.Args[1] == "cleanup"`, **before** the server path; the process performs the teardown and exits (it does **not** start HTTP servers).
- `--config` / `--migrations`: same flags/defaults as the server (`config/o3k.yaml`, `migrations`).
- `--dry-run`: enumerate and log what *would* be purged; delete nothing; exit 0.
- **No Kubernetes flags/clients** — quiesce, PVC delete, and Deployment scaling are the chart's kubectl Job, not this command (R1).

## Behavior (order — R3)

1. Load config; connect to the DB (fail-fast like the server **unless** unreachable → see DB-unreachable).
2. **Enumerate** release state from the DB.
3. **Compute**: destroy + undefine each enumerated libvirt domain (`VMManager.DeleteVM`).
4. **Network**: delete each enumerated project/router namespace and its bridges/TAP/iptables/VXLAN.
5. **Storage**: delete node-local volume/image files under the host storage path; sweep orphans (FR-006).
6. **DB reset**: `database.MigrateReset()` — **last** (skipped only if the DB is unreachable).

## Modes (Constitution IV)

- **stub**: no host managers wired → steps 3–5 are no-ops; performs the DB reset only. CI-runnable, unprivileged.
- **real**: full host purge via the privileged managers (needs the host libvirt socket + host mounts + privileges — the purge Job provides them).

## Idempotency & failure (FR-008/016, R9)

- Not-found / already-absent at any step ⇒ **success** for that artifact (idempotent; re-runs are safe).
- **Fail-closed**: exit **non-zero** if a *required class* could not be completed — **compute**, **network**, **node-local storage**, or the **DB reset** (when the DB is reachable). Per-artifact errors within a class are logged but roll up to a class failure.
- **DB unreachable**: **ignore** it — do **not** fail on that account; skip enumeration-from-DB and the reset; purge host state best-effort via host-level enumeration (`light-ns-*`/`qrouter-*` netns, libvirt `ListAllDomains`). (C9)
- Bounded externally by the Job's `activeDeadlineSeconds` (FR-016); the command itself runs to completion or error and does not retry-loop (Constitution III).

## Exit codes

| Code | Meaning |
|-----:|---------|
| 0 | All required classes purged (or `--dry-run`, or nothing to purge). Hook passes. |
| 1 | A required class failed → hook fails → `helm uninstall` aborts (FR-001). |
| 2 | Usage/config error (bad flags, unreadable config). |

## Observability (FR-012)

Structured JSON to stdout: one line per artifact (`purged` / `absent` / `failed` with id + class), a final summary (counts per class, overall result), captured as the Job's logs (the audit trail).
