#!/usr/bin/env bash
# SPEC-008 T020 — multi-node e2e (DEFERRED, gated on issue #52 / WS-1).
#
# Cross-node VM lifecycle (SC-003: all instances active in 60s) and cross-node tenant
# traffic (SC-004) require the multi-node VM-placement post-condition (WS-1). Until it
# ships, this suite is gated OFF — the chart renders/installs a multi-node DaemonSet,
# but lifecycle ops are not yet guaranteed to reach the hosting node.
set -euo pipefail

if [ "${LIGHTSTACK_RUN_DEFERRED_MULTINODE:-}" != "1" ]; then
  echo "SKIP: multi-node e2e is deferred until the post-conditions land (issue #52 / WS-1)."
  echo "      Set LIGHTSTACK_RUN_DEFERRED_MULTINODE=1 to run once a WS-1-capable product image exists,"
  echo "      on a multi-node cluster with >=2 nodes labeled ${LIGHTSTACK_NODE_LABEL:-lightstack.io/compute=true}."
  exit 0
fi

REL="${REL:-lightstack-mn}"; NS="${NS:-lightstack-mn}"
CHART="$(cd "$(dirname "$0")/.." && pwd)/charts/lightstack"

helm install "$REL" "$CHART" -n "$NS" --create-namespace \
  --set deploymentMode=multi-node \
  --set image.repository="${LIGHTSTACK_IMAGE_REPO:?set LIGHTSTACK_IMAGE_REPO}" \
  --set image.tag="${LIGHTSTACK_IMAGE_TAG:-latest}" \
  --set secrets.jwtSecret="$(openssl rand -hex 16)" \
  --set secrets.adminPassword="Mn-Admin-1" \
  --set database.url="${LIGHTSTACK_DB_URL:?set LIGHTSTACK_DB_URL}"

kubectl -n "$NS" rollout status "ds/${REL}-lightstack" --timeout=300s

# SC-003 / SC-004: reuse the existing overlay/multi-node networking suite against the
# chart-deployed release once WS-1 is available.
if [ -f test/vxlan_multinode_test.sh ]; then
  ./test/vxlan_multinode_test.sh
else
  echo "NOTE: test/vxlan_multinode_test.sh not found; run the overlay suite manually."
fi

helm uninstall "$REL" -n "$NS"
echo "MULTI-NODE E2E DONE"
