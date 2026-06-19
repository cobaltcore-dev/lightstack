#!/usr/bin/env bash
# SPEC-008 T004 — chart test runner (no cluster needed).
#   helm plugin install https://github.com/helm-unittest/helm-unittest   # one-time
#   charts/o3k/ci/run-tests.sh
set -euo pipefail
CHART="$(cd "$(dirname "$0")/.." && pwd)"
VALS="$CHART/ci/test-values.yaml"

echo "== helm lint =="
helm lint "$CHART" -f "$VALS"

echo "== helm template (renders cleanly) =="
helm template r "$CHART" -f "$VALS" >/dev/null

echo "== helm template: TLS + real-mode permutations render =="
helm template r "$CHART" -f "$VALS" --set tls.enabled=true --set tls.secretName=c --set ingress.enabled=true >/dev/null
helm template r "$CHART" -f "$VALS" --set realMode.enabled=true --set realMode.privileged=true >/dev/null

echo "== helm unittest (requires the helm-unittest plugin) =="
helm unittest "$CHART"

echo "chart tests OK"
