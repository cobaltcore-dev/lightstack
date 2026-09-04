# Phase 1 Data Model: Readiness/liveness

This feature persists **nothing** — no tables, no migrations. The "model" is the in-memory health state of one process. (Entities map to the spec's Key Entities.)

## Entities (in-memory, `internal/health`)

### Check (a named component check)
| Field | Type | Notes |
|-------|------|-------|
| `Name` | string | stable identifier, e.g. `database` (becomes the failing-check name in `/readyz` output) |
| `Probe` | `func(ctx) error` | bounded check; returns nil = pass, error = fail. For `database`: wraps `database.HealthCheck(ctx)` |
| `Timeout` | duration | per-check sub-second bound (Principle III); the Checker wraps the call in `context.WithTimeout` |

Initial registry (FR-001, R1): exactly one gating check — **`database`**. The registry is a slice so more checks *could* be added later, but none gate readiness in this feature (Q1).

### Checker (the aggregator)
| Field | Type | Notes |
|-------|------|-------|
| `checks` | `[]Check` | the gating component checks |
| `started` | `atomic.Bool` | the "all HTTP listeners bound" gate (R8); false until `main` finishes binding all 7 listeners |
| `draining` | `atomic.Bool` | set true on SIGTERM (R9); forces not-ready while the pod drains |

Behavior:
- `Ready(ctx) (ok bool, failed []string)` — returns `false` + `["starting"]` if `!started`; `false` + `["draining"]` if `draining`; else runs each check concurrently under its timeout and collects the **names** of failures. Total wall-time bounded by the max per-check timeout (sub-second) — never hangs (FR-003, SC-003).
- `Live() bool` — always true while the process can execute the handler (no deps, FR-002).

### Readiness state (derived, not stored)
The aggregate verdict consumed by the HTTP layer and logged on transition (FR-015):
| Field | Type | Notes |
|-------|------|-------|
| `ready` | bool | `Ready()` verdict |
| `reason` | string | short curated reason when not ready (e.g. `database unavailable`, `starting`, `draining`) — **no** error text/latency/stack trace (Q3) |
| `failed` | []string | failing check names (for `?`-less verbose / JSON body) |

The previous verdict is held in-process only to detect **transitions** (ready↔not-ready) for the structured log line (FR-015). Not persisted.

### Liveness state (derived)
Just "process responding." No fields, no deps (FR-002).

## State transitions (readiness)

```
                 all 7 listeners bound          DB Ping ok
   starting ───────────────────────▶ (started) ──────────▶ READY
      │                                  │  ▲                 │
      │ (boot grace exceeded,            │  │ DB Ping ok      │ DB Ping fails  /  SIGTERM
      │  startupProbe restarts pod)      │  └─────────────────┤
      ▼                                  │                    ▼
  CrashLoopBackOff                       └──────────────  NOT-READY (reason: database unavailable | draining)
```

- **starting → started**: `main` finishes `net.Listen` on all 7 ports (R8).
- **started ↔ READY**: each readiness poll runs the DB `Ping`; flips on result. Runtime DB outage → NOT-READY, **no restart** (liveness is `/livez`, R4/US2).
- **boot-time DB never up**: startup probe keeps failing → kubelet restarts (CrashLoopBackOff) (R4/Q6).
- **any → draining (NOT-READY)**: SIGTERM sets `draining`; pod leaves the Service before listeners close (R9).

## Configuration (chart values → probe wiring; no app config schema change)

Probe knobs live in `values.yaml` under `probes:` (rendered into `lightstack.podSpec`), validated by `values.schema.json` (draft-07). Per probe (`startup`/`readiness`/`liveness`): `enabled`, `path`, `periodSeconds`, `timeoutSeconds`, `failureThreshold`, `successThreshold`, plus `initialDelaySeconds` where applicable; defaults chosen so SC-002 (~30 s detect), SC-004 (~60 s restart), SC-005 (~150 s boot grace) hold. Plus a drain delay (`terminationGracePeriodSeconds` / optional preStop) for SC-006. Concrete defaults are set in `/cl.tasks`.
