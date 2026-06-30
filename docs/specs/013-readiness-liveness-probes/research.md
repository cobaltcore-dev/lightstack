# Phase 0 Research: Readiness/liveness endpoints

All spec `NEEDS CLARIFICATION` were resolved by the 11 clarifications (spec `## Clarifications`, 2026-06-30). This file consolidates the decisions and confirms the load-bearing technical facts the design depends on. Format: **Decision · Rationale · Alternatives**.

## R1 — What gates readiness (Q1)
- **Decision**: readiness = DB reachable **AND** all HTTP listeners bound. Cache/Redis, Ceph/S3, libvirt never affect it.
- **Rationale**: ticket scopes it to DB + listeners ("thin, honest"); per-backend health is Tier-2 (AC6). A storage blip shouldn't yank a still-serving API from traffic.
- **Alternatives**: gate on all configured backends (too heavy, false-NotReady on intentional read replicas / optional backends); WARN-only on backends (needless complexity).

## R2 — Probe port topology (Q2)
- **Decision**: serve only on the identity listener (35357); the kubelet `httpGet` targets one port.
- **Rationale**: least unauthenticated surface; identity is already the chart's probe anchor + contract entry point. Mirroring to other ports adds surface with no probe benefit.
- **Alternatives**: all API ports (rejected — surface, no benefit); include metadata (rejected — leaks health to guest VMs).

## R3 — pgx `Pool.Ping` semantics (Q7)
- **Decision**: reuse `database.HealthCheck` (`internal/database/db.go:92`), which calls `pgxpool.Pool.Ping(ctx)`.
- **Rationale**: pgx `Ping` acquires a pooled connection and performs a **protocol round-trip to the server** (not a mere TCP connect), so it proves the DB *answers*. Bounded by a sub-second `context.WithTimeout`, an unresponsive/hung DB returns a deadline error fast → 503, never hangs. This is the thinnest honest check (FR-013).
- **Alternatives**: explicit `SELECT 1` (Octavia) — duplicates what `Ping` already does; write/not-in-recovery probe — exceeds thin budget and false-negatives on read replicas.

## R4 — Startup vs readiness vs liveness semantics (Q6)
- **Decision**: `startupProbe → /readyz` (DB-gated) with a generous grace; `livenessProbe → /livez` (no deps); `readinessProbe → /readyz`.
- **Rationale**: kubelet semantics — while the startup probe runs, liveness/readiness are suppressed; on success they take over; on startup-probe failure past `failureThreshold` the container is **restarted**. Pointing startup at `/readyz` gives a slow DB connect time to succeed; if the DB never appears at boot the pod CrashLoopBackOffs (acceptable for an unmet boot dependency). Because post-startup **liveness is `/livez` (DB-independent)**, a *runtime* DB outage flips readiness but never restarts (US2). This asymmetry (boot crash-loop vs runtime no-restart) is intentional and standard.
- **Alternatives**: `startupProbe → /livez` (DB-down-at-boot → Running+NotReady forever, no restart) — more consistent with US2 but contradicts "startupProbe for slow DB connect"; no startup probe (liveness could kill a slow boot).

## R5 — Unauthenticated exposure detail + OpenStack precedent (Q3, Q4, Q10)
- **Decision**: coarse `200 "OK"` / `503 "<short reason>"` (e.g. `database unavailable`) + failing-check names; **no** error text/latency/stack traces. `/healthcheck` is an oslo/Octavia-compatible alias answering GET+HEAD and negotiating text/JSON by `Accept`. The detailed/stack-trace mode is out of scope (would be off-by-default + access-restricted).
- **Rationale**: verified OpenStack precedent — `oslo.middleware` `/healthcheck` is unauthenticated, returns `OK`/`503 <reason>` by default; **Octavia's DB-check plugin** runs a query and returns `503 "The … database is unavailable."` for *exactly* this "200-while-DB-down" problem; oslo's `detailed` mode exposes stack traces and is "highly recommended not to enable." So the curated-reason default is the established convention.
- **Alternatives**: fully coarse (A — weaker FR-010 diagnosis); full detail (C — leaks internals, against OpenStack guidance).
- **Sources**: oslo.middleware healthcheck plugins doc; Octavia API Health Monitoring admin doc.

## R6 — Observability (Q9)
- **Decision**: structured log on each readiness transition (with cause); **no** o3k-exposed metrics endpoint.
- **Rationale**: readiness is already scrapable at the platform layer via `kube-state-metrics` (`kube_pod_status_ready`) — the k8s-native path; a self-exposed metric duplicates it and pulls in a metrics subsystem o3k lacks.
- **Alternatives**: o3k Prometheus gauge (redundant + new subsystem); nothing (loses the cheap, high-value transition log).

## R7 — External exposure (Q8)
- **Decision**: serve unauthenticated on the identity port; **document** that operators restrict external access at their LB/Ingress; chart adds no per-path deny rules.
- **Rationale**: mirrors OpenStack's own guidance ("restrict access on your external load balancer"); per-path Ingress deny rules are controller-specific and brittle; the kubelet probes the pod IP in-cluster regardless. A short cache of the readiness result can blunt DB-round-trip amplification (plan-level).
- **Alternatives**: chart-enforced deny rules (brittle, controller-specific); leave fully open (mild amplification + info exposure).

## R8 — "All listeners bound" gate (FR-001)
- **Decision**: in `main.go`, create each of the 7 servers' listeners with `net.Listen("tcp", addr)` **synchronously up-front** (binds immediately, fails fast on a port clash), then start `srv.Serve(ln)` goroutines and flip a process-level atomic `ready` flag. `/readyz` returns 503 until the flag is set.
- **Rationale**: today `go srv.ListenAndServe()` binds *inside* the goroutine, so "are all listeners up?" is unknowable at flag-flip time. Binding up-front makes it deterministic and preserves fail-fast (a port clash aborts startup immediately). Once any server serves, all are bound.
- **Alternatives**: flag-after-`go` (race — bind may not have happened); per-listener readiness introspection (over-engineered for one process).

## R9 — Graceful drain (FR-009, SC-006)
- **Decision**: on `SIGTERM`, set `ready=false` first, sleep a configurable drain delay (lets the EndpointSlice controller remove the pod), then run the existing `srv.Shutdown(ctx)`. Pair with chart `terminationGracePeriodSeconds` (and optional `lifecycle.preStop` sleep) so the kubelet waits out the drain.
- **Rationale**: readiness removal from a Service is asynchronous; flipping not-ready and pausing before closing listeners prevents dropped in-flight/newly-routed requests during rollouts. Matters most for scalable stub-mode deployments (real mode pins 1 replica, Recreate).
- **Alternatives**: immediate shutdown (drops requests mid-rollout); rely only on preStop (still need the readiness flip to leave endpoints).

## R10 — `o3k healthcheck` CLI (FR-011)
- **Decision**: dispatch `o3k healthcheck [--ready|--live] [--addr]` like the existing `o3k cleanup` (intercept `os.Args[1]` before `flag.Parse`); it issues an HTTP GET to `localhost:<identityPort>/readyz` (or `/livez`), prints the reason, exits 0/1.
- **Rationale**: reuses the established subcommand pattern; usable as an exec-probe alternative and for host-side diagnosis without a token.
- **Alternatives**: separate binary (violates single-binary, Principle VI); none (loses local diagnosis affordance).

## R11 — Gin specifics
- **Decision**: register the three routes directly on the Keystone `gin.Engine` (`r`), beside the existing root `r.GET("/")`, **outside** the auth group — so they're unauthenticated. Use `c.Negotiate`/`c.NegotiateFormat` for `/healthcheck` text-vs-JSON; handle `HEAD` (gin routes HEAD; return status only).
- **Rationale**: matches the existing unauth root-version pattern in `createKeystoneServer`; no middleware changes.
- **Alternatives**: a separate health server/port (rejected — Q2 identity-only); middleware-level interception (unnecessary).
