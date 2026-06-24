#!/usr/bin/env bash
# SPEC-010 (issue #53) — teardown scenario: dataRetentionPolicy=delete, single-node stub.
#
# Requires a real cluster — NOT runnable in CI sandboxes without one:
#   a Kubernetes cluster (kind/k3s), kubectl, helm, an external PostgreSQL, an O3K
#   image, a kubectl image, and psql on PATH.
#
# Validates:
#   US1  SC-001/003/006 — the blocking purge runs and leaves zero release residue;
#                          the database schema is reset (FR-013).
#   US2  SC-002         — an immediate redeploy is pristine (no stale records/collisions).
#   US3  SC-004         — the default `retain` runs NO teardown hooks (no regression).
#                          SC-005 idempotency is unit-tested in internal/teardown; the
#                          delete path is re-runnable by design (not-found = success).
#
#   O3K_IMAGE_REPO=ghcr.io/you/o3k KUBECTL_IMAGE=rancher/kubectl:v1.31 \
#   O3K_DB_URL='postgres://o3k:pw@host:5432/o3k?sslmode=disable' \
#   PSQL_URL='postgres://o3k:pw@host:5432/o3k' ./test/helm_teardown_test.sh
set -euo pipefail

REL="${REL:-o3k-teardown}"; NS="${NS:-o3k-teardown}"
CHART="$(cd "$(dirname "$0")/.." && pwd)/charts/o3k"
COMMON=(
  --set image.repository="${O3K_IMAGE_REPO:?set O3K_IMAGE_REPO}"
  --set image.tag="${O3K_IMAGE_TAG:-latest}"
  --set secrets.jwtSecret="$(openssl rand -hex 16)"
  --set secrets.adminPassword="Teardown-Admin-1"
  --set database.url="${O3K_DB_URL:?set O3K_DB_URL}"
)
DELETE=(--set dataRetentionPolicy=delete --set teardown.kubectlImage="${KUBECTL_IMAGE:?set KUBECTL_IMAGE}")
psql_count() { psql "${PSQL_URL:?set PSQL_URL}" -tAc "$1" | tr -d '[:space:]'; }

# ---------------------------------------------------------------------------
# US1 — install in delete mode, then tear down: blocking purge + zero residue.
# ---------------------------------------------------------------------------
helm install "$REL" "$CHART" -n "$NS" --create-namespace "${COMMON[@]}" "${DELETE[@]}"
kubectl -n "$NS" rollout status "deploy/${REL}-o3k" --timeout=300s
helm test "$REL" -n "$NS"
echo "networks in DB before teardown: $(psql_count 'SELECT count(*) FROM networks')"

# helm uninstall returns only once the blocking pre-delete hooks succeed (SC-003).
helm uninstall "$REL" -n "$NS"

sleep 3
LEFT="$(kubectl -n "$NS" get deploy,svc,secret,pvc,job -l "app.kubernetes.io/instance=${REL}" -o name 2>/dev/null || true)"
[ -z "$LEFT" ] && echo "PASS: no release residue after delete teardown (SC-001/006)" \
  || { echo "FAIL: residue remains: $LEFT"; exit 1; }

[ "$(psql_count 'SELECT count(*) FROM networks')" = "0" ] \
  && echo "PASS: database schema reset — no residual records (FR-013/SC-001)" \
  || { echo "FAIL: database still holds records after delete teardown"; exit 1; }

# ---------------------------------------------------------------------------
# US2 — an immediate redeploy (same release/node/DB) is pristine (SC-002).
# ---------------------------------------------------------------------------
helm install "$REL" "$CHART" -n "$NS" "${COMMON[@]}" "${DELETE[@]}"
kubectl -n "$NS" rollout status "deploy/${REL}-o3k" --timeout=300s
helm test "$REL" -n "$NS" && echo "PASS: immediate redeploy healthy, no collisions (SC-002)"
helm uninstall "$REL" -n "$NS"   # tidy (delete mode)

# ---------------------------------------------------------------------------
# US3 — the default `retain` removes workloads but runs NO teardown hooks (SC-004).
# ---------------------------------------------------------------------------
RET="${REL}-retain"
helm install "$RET" "$CHART" -n "$NS" "${COMMON[@]}"   # retain is the default
kubectl -n "$NS" rollout status "deploy/${RET}-o3k" --timeout=300s
helm uninstall "$RET" -n "$NS"
sleep 3
JOBS="$(kubectl -n "$NS" get jobs -o name 2>/dev/null | grep teardown || true)"
[ -z "$JOBS" ] && echo "PASS: retain ran no teardown hooks (SC-004 — no regression)" \
  || { echo "FAIL: teardown jobs appeared under retain: $JOBS"; exit 1; }

echo "TEARDOWN SCENARIO PASS"
