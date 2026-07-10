# Contract: Health Endpoints

o3k exposes three unauthenticated HTTP endpoints on the **identity listener (`:35357`) only**. No keystone token required. They do not appear on any other listener (Q2). These are infra endpoints, not OpenStack API — they add no microversion and must not shadow existing Keystone routes (`/`, `/v3`).

## `GET /livez` — liveness
| | |
|--|--|
| Auth | none |
| Methods | `GET` (status only) |
| `200` | process is up and the HTTP handler responds. Body: `ok` (text). **Never** depends on the DB or any backend (FR-002). |
| Failure | only by not responding at all (process wedged) → kubelet liveness fails → restart (FR-005) |

## `GET /readyz` — readiness
| | |
|--|--|
| Auth | none |
| Methods | `GET` |
| `200` | `started` (all 7 listeners bound) **and** DB `Ping` ok **and** not draining. Body: `ok` (text). |
| `503` | not ready. Body: a **short curated reason** (text), one of: `database unavailable`, `starting`, `draining`. **No** error text, connection details, latencies, or stack traces (Q3). |
| Timing | DB check bounded sub-second; the endpoint returns within 1 s even if the DB hangs (FR-003, SC-003). |
| Optional verbose | a `verbose` view MAY list failing **check names** only (same non-sensitive constraint). |

Effect: `503` → the EndpointSlice controller removes the pod from the Service (no traffic), **no restart** (FR-004). Recovery → re-added.

## `GET|HEAD /healthcheck` — OpenStack-compatible alias of readiness (FR-014)
| | |
|--|--|
| Auth | none |
| Methods | `GET` **and** `HEAD` |
| Behavior | **alias of `/readyz`** — same DB-gated verdict and short reason |
| Content negotiation | by `Accept` (oslo/Octavia-faithful): `text/plain` → `OK` / `<reason>`; `application/json` → `{"detailed": false, "reasons": ["OK"]}` style body (reason on failure). `HEAD` → status only, no body. |
| `200` / `503` | same conditions as `/readyz` |

Purpose: OpenStack-native load balancers / monitoring that probe `/healthcheck` work unchanged. External exposure is the operator's to restrict at the LB/Ingress (Q8).

## Consumed signal (FR-007 / AC4)
The machine-readable health state downstream consumers (e.g. sci-in-a-box, per cloud-storage#4) read is the **Kubernetes Pod `Ready` condition** — the aggregate of the readiness probe above — and its roll-ups (`Deployment` Available, `helm install --wait`, `kubectl wait --for=condition=Ready`). **Not** a custom o3k status protocol.

## `CONTRACT.md` reconciliation (required)
`charts/o3k/CONTRACT.md:91` currently states the readiness probe is `GET /v3`. It MUST change to `GET /readyz` (DB-gated): "A **Ready** release's API is genuinely serving — the readiness probe is `GET /readyz` on the identity port, which is green only when the database is reachable and all listeners are up." The Pod `Ready` condition remains the consumed signal.

## Probe wiring contract (chart)
`o3k.podSpec` (both Deployment and DaemonSet) MUST set:
- `livenessProbe.httpGet` → `/livez` on `identity`
- `readinessProbe.httpGet` → `/readyz` on `identity`
- `startupProbe.httpGet` → `/readyz` on `identity` (generous `failureThreshold × periodSeconds` ≈ 150 s boot grace)

All with a `timeoutSeconds` bound; all rendered from `values.yaml` and validated by `values.schema.json`.

## Test surfaces (verification)
- **Unit** (`internal/health/health_test.go`, httptest): `/livez`=200 always; `/readyz`=503 when not-started / DB error / DB timeout, 200 when ok; `/healthcheck` GET+HEAD + text/JSON negotiation; reason strings contain no sensitive detail; bounded-timeout never hangs.
- **helm-unittest** (`charts/o3k/tests/probes_test.yaml`): all three probes/paths/ports for Deployment + DaemonSet; defaults render; overrides apply; schema rejects bad values.
- **Integration** (`test/readyz_test.sh`): deploy → `/readyz`=200; stop DB → 503 + pod leaves EndpointSlice; restore → back in.
- **No-regression contract gate** (Principle I): existing gophercloud/`openstack` contract tests still pass (new paths don't shadow Keystone routes).
