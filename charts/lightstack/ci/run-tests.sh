#!/usr/bin/env bash
# SPEC-008 T004 — chart test runner (no cluster needed).
#   helm plugin install https://github.com/helm-unittest/helm-unittest   # one-time
#   charts/lightstack/ci/run-tests.sh
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

echo "== schema rejection: invalid value sets must be rejected before execution (US1 / FR-003) =="
INVALID_DIR="$CHART/ci/invalid-values"
shopt -s nullglob
fixtures=("$INVALID_DIR"/*.yaml)
[ ${#fixtures[@]} -gt 0 ] || { echo "FAIL: no rejection fixtures in $INVALID_DIR"; exit 1; }
for f in "${fixtures[@]}"; do
	name="$(basename "$f")"
	if [ "$name" = "valid-complete.yaml" ]; then
		helm template r "$CHART" -f "$f" >/dev/null || { echo "FAIL: $name should render"; exit 1; }
		echo "ok (renders):  $name"
	elif helm template r "$CHART" -f "$f" >/dev/null 2>&1; then
		echo "FAIL: $name rendered but must be rejected"; exit 1
	else
		echo "ok (rejected): $name"
	fi
done

echo "== schema<->values key coverage (drift gate; SC-005) =="
vkeys="$(grep -E '^[A-Za-z][A-Za-z0-9]*:' "$CHART/values.yaml" | sed 's/:.*//' | sort -u)"
skeys="$(jq -r '.properties | keys[]' "$CHART/values.schema.json" | sort -u)"
if [ "$vkeys" != "$skeys" ]; then
	echo "FAIL: values.yaml top-level keys != values.schema.json properties"
	echo "  only in values.yaml:"; comm -23 <(printf '%s\n' "$vkeys") <(printf '%s\n' "$skeys") | sed 's/^/    /'
	echo "  only in schema:";      comm -13 <(printf '%s\n' "$vkeys") <(printf '%s\n' "$skeys") | sed 's/^/    /'
	exit 1
fi
echo "key coverage OK ($(printf '%s\n' "$vkeys" | grep -c .) top-level keys)"

echo "== helm unittest (requires the helm-unittest plugin) =="
helm unittest "$CHART"

echo "== CONTRACT.md ships in the package (US3 / FR-015) =="
PKG_DIR="$(mktemp -d)"
helm package "$CHART" -d "$PKG_DIR" >/dev/null
# capture first, then grep — piping `tar | grep -q` under `set -o pipefail` can report a false
# failure: grep -q closes the pipe on first match and tar dies with SIGPIPE (R1 review fix).
pkg_entries="$(tar tzf "$PKG_DIR"/lightstack-*.tgz)"
printf '%s\n' "$pkg_entries" | grep -q '^lightstack/CONTRACT.md$' || { echo "FAIL: CONTRACT.md not in the packaged chart"; rm -rf "$PKG_DIR"; exit 1; }
rm -rf "$PKG_DIR"
echo "CONTRACT.md packaged OK"

echo "chart tests OK"
