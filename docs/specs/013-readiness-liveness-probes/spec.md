# Feature Specification: Readiness/liveness endpoints driving k8s-native heartbeating

**Feature Branch**: `013-readiness-liveness-probes` (git: `feature/475-…`)  
**Created**: 2026-06-29  
**Status**: Draft  
**Input**: cloud-storage#475 — "[FEAT] Readiness/liveness endpoints driving k8s-native heartbeating"

## Overview

An operator deploying Lightstack needs a trustworthy answer to one question: *is it running and healthy?* Today the only endpoints a probe can hit are static version routes that return success even when the database is down, so "healthy" can be a lie. This feature makes Lightstack expose a **thin, honest readiness signal** and a **liveness signal**, and lets Kubernetes do the heartbeating natively — polling, restarting, gating traffic, and aggregating the result into the standard **Pod `Ready` condition** that operators and downstream automation already know how to read. Lightstack builds no custom status protocol; it only tells the truth about whether it can serve.

## Clarifications

### Session 2026-06-30

- Q: Which of lightstack's dependencies should flip its readiness signal? → A: Database reachability + lightstack's own HTTP listeners only — cache/Redis, Ceph/S3, and libvirt outages never affect readiness (per-backend health deferred to Tier-2 per AC6).
- Q: Which port(s) serve the readiness/liveness endpoints? → A: The identity listener (port 35357) only — the single canonical probe target; not mirrored on other API ports or the metadata port, to minimize unauthenticated surface.
- Q: How much does the unauthenticated readiness endpoint reveal? → A: A short curated reason only (e.g., `database unavailable`) / names of failing checks — no error text, connection details, latencies, or stack traces. Mirrors the OpenStack `oslo.middleware`/Octavia healthcheck default; the detailed/stack-trace mode is out of scope (off-by-default, access-restricted).
- Q: Also expose OpenStack's conventional `/healthcheck` path? → A: Yes — add `/healthcheck` as an OpenStack-compatible alias of the readiness check (same DB-gated behavior + short reason), on the identity port, alongside the Kubernetes `/readyz`/`/livez` probe targets.
- Q: Tier-2 rich `/healthz` diagnostics — in or out? → A: Out of scope — deferred to a separate ticket per AC6; #475 stays thin (only the per-component reason on readiness, FR-010).
- Q: What does the startup probe gate on, and what if the DB never comes up at boot? → A: Startup probe targets readiness (DB-gated) with a generous grace (~150s); if the DB never connects at boot the container restarts (CrashLoopBackOff is acceptable for an unmet boot-time dependency). Once started, liveness is DB-independent, so a runtime outage flips readiness but never restarts (US2 preserved).
- Q: How deep is the database readiness check? → A: Reuse `database.HealthCheck` (pgx pool `Ping` — a server round-trip bounded by the sub-second timeout); no explicit `SELECT 1`, write, or not-in-recovery probe (stays within the thin-signal budget, FR-013).
- Q: Should the endpoints be reachable externally, or restricted? → A: Chart serves them unauthenticated on the identity port; operators are advised (per OpenStack guidance) to restrict external access at their LB/Ingress. The chart adds no per-path deny rules (controller-specific, brittle); in-cluster kubelet probing is unaffected.
- Q: What observability does the readiness state emit? → A: A structured log line on each ready↔not-ready transition (with the cause); no lightstack-exposed metrics endpoint — the scrapable signal is the platform-native Pod `Ready` condition via `kube-state-metrics`.
- Q: HTTP methods / content negotiation for `/healthcheck`? → A: `/healthcheck` answers GET + HEAD and content-negotiates text/plain + JSON by `Accept` (oslo/Octavia-faithful); the k8s probes `/readyz`/`/livez` stay minimal (status code + plain text).
- Q: Support an oslo-style manual disable (file-based drain)? → A: No — rely on k8s-native draining (cordon / pod deletion / scale-down + the FR-009 shutdown drain); readiness stays an honest reflection of DB + listeners, with no operator side-channel toggle.

## User Scenarios & Testing *(mandatory)*

### User Story 1 - Honest readiness gates traffic (Priority: P1)

An operator deploys lightstack. It is reported **Ready** only once it can genuinely serve — its database is reachable and all of its HTTP service listeners are accepting connections. While the database is unreachable, Lightstack is reported **not-ready** and the platform routes no client traffic to it.

**Why this priority**: This is the core defect the feature fixes. Without it, every other guarantee is built on a dishonest signal. A correct readiness signal alone is a viable MVP: deployments stop reporting healthy-but-broken.

**Independent Test**: Deploy Lightstack with the database down; confirm it never reports Ready and receives no traffic. Bring the database up; confirm it reports Ready and serves. Take the database down on a running instance; confirm it flips to not-ready and is pulled from traffic routing.

**Acceptance Scenarios**:

1. **Given** Lightstack is starting with an unreachable database, **When** the platform polls readiness, **Then** readiness reports not-ready and the instance receives no client traffic.
2. **Given** Lightstack is running and the database is reachable, **When** the platform polls readiness, **Then** readiness reports ready and the instance serves client traffic.
3. **Given** a running, Ready instance, **When** the database becomes unreachable, **Then** readiness flips to not-ready and the instance is removed from traffic routing.
4. **Given** the database recovers, **When** the platform next polls readiness, **Then** the instance returns to Ready and is restored to traffic routing.

---

### User Story 2 - Liveness recovers a wedged process without crash-looping on outages (Priority: P2)

If the Lightstack process becomes unresponsive (deadlocked/wedged), the platform restarts it automatically. Crucially, a **database outage does not** trigger restarts — losing a dependency must not cause a restart loop, because restarting fixes nothing and makes the outage worse.

**Why this priority**: Liveness is half of "k8s-native heartbeating," but it is only safe once readiness (US1) correctly separates "can't serve right now" from "process is broken." Getting the liveness/readiness split wrong turns a database blip into a cluster-wide crash loop.

**Independent Test**: Wedge the process; confirm it is restarted within the expected window. Separately, take the database down for an extended period; confirm the process is **not** restarted (only readiness toggles).

**Acceptance Scenarios**:

1. **Given** the Lightstack process is unresponsive, **When** the platform polls liveness repeatedly, **Then** the platform restarts the instance.
2. **Given** the database is unreachable for an extended period, **When** the platform polls liveness, **Then** liveness continues to report healthy and the process is not restarted.

---

### User Story 3 - Operators consume the native health state (Priority: P2)

An operator — a person at a terminal, a `--wait`/`wait` gate, or a future controller — determines whether Lightstack is up and healthy using **only standard platform mechanisms** (the Pod `Ready` condition and its roll-ups), with no lightstack-specific status protocol to learn or maintain.

**Why this priority**: This is the operator's actual goal and the integration contract downstream consumers depend on. It is delivered as a consequence of US1/US2 being wired into the platform correctly, plus reconciling the published contract.

**Independent Test**: Using only standard platform tooling (no custom client), gate a deploy on Lightstack becoming healthy and confirm it blocks until Ready and fails on timeout; confirm a watcher observing the Pod `Ready` condition sees the same transitions as US1.

**Acceptance Scenarios**:

1. **Given** Lightstack is deploying, **When** an operator waits on the native readiness condition, **Then** the wait blocks until Lightstack is genuinely serving and fails if it never becomes Ready within the timeout.
2. **Given** a downstream consumer reads health, **When** it inspects lightstack, **Then** the authoritative signal is the native Pod `Ready` condition, not a bespoke Lightstack status endpoint.

---

### User Story 4 - Resilient startup and zero-downtime rollout (Priority: P3)

A slow initial database connection at startup does not cause Lightstack to be killed before it has had a fair chance to come up. During a rolling update, an instance stops accepting new traffic **before** it stops serving, so in-flight and newly routed requests are not dropped.

**Why this priority**: Operational polish that materially improves real deployments, but the feature delivers value without it.

**Independent Test**: Introduce a slow database connect at startup and confirm no premature restart within the grace window. Perform a rolling update under load and confirm zero dropped requests.

**Acceptance Scenarios**:

1. **Given** the database takes a long time to accept connections at startup, **When** Lightstack is within its startup grace window, **Then** Lightstack is not restarted for not-yet-being-ready.
2. **Given** a rolling update is in progress under client load, **When** an instance is being replaced, **Then** it reports not-ready and drains before terminating, and no client request is dropped.

---

### User Story 5 - Diagnosing an unhealthy instance (Priority: P3)

When an instance is not-ready, an operator can tell **which component** caused it directly from the readiness signal, and can query the health signals from the command line for local diagnosis.

**Why this priority**: Speeds incident response, but is not required for the core guarantee.

**Independent Test**: Force a not-ready state and confirm the readiness response names the failing component; run the command-line health query and confirm it reflects the same state with a meaningful exit status.

**Acceptance Scenarios**:

1. **Given** Lightstack is not-ready because the database is unreachable, **When** an operator inspects the readiness signal in detail, **Then** the response identifies the database as the failing component.
2. **Given** an operator on the host/command line, **When** they query lightstack's health, **Then** they receive the current readiness/liveness state with a success/failure exit status.

### Edge Cases

- **Database hangs (not just refuses)**: the readiness check must time out quickly and report not-ready; the health signal must never block or hang.
- **Database flapping**: readiness reflects current state; rapid flaps toggle only readiness/traffic routing and never trigger restarts.
- **Unauthenticated probing**: the platform cannot present credentials, so readiness and liveness must be reachable without authentication.
- **Partial startup**: if the database is reachable but not all HTTP service listeners are yet accepting connections, the instance must report not-ready until all are up.
- **Shutdown**: on termination the instance must report not-ready promptly so traffic drains before it stops serving.
- **Multi-node**: in a multi-node deployment each node's instance reports its own health independently; one unhealthy node does not mark others unhealthy.
- **Non-database backend down**: if a non-gating backend (cache/Redis, Ceph/S3, libvirt) is unavailable while the database is reachable, the instance stays Ready and in traffic — these do not gate readiness (their health is a Tier-2 concern).
- **Database never reachable at boot**: the startup check keeps failing past its grace and the container restarts (CrashLoopBackOff) until the database appears — distinct from a *runtime* outage after a successful start, which flips readiness but never restarts.

## Requirements *(mandatory)*

### Functional Requirements

- **FR-001**: Lightstack MUST expose an **unauthenticated readiness signal** that reports not-ready until its database is reachable AND all of its HTTP service listeners are accepting connections, and reports ready only while it is genuinely serving. The readiness gate is limited to these two inputs; other configured backends (cache/Redis, Ceph/S3, libvirt) MUST NOT affect readiness.
- **FR-002**: Lightstack MUST expose an **unauthenticated liveness signal** that reports healthy whenever the process is running and responsive, **independent of** database or other external-dependency availability.
- **FR-003**: The readiness and liveness checks MUST complete within a short, bounded time (sub-second, consistent with lightstack's fail-fast principle) and MUST never hang, even when a checked dependency is unresponsive.
- **FR-004**: When readiness reports not-ready, the platform MUST stop routing client traffic to that instance **without restarting it**.
- **FR-005**: When liveness reports unhealthy, the platform MUST restart the instance.
- **FR-006**: Lightstack MUST tolerate a slow initial database connection during startup for a bounded grace period without being restarted (the startup check gates on readiness). If the database never becomes reachable within that grace, the platform MAY restart the instance — a restart / `CrashLoopBackOff` loop is acceptable for an unmet boot-time dependency. Once startup has succeeded, liveness no longer depends on the database (FR-002), so a later runtime outage flips readiness but never restarts the instance (FR-005, US2).
- **FR-007**: The machine-readable health state consumed by operators and downstream automation MUST be the **platform-native readiness condition** (the Kubernetes Pod `Ready` condition and its roll-ups), NOT a custom status protocol or bespoke status endpoint.
- **FR-008**: The deployment artifact (Helm chart) MUST configure readiness, liveness, and startup checks against these signals for **both single-node and multi-node** deployment modes.
- **FR-009**: During graceful shutdown and rolling updates, Lightstack MUST report not-ready **before** it stops accepting requests, so in-flight and newly routed requests are not dropped.
- **FR-010**: When readiness reports not-ready, an operator MUST be able to determine **which component** caused the failure from the readiness signal itself, without standing up a separate diagnostics service. The unauthenticated response MUST expose only a **short, curated reason / the names of failing checks** (e.g., `database unavailable`); it MUST NOT expose error text, connection details, latencies, or stack traces. (Mirrors the OpenStack `oslo.middleware`/Octavia healthcheck default; any detailed/stack-trace mode is out of scope and would be off-by-default and access-restricted.)
- **FR-011**: Operators MUST be able to **query the health signals from the command line** (mirrors lightstack's existing CLI subcommand pattern, e.g. `lightstack cleanup`).
- **FR-012**: Operators MUST be able to **tune health-check timing and thresholds** through deployment configuration, and invalid configuration MUST be rejected before deployment.
- **FR-013**: The readiness signal MUST remain a **thin, low-overhead** check — it must not perform expensive or wide-ranging diagnostics (the honest-signal principle).
- **FR-014**: Lightstack MUST also expose the OpenStack-conventional unauthenticated **`/healthcheck`** path as an alias of the readiness check (same database-gated behavior and short curated reason as FR-001/FR-010), served on the identity port, so OpenStack-native load balancers and monitoring work unchanged. `/healthcheck` MUST answer **GET and HEAD** and content-negotiate **text/plain and JSON** by `Accept` (matching oslo/Octavia). The Kubernetes probes (`/readyz`, `/livez`) stay minimal — status code + plain text, since the kubelet reads only the status code.
- **FR-015**: Lightstack MUST emit a structured log line on each readiness state transition (ready↔not-ready), including the cause. It MUST NOT expose a separate metrics endpoint for health — the scrapable readiness signal is the platform-native Pod `Ready` condition (e.g., via `kube-state-metrics`).

### Key Entities

- **Readiness state**: whether Lightstack can currently serve. Composed of the **database-reachability check** plus the **HTTP-listeners-bound** state — the only inputs that gate readiness in this feature — each with a pass/fail status and, when failing, a brief reason. Aggregates to a single ready / not-ready verdict.
- **Liveness state**: whether the process is running and responsive; carries no external-dependency status.

## Success Criteria *(mandatory)*

### Measurable Outcomes

- **SC-001**: 100% of deployments report Ready only after the instance can serve real API requests backed by a reachable database (zero false-ready).
- **SC-002**: When the database becomes unreachable, the instance is reported not-ready and removed from traffic routing within 30 seconds, and receives zero client requests until it recovers.
- **SC-003**: A health check returns a result within 1 second even when the database is unresponsive (the check never hangs).
- **SC-004**: An unresponsive/deadlocked process is automatically restarted within 60 seconds without operator intervention.
- **SC-005**: A slow initial database connection of up to ~150 seconds at startup does not cause the instance to be restarted.
- **SC-006**: During a rolling update under client load, zero client requests are dropped.
- **SC-007**: An operator can determine whether Lightstack is running and healthy using only standard platform mechanisms — no custom client or protocol — in 100% of cases.
- **SC-008**: When an instance is not-ready, an operator can identify the responsible component in under 1 minute using the health signal alone.
- **SC-009**: A database outage never causes a restart loop (zero restarts attributable to dependency unavailability).

## Assumptions

- **Topology / AC2 decision (RESOLVED — Q2)**: Lightstack runs as a single container exposing all of its service listeners in one process; platform probes target one container. The readiness and liveness signals are served on the **identity listener (port 35357) only** — the single canonical probe target, already the chart's probe anchor and the integration contract's entry point. They are **not** served on the other API listeners or the metadata listener, keeping the unauthenticated surface minimal.
- **Endpoint naming**: Kubernetes naming for the probes (`/readyz`, `/livez`) plus the OpenStack-conventional `/healthcheck` alias for readiness (FR-014); exact paths are configurable.
- **Default timing**: probe periods, failure thresholds, the ~150 s startup grace, and the sub-second check timeout are reasonable defaults; exact values are finalized during planning and are tunable by the operator.
- **Tier-2 diagnostics out of scope (AC6, RESOLVED — Q5)**: a rich aggregated multi-component JSON health/diagnostics surface is **not** built here; it is deferred to a separate ticket. This spec includes only a thin per-component cause on the readiness signal (FR-010).
- **Database ownership**: the external database is operator-owned and unchanged; this feature adds no schema, migration, or persisted data.
- **Reuse / DB check depth (Q7)**: the readiness DB check reuses lightstack's existing `database.HealthCheck` (a pool `Ping` — a protocol round-trip that proves the database answers, not a TCP-only check), bounded by the sub-second timeout. No explicit `SELECT 1` or write / not-in-recovery probe — that would exceed the thin-signal budget (FR-013).
- **DB check vs. Postgres operator**: readiness performs lightstack's own client-side database reachability check rather than relying on an external PostgreSQL operator's readiness. The operator reports the database's health; only Lightstack can report whether it can actually reach and use the database (network, DNS, credentials, connection pool, or a runtime outage after startup). The two signals are complementary.
- **External exposure (Q8)**: the endpoints are unauthenticated and served on the identity port, so they are reachable wherever that port is exposed. Operators are advised (mirroring OpenStack's guidance) to restrict `/readyz`/`/livez`/`/healthcheck` at the external LB/Ingress; the chart does not add per-path deny rules (ingress-controller-specific and brittle). In-cluster kubelet probing via the pod IP is unaffected. Briefly caching the readiness result to blunt DB-round-trip amplification is a plan-level option.

## Dependencies

- Kubernetes deployment via the existing Helm chart (single-node Deployment and multi-node DaemonSet).
- The existing database connectivity check that this feature wires to a network-reachable signal.
- The integration contract status conventions (cloud-storage#4 / SPEC-012), which must be reconciled so the published readiness anchor reflects the new honest signal instead of the static version route.

## Out of Scope

- A Tier-2 rich JSON health/diagnostics service with per-service / cache / storage-backend breakdown (separate ticket per AC6).
- Gating readiness on any non-database backend (cache/Redis, Ceph/S3, libvirt) — a backend outage does not remove the pod from traffic.
- Any custom/bespoke status protocol or push-based heartbeat (the standard OpenStack `/healthcheck` alias of FR-014 is not such a protocol).
- Changes to the database schema or to authenticated API behavior.
- Chart-enforced Ingress/LB rules blocking external access to the health endpoints — restricting external exposure is the operator's responsibility (documented), not enforced by the chart.
- A lightstack-exposed Prometheus/metrics endpoint for health state — the platform-native Pod `Ready` condition (via `kube-state-metrics`) is the scrapable signal.
- An oslo-style `disable_by_file` / manual readiness toggle — maintenance draining uses k8s-native mechanisms (cordon / delete / scale-down), not an app-internal side-channel.
