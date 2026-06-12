#!/usr/bin/env bash
# SPEC-008 T011 — single-node stub smoke test for the O3K Helm chart.
# Requires: a Kubernetes cluster (kind/k3s), kubectl, helm, the openstack CLI,
# a reachable external PostgreSQL, and an O3K image. Validates SC-001/002 (stub subset)
# and the helm test hook (FR-015).
#
#   O3K_IMAGE_REPO=ghcr.io/you/o3k O3K_IMAGE_TAG=v0.7.0 \
#   O3K_DB_URL='postgres://o3k:pw@host:5432/o3k?sslmode=disable' \
#   ./test/helm_singlenode_stub_test.sh
set -euo pipefail

REL="${REL:-o3k-smoke}"
NS="${NS:-o3k-smoke}"
ADMIN_PW="Smoke-Admin-1"
CHART="$(cd "$(dirname "$0")/.." && pwd)/charts/o3k"

helm install "$REL" "$CHART" -n "$NS" --create-namespace \
  --set image.repository="${O3K_IMAGE_REPO:?set O3K_IMAGE_REPO}" \
  --set image.tag="${O3K_IMAGE_TAG:-latest}" \
  --set secrets.jwtSecret="$(openssl rand -hex 16)" \
  --set secrets.adminPassword="$ADMIN_PW" \
  --set database.url="${O3K_DB_URL:?set O3K_DB_URL}"

kubectl -n "$NS" rollout status "deploy/${REL}-o3k" --timeout=300s
helm test "$REL" -n "$NS"

# Forward every API port the CLI follows from the catalog (it advertises localhost:<port>);
# 35357 alone is not enough — the first compute/image call would be connection-refused.
kubectl -n "$NS" port-forward "svc/${REL}-o3k" \
  35357:35357 8774:8774 8778:8778 9696:9696 8776:8776 9292:9292 >/dev/null 2>&1 &
PF=$!; trap 'kill $PF 2>/dev/null || true' EXIT
sleep 4

export OS_AUTH_URL=http://localhost:35357/v3
export OS_USERNAME=admin OS_PROJECT_NAME=default
export OS_USER_DOMAIN_NAME=Default OS_PROJECT_DOMAIN_NAME=Default

OS_PASSWORD="$ADMIN_PW" openstack token issue >/dev/null && echo "PASS: configured admin password works"
if OS_PASSWORD=secret openstack token issue >/dev/null 2>&1; then
  echo "FAIL: default 'secret' must be rejected"; exit 1
fi
echo "PASS: default 'secret' rejected"

OS_PASSWORD="$ADMIN_PW" openstack flavor list >/dev/null && echo "PASS: compute catalog endpoint reachable"
# Stub glance seeds no image; create one so the server has something to boot from.
head -c 1024 /dev/zero > /tmp/o3k-smoke.img
OS_PASSWORD="$ADMIN_PW" openstack image create cirros \
  --disk-format qcow2 --container-format bare --public --file /tmp/o3k-smoke.img >/dev/null
OS_PASSWORD="$ADMIN_PW" openstack server create --flavor m1.small --image cirros smoke-vm --wait >/dev/null \
  && echo "PASS: stub server create"
OS_PASSWORD="$ADMIN_PW" openstack server list >/dev/null && echo "PASS: stub server lifecycle"
OS_PASSWORD="$ADMIN_PW" openstack server delete smoke-vm >/dev/null 2>&1 || true
OS_PASSWORD="$ADMIN_PW" openstack image delete cirros >/dev/null 2>&1 || true

helm uninstall "$REL" -n "$NS"
echo "SMOKE PASS"
