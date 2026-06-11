# Phase 1 Contracts: SPEC-008 Helm Charts

The chart exposes **three observable contracts**: (C1) the values input + install-time gates, (C2) the rendered Kubernetes resources per mode, and (C3) the post-install verification. The chart changes **no OpenStack API surface** — OpenStack CLI/Terraform/Horizon compatibility is the product's contract (SPEC-000) and is reused unchanged against any chart-deployed release.

---

## C1 — Install-time input contract (fail-fast)

`helm install`/`helm upgrade` MUST fail with a clear message (never start insecure) when:

| Condition | Result |
|-----------|--------|
| `image.repository` empty | `fail` — no implicit public image (FR-018) |
| no token signing secret (value or `existingSecret`) | `fail` — no insecure default (FR-007) |
| no admin password (value or `existingSecret`) | `fail` — no `admin/secret` (FR-007a) |
| no database URL (value or `existingSecret`) | `fail` — external DB is a hard prerequisite (FR-006) |
| `tls.enabled` but no cert/issuer | `fail` — don't advertise https without termination (Edge Cases) |
| `deploymentMode=multi-node` + real + local Glance/Cinder backend | `fail`/hard-warn (FR-012e) |
| `cache.enabled` + multi-node + non-shared Redis | warn (FR-006a) |

Enforced by `values.schema.json` (types/enums/required at install time) + `_helpers.tpl` `fail` guards. A valid minimal install supplies only: image, database URL, token secret, admin password.

---

## C2 — Rendered-resource contract (per mode)

Every mode permutation MUST `helm template` cleanly. Expected resources:

**single-node / stub (the validated MVP):**
- `Deployment` (`replicaCount` allowed, **unprivileged**, no hostNetwork/hostPath → passes `restricted` PSA)
- `Secret` carrying the rendered `o3k.yaml` (full config incl. credentials; unless `existingSecret`), `Service` (`ClusterIP`, 7 ports), optional `Ingress`
- `Pod` (helm test hook). **No** privileged/hostPath/PSA-privileged objects.

**single-node / real:** as above but `Deployment` `replicas=1` + `strategy: Recreate`, `privileged`, `hostNetwork`, hostPath mounts (libvirt socket + host storage), optional `privileged` namespace label.

**multi-node:** `DaemonSet` (nodeSelector = designation label) replacing the Deployment; `neutron.vxlan_enabled: true` (in the rendered config); single front-door `Service` load-balancing all instances; metadata reached node-locally (not via the Service). *(End-to-end behavior deferred — see C2-note.)*

> **C2-note (2026-06-11 clarification):** real-mode and multi-node resources are authored and render/lint-clean now, but their runtime behavior depends on the product post-conditions (WS-1/4/5/6, issue [#52](https://github.com/cobaltcore-dev/o3k/issues/52)). Their e2e success criteria (SC-003, SC-004, real-VM login/floating-IP/restart) are gated on those landing.

---

## C3 — Post-install verification contract (`helm test` — FR-015)

`helm test <release>` runs a short-lived Job that:
- resolves the in-cluster identity endpoint and issues `GET /v3` (and optionally `openstack token issue` with the seeded admin),
- asserts a successful identity response,
- reports pass/fail on demand without leaving residue.

This is the operator-runnable confirmation that the identity service answers correctly (Acceptance Scenario US1-3).

---

## Verification mapping

| Contract | Verified by |
|----------|-------------|
| C1 (gates) | helm-unittest gate cases + `helm install --dry-run` failure assertions |
| C2 (render) | `helm template` snapshots for every mode + helm-unittest manifest assertions |
| C3 (helm test) | `helm test` on a kind/k3s single-node-stub install |
| OpenStack compat | reused SPEC-000 contract tests + CLI/Terraform flows against the deployed release (SC-002, stub subset now; full set when post-conditions land) |

**Deferred (gated on issue #52):** `test/vxlan_multinode_test.sh` (SC-004) and real-mode bash suites; multi-node N-active-in-60s (SC-003).
