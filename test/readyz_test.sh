#!/bin/bash
# SPEC-013 — readiness/liveness endpoint integration test.
#
# Two parts:
#   1. Endpoint behavior against a running o3k (local binary or port-forward).
#   2. Kubernetes-native consumption + traffic gating (requires a cluster + DB).
#
# Part 1 needs a reachable o3k identity port; part 2 is skipped unless RUN_K8S=1.
#   O3K_URL=http://localhost:35357 ./test/readyz_test.sh            # part 1
#   RUN_K8S=1 NS=o3k ./test/readyz_test.sh                          # + part 2

GREEN='\033[0;32m'; RED='\033[0;31m'; YELLOW='\033[0;33m'; NC='\033[0m'
O3K_URL="${O3K_URL:-http://localhost:35357}"
PASSED=0; FAILED=0

pass() { echo -e "${GREEN}✓${NC} $1"; PASSED=$((PASSED+1)); }
fail() { echo -e "${RED}✗${NC} $1"; FAILED=$((FAILED+1)); }
skip() { echo -e "${YELLOW}-${NC} (skip) $1"; }

# expect_status <name> <url> <want> [curl args...]
expect_status() {
	local name=$1 url=$2 want=$3; shift 3
	local got
	got=$(curl -s -o /dev/null -w '%{http_code}' "$@" "$url" 2>/dev/null)
	if [ "$got" = "$want" ]; then pass "$name ($got)"; else fail "$name: got $got want $want"; fi
}

echo "== Part 1: endpoint behavior @ ${O3K_URL} =="

# /livez is always 200 while the process is up (no deps).
expect_status "GET /livez = 200" "${O3K_URL}/livez" 200

# /readyz reflects the DB: 200 when reachable, 503 otherwise. With a healthy DB:
expect_status "GET /readyz = 200 (DB reachable)" "${O3K_URL}/readyz" 200

# /healthcheck (oslo alias): GET text, HEAD status-only, JSON negotiation.
expect_status "GET /healthcheck = 200" "${O3K_URL}/healthcheck" 200
expect_status "HEAD /healthcheck = 200" "${O3K_URL}/healthcheck" 200 -I
if curl -s -H 'Accept: application/json' "${O3K_URL}/healthcheck" | grep -q '"reasons"'; then
	pass "GET /healthcheck negotiates JSON"
else
	fail "GET /healthcheck JSON body missing \"reasons\""
fi

# /readyz must never expose error detail — only a coarse reason.
body=$(curl -s "${O3K_URL}/readyz?verbose")
if echo "$body" | grep -qiE 'password|stack|goroutine|sslmode'; then
	fail "/readyz?verbose leaked sensitive detail"
else
	pass "/readyz?verbose exposes no sensitive detail"
fi

echo ""
echo "== Manual check: stop the database, then GET /readyz must return 503 'database unavailable' =="
echo "   (and /livez must stay 200 — a DB outage must not restart the process)"

# CLI subcommand (if the binary is on PATH).
if command -v o3k >/dev/null 2>&1; then
	if o3k healthcheck --ready --addr "${O3K_URL}" >/dev/null 2>&1; then
		pass "o3k healthcheck --ready exits 0"
	else
		fail "o3k healthcheck --ready exits non-zero (DB down?)"
	fi
else
	skip "o3k binary not on PATH — CLI subcommand check"
fi

echo ""
echo "== Part 2: Kubernetes-native consumption =="
if [ "${RUN_K8S:-0}" != "1" ]; then
	skip "RUN_K8S != 1 — cluster checks (Pod Ready gating, EndpointSlice removal, rollout drain)"
else
	NS="${NS:-o3k}"
	if kubectl -n "$NS" wait --for=condition=Ready pod -l app.kubernetes.io/name=o3k --timeout=180s >/dev/null 2>&1; then
		pass "Pod reaches Ready (helm --wait / kubectl wait gate)"
	else
		fail "Pod never became Ready within 180s"
	fi
	# After stopping the DB, the pod must leave the Service EndpointSlice (no traffic),
	# WITHOUT restarting (liveness is /livez). These are asserted by the operator/runbook:
	echo "   Manual/runbook: kill the DB → 'kubectl get endpointslices' drops the pod IP;"
	echo "                   RESTARTS stays unchanged; restore DB → pod re-added."
fi

echo ""
echo "Passed: ${PASSED}  Failed: ${FAILED}"
[ "$FAILED" -eq 0 ]
