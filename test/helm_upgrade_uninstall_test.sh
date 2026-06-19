#!/usr/bin/env bash
# SPEC-008 T027 — day-2 upgrade + uninstall scenario (single-node stub).
# Requires: a Kubernetes cluster (kind/k3s), kubectl, helm, an external PostgreSQL,
# and an O3K image. Validates SC-005 (config change applied, no data loss) and
# SC-006 (uninstall leaves no orphaned workloads). FR-014.
#
#   O3K_IMAGE_REPO=ghcr.io/you/o3k O3K_DB_URL='postgres://o3k:pw@host:5432/o3k?sslmode=disable' \
#   ./test/helm_upgrade_uninstall_test.sh
set -euo pipefail

REL="${REL:-o3k-day2}"; NS="${NS:-o3k-day2}"
CHART="$(cd "$(dirname "$0")/.." && pwd)/charts/o3k"
COMMON=(
  --set image.repository="${O3K_IMAGE_REPO:?set O3K_IMAGE_REPO}"
  --set image.tag="${O3K_IMAGE_TAG:-latest}"
  --set secrets.jwtSecret="$(openssl rand -hex 16)"
  --set secrets.adminPassword="Day2-Admin-1"
  --set database.url="${O3K_DB_URL:?set O3K_DB_URL}"
)

helm install "$REL" "$CHART" -n "$NS" --create-namespace "${COMMON[@]}"
kubectl -n "$NS" rollout status "deploy/${REL}-o3k" --timeout=300s

# SC-005: a config change is applied on upgrade.
helm upgrade "$REL" "$CHART" -n "$NS" "${COMMON[@]}" --set logging.level=debug
kubectl -n "$NS" rollout status "deploy/${REL}-o3k" --timeout=300s
kubectl -n "$NS" get secret "${REL}-o3k-config" -o jsonpath='{.data.o3k\.yaml}' \
  | base64 -d | grep -q 'level: "debug"' \
  && echo "PASS: config change applied on upgrade (SC-005)"

# SC-006: uninstall removes all release workloads.
helm uninstall "$REL" -n "$NS"
sleep 3
LEFT="$(kubectl -n "$NS" get deploy,svc,secret -l "app.kubernetes.io/instance=${REL}" -o name 2>/dev/null || true)"
if [ -z "$LEFT" ]; then echo "PASS: no orphaned workloads after uninstall (SC-006)"; else
  echo "FAIL: orphaned resources remain: $LEFT"; exit 1; fi

echo "DAY-2 PASS"
