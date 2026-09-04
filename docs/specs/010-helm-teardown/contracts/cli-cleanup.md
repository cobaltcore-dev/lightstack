# Contract: `lightstack cleanup` command

**Feature**: SPEC-010 | **Surface**: product CLI (the existing single binary) | **Refs**: FR-003/004/005/006/008/010/012/013, R2/R3/R5/R9

The blocking pre-delete hook's purge Job invokes this. It also runs standalone (Compose/bare-metal) for host teardown.

## Invocation

```
lightstack cleanup [--config <path>] [--dry-run]
```

- Dispatched in `cmd/lightstack/main.go` from `os.Args[1] == "cleanup"`, **before** the server path; the process performs the teardown and exits (it does **not** start HTTP servers).
- `--config`: same flag/default as the server (`config/lightstack.yaml`).
- `--dry-run`: enumerate and log what *would* be purged; delete nothing; exit 0.
- **No `--migrations` and no DB write** — the database is operator-owned and is never reset (FR-013, revised 2026-06-24).
- **No Kubernetes flags/clients** — quiesce, PVC delete, and Deployment scaling are the chart's kubectl Job, not this command (R1).

## Behavior (order — R3; revised 2026-06-24: no DB reset)

1. Load config; connect to the operator-owned DB **read-only** to enumerate. If unreachable, ignore it (C9) and fall back to host-only cleanup.
2. **Compute**: destroy + undefine each enumerated libvirt domain (`VMManager.DeleteVM`).
3. **Network**: `neutron.PurgeRealModeNetworkState` — remove all host network state (egress SNAT, floating-IP DNAT + the `/32` on the uplink, ingress SG filters, metadata DNAT, dnsmasq, host bridges, router + tenant namespaces).
4. **Storage**: `os.RemoveAll` every node-local tree under `/var/lib/lightstack` (`instances/`, `cloud-init/`, `images/`, `volumes/`, `dhcp/`); covers orphans (FR-006).

The **database is never written** (operator-owned, FR-013); the global `net.ipv4.ip_forward` sysctl is left as-is.

## Modes (Constitution IV)

- **stub**: no host managers wired → a no-op (nothing to purge); the DB is only read to enumerate. CI-runnable, unprivileged.
- **real**: full host purge via the privileged managers (needs the host libvirt socket + host mounts + privileges — the purge Job provides them).

## Idempotency & failure (FR-008/016, R9)

- Not-found / already-absent at any step ⇒ **success** for that artifact (idempotent; re-runs are safe).
- **Fail-closed**: exit **non-zero** if a *required class* could not be completed — **compute**, **network**, or **node-local storage**. Per-artifact errors within a class are logged but roll up to a class failure. (The operator-owned database is read-only here — never reset — so it is not a purge class; FR-013.)
- **DB unreachable**: **ignore** it — do **not** fail on that account; skip DB enumeration entirely and purge host state best-effort: wholesale removal of the node-local storage trees + deletion of `light-ns-*`/`qrouter-*` namespaces by prefix. Libvirt domains and host bridges/iptables cannot be enumerated without the DB and are left for the operator (logged). (C9)
- Bounded externally by the Job's `activeDeadlineSeconds` (FR-016); the command itself runs to completion or error and does not retry-loop (Constitution III).

## Exit codes

| Code | Meaning |
|-----:|---------|
| 0 | All required classes purged (or `--dry-run`, or nothing to purge). Hook passes. |
| 1 | A required class failed → hook fails → `helm uninstall` aborts (FR-001). |
| 2 | Usage/config error (bad flags, unreadable config). |

## Observability (FR-012)

Structured JSON to stdout: one line per artifact (`purged` / `absent` / `failed` with id + class), a final summary (counts per class, overall result), captured as the Job's logs (the audit trail).
