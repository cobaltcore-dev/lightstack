#!/bin/bash
# Console API test (SPEC-021 / cloud-storage#534, finding F1).
#
# The version this replaces asserted only that *a* URL string came back — which is
# exactly why it stayed green against a console that could never work: the old
# handlers returned a hardcoded http://localhost:6080 URL with a token nothing
# recorded and a port derived from a hash. This asserts the parts that make a URL
# real, and that everything we cannot serve is refused rather than faked.
#
# Runs against the STUB stack by default, where the console must refuse with 501.
# In real mode it additionally checks that a resolvable URL is issued.
#
# Usage:  ./test/console_test.sh
# Env:    KEYSTONE_URL, NOVA_URL, O3K_MODE (stub|real)

set -euo pipefail

KEYSTONE_URL="${KEYSTONE_URL:-http://localhost:35357}"
NOVA_URL="${NOVA_URL:-http://localhost:8774}"
O3K_MODE="${O3K_MODE:-stub}"

pass=0
fail=0
ok()   { echo "  ✓ $1"; pass=$((pass + 1)); }
bad()  { echo "  ✗ $1"; fail=$((fail + 1)); }

echo "=== Console API test (mode: $O3K_MODE) ==="

TOKEN=$(curl -i -s -X POST "$KEYSTONE_URL/v3/auth/tokens" \
    -H "Content-Type: application/json" \
    -d '{"auth":{"identity":{"methods":["password"],"password":{"user":{"name":"admin","password":"secret","domain":{"name":"Default"}}}},"scope":{"project":{"name":"default","domain":{"name":"Default"}}}}}' \
    | grep -i "X-Subject-Token:" | awk '{print $2}' | tr -d '\r')

if [ -z "$TOKEN" ]; then
    echo "✗ authentication failed"
    exit 1
fi
echo "✓ authenticated"

# Per-run scratch dir, not fixed /tmp paths: this script takes O3K_MODE, so stub and
# real runs are meant to be run side by side, and shared paths would have each
# asserting against the other's response body. Real-mode runs also happen under sudo,
# which would otherwise leave root-owned files that break the next unprivileged run.
#
# Installed before the instance exists so an early failure still cleans up; both the
# rm and the DELETE tolerate a half-finished run.
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"; [ -n "${INSTANCE_ID:-}" ] && curl -s -X DELETE "$NOVA_URL/v2.1/servers/$INSTANCE_ID" -H "X-Auth-Token: $TOKEN" >/dev/null 2>&1 || true' EXIT

INSTANCE_ID=$(curl -s -X POST "$NOVA_URL/v2.1/servers" \
    -H "X-Auth-Token: $TOKEN" -H "Content-Type: application/json" \
    -d '{"server":{"name":"spec021-console-vm","flavorRef":"00000000-0000-0000-0000-000000000010"}}' \
    | jq -r '.server.id')

if [ -z "$INSTANCE_ID" ] || [ "$INSTANCE_ID" = "null" ]; then
    echo "✗ failed to create the test instance"
    exit 1
fi
echo "✓ created instance $INSTANCE_ID"
sleep 2

remote_console() {  # $1 = type
    curl -s -o "$TMP/body" -w '%{http_code}' \
        -X POST "$NOVA_URL/v2.1/servers/$INSTANCE_ID/remote-consoles" \
        -H "X-Auth-Token: $TOKEN" -H "Content-Type: application/json" \
        -d "{\"remote_console\":{\"protocol\":\"vnc\",\"type\":\"$1\"}}"
}

echo ""
echo "1. novnc console"
CODE=$(remote_console novnc)
BODY=$(cat "$TMP/body")

if [ "$O3K_MODE" = "stub" ]; then
    # Stub mode has no hypervisor and therefore no guest. Saying so is the whole
    # point — a URL here would reproduce F1, where the browser failed opaquely.
    [ "$CODE" = "501" ] && ok "stub mode refuses with 501" || bad "expected 501, got $CODE: $BODY"
    echo "$BODY" | grep -qi "stub" && ok "the refusal names stub mode" || bad "refusal should name stub mode: $BODY"
    echo "$BODY" | grep -q "http://" && bad "stub mode must not return a URL: $BODY" || ok "no URL returned"
else
    [ "$CODE" = "200" ] && ok "real mode issues a console" || bad "expected 200, got $CODE: $BODY"
    URL=$(echo "$BODY" | jq -r '.remote_console.url')

    # The URL must name a page the deployed client actually serves. noVNC 1.6.0
    # has no vnc_auto.html — the operator docs pointed at it for years.
    case "$URL" in
        *"/vnc_lite.html?path="*) ok "URL uses the vnc_lite.html client page" ;;
        *) bad "unexpected console URL shape: $URL" ;;
    esac
    case "$URL" in
        *vnc_auto.html*) bad "vnc_auto.html does not exist in noVNC 1.6.0: $URL" ;;
        *localhost*) bad "URL points at localhost — unreachable from a browser: $URL" ;;
        *) ok "URL points at a configured, non-localhost origin" ;;
    esac

    # A token that cannot be resolved is the F1 bug in a new costume.
    TOK=$(printf '%s' "$URL" | sed -n 's/.*token%3D\([0-9a-f]*\).*/\1/p')
    [ -n "$TOK" ] && ok "URL carries a token" || bad "no token in URL: $URL"

    AUTH_CODE=$(curl -s -o "$TMP/auth" -w '%{http_code}' \
        "$NOVA_URL/v2.1/os-console-auth-tokens/$TOK" -H "X-Auth-Token: $TOKEN")
    [ "$AUTH_CODE" = "200" ] && ok "the token resolves via os-console-auth-tokens" \
        || bad "token did not resolve ($AUTH_CODE): $(cat "$TMP/auth")"

    RESOLVED=$(jq -r '.console.instance_uuid' < "$TMP/auth" 2>/dev/null || echo "")
    [ "$RESOLVED" = "$INSTANCE_ID" ] && ok "the token maps to this instance" \
        || bad "token maps to $RESOLVED, expected $INSTANCE_ID"
fi

echo ""
echo "2. legacy os-getVNCConsole agrees with remote-consoles"
LEG_CODE=$(curl -s -o "$TMP/legacy" -w '%{http_code}' \
    -X POST "$NOVA_URL/v2.1/servers/$INSTANCE_ID/action" \
    -H "X-Auth-Token: $TOKEN" -H "Content-Type: application/json" \
    -d '{"os-getVNCConsole":{"type":"novnc"}}')
[ "$LEG_CODE" = "$CODE" ] && ok "both surfaces answer $LEG_CODE" \
    || bad "remote-consoles said $CODE but the legacy action said $LEG_CODE"

echo ""
echo "3. unsupported console types are refused, not faked"
for t in xvpvnc spice-html5 rdp-html5; do
    C=$(remote_console "$t")
    B=$(cat "$TMP/body")
    if [ "$C" = "400" ] && ! echo "$B" | grep -q "http://"; then
        ok "$t refused with 400 and no URL"
    else
        bad "$t returned $C: $B"
    fi
done

echo ""
echo "4. the serial console log still works (the documented fallback)"
OUT_CODE=$(curl -s -o "$TMP/out" -w '%{http_code}' \
    -X POST "$NOVA_URL/v2.1/servers/$INSTANCE_ID/action" \
    -H "X-Auth-Token: $TOKEN" -H "Content-Type: application/json" \
    -d '{"os-getConsoleOutput":{"length":50}}')
[ "$OUT_CODE" = "200" ] && ok "os-getConsoleOutput returns 200" || bad "console log broke: $OUT_CODE"
jq -e '.output' < "$TMP/out" >/dev/null 2>&1 && ok "output field present" || bad "no output field"

echo ""
echo "========================================"
echo "passed: $pass   failed: $fail"
echo "========================================"
[ "$fail" -eq 0 ]
