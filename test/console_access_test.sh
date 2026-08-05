#!/bin/bash
# Console security probe (SPEC-021 / cloud-storage#534, SC-005 + SC-007).
#
# A console URL bypasses SSH keys, guest firewalls and every in-guest control, so
# the link must open exactly one instance, for one entitled caller, for a bounded
# window. This probes the ways that can go wrong.
#
# REAL MODE ONLY: stub mode refuses console requests at the API (501), so there is
# no token to attack. Run ./test/console_test.sh for the stub-mode checks.
#
# Usage:  LIGHTSTACK_MODE=real ./test/console_access_test.sh
# Env:    KEYSTONE_URL, NOVA_URL

set -uo pipefail

KEYSTONE_URL="${KEYSTONE_URL:-http://localhost:35357}"
NOVA_URL="${NOVA_URL:-http://localhost:8774}"
GLANCE_URL="${GLANCE_URL:-http://localhost:9292}"
NEUTRON_URL="${NEUTRON_URL:-http://localhost:9696}"

pass=0
fail=0
ok()  { echo "  ✓ $1"; pass=$((pass + 1)); }
bad() { echo "  ✗ $1"; fail=$((fail + 1)); }

echo "=== Console security probe ==="

auth() {
  curl -i -s -X POST "$KEYSTONE_URL/v3/auth/tokens" -H "Content-Type: application/json" \
    -d '{"auth":{"identity":{"methods":["password"],"password":{"user":{"name":"admin","password":"secret","domain":{"name":"Default"}}}},"scope":{"project":{"name":"default","domain":{"name":"Default"}}}}}' \
    | grep -i "X-Subject-Token:" | awk '{print $2}' | tr -d '\r'
}

TOKEN=$(auth)
[ -n "$TOKEN" ] || { echo "✗ authentication failed"; exit 1; }
echo "✓ authenticated"

# Real mode boots the guest for real, so an instance needs a bootable image AND a
# network — without them it reaches ERROR, gets no libvirt domain, and the console
# is (correctly) refused with 409. Discover both, or override via env.
IMAGE_NAME="${IMAGE_NAME:-cirros}"
NETWORK_NAME="${NETWORK_NAME:-spec021-net}"

lookup() {  # $1 = url path, $2 = json array key, $3 = name
  curl -s "$1" -H "X-Auth-Token: $TOKEN" | jq -r --arg n "$3" ".$2[] | select(.name==\$n) | .id" | head -1
}

IMAGE_ID=$(lookup "$GLANCE_URL/v2/images" images "$IMAGE_NAME")
NET_ID=$(lookup "$NEUTRON_URL/v2.0/networks" networks "$NETWORK_NAME")
if [ -z "$IMAGE_ID" ] || [ -z "$NET_ID" ]; then
  echo "✗ need image '$IMAGE_NAME' and network '$NETWORK_NAME' (see the real-mode runbook Setup)"
  exit 1
fi
echo "✓ image $IMAGE_NAME and network $NETWORK_NAME found"

new_instance() {  # $1 = name -> prints the id once ACTIVE
  local id
  id=$(curl -s -X POST "$NOVA_URL/v2.1/servers" -H "X-Auth-Token: $TOKEN" \
    -H "Content-Type: application/json" \
    -d "{\"server\":{\"name\":\"$1\",\"flavorRef\":\"00000000-0000-0000-0000-000000000011\",\"imageRef\":\"$IMAGE_ID\",\"networks\":[{\"uuid\":\"$NET_ID\"}]}}" \
    | jq -r '.server.id')
  [ -n "$id" ] && [ "$id" != "null" ] || return 1
  # Wait for the hypervisor to actually define the domain; until then there is no
  # console endpoint to read and the API answers 409.
  for _ in $(seq 1 30); do
    [ "$(curl -s "$NOVA_URL/v2.1/servers/$id" -H "X-Auth-Token: $TOKEN" | jq -r '.server.status')" = "ACTIVE" ] && break
    sleep 1
  done
  printf '%s' "$id"
}

console_url() {  # $1 = instance id -> prints the URL, or nothing
  curl -s -X POST "$NOVA_URL/v2.1/servers/$1/remote-consoles" \
    -H "X-Auth-Token: $TOKEN" -H "Content-Type: application/json" \
    -d '{"remote_console":{"protocol":"vnc","type":"novnc"}}' \
    | jq -r '.remote_console.url // empty'
}

token_of() { printf '%s' "$1" | sed -n 's/.*token%3D\([0-9a-f]*\).*/\1/p'; }

resolves() {  # $1 = token -> 0 if it resolves
  [ "$(curl -s -o /dev/null -w '%{http_code}' \
      "$NOVA_URL/v2.1/os-console-auth-tokens/$1" -H "X-Auth-Token: $TOKEN")" = "200" ]
}

proxy_resolves() {  # $1 = console URL -> 0 if the proxy still resolves its token
  # websockify looks the token up before it answers the upgrade: 101 while the
  # projection entry exists, the connection closed without a response once it is gone.
  # --max-time ends the guest session a 101 opens.
  [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 \
      -H 'Connection: Upgrade' -H 'Upgrade: websocket' -H 'Sec-WebSocket-Version: 13' \
      -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' \
      "${1%%/vnc_lite.html*}/?token=$(token_of "$1")")" = "101" ]
}

VM_A=$(new_instance spec021-sec-a)
VM_B=$(new_instance spec021-sec-b)
[ -n "$VM_A" ] && [ -n "$VM_B" ] || { echo "✗ could not create the test instances"; exit 1; }
echo "✓ booted $VM_A and $VM_B"
trap 'for v in "$VM_A" "$VM_B"; do curl -s -X DELETE "$NOVA_URL/v2.1/servers/$v" -H "X-Auth-Token: $TOKEN" >/dev/null || true; done' EXIT
sleep 1

URL_A=$(console_url "$VM_A")
if [ -z "$URL_A" ]; then
  echo "✗ no console issued for VM A — run this in REAL mode (stub refuses with 501)"
  exit 1
fi
TOK_A=$(token_of "$URL_A")
echo "✓ issued a console for VM A"

echo ""
echo "1. a legitimate link resolves — and keeps resolving (FR-010a)"
resolves "$TOK_A" && ok "first use resolves" || bad "a freshly issued token did not resolve"
# A page refresh or a client reconnect must not invalidate the link.
resolves "$TOK_A" && ok "second use also resolves (refresh/reconnect works)" \
  || bad "the token stopped resolving on reuse"

echo ""
echo "2. an invented link is refused"
resolves "deadbeefdeadbeefdeadbeefdeadbeef" && bad "an invented token resolved!" \
  || ok "invented token refused"

echo ""
echo "3. a link for VM A never reaches VM B"
URL_B=$(console_url "$VM_B")
TOK_B=$(token_of "$URL_B")
A_INST=$(curl -s "$NOVA_URL/v2.1/os-console-auth-tokens/$TOK_A" -H "X-Auth-Token: $TOKEN" | jq -r '.console.instance_uuid')
B_INST=$(curl -s "$NOVA_URL/v2.1/os-console-auth-tokens/$TOK_B" -H "X-Auth-Token: $TOKEN" | jq -r '.console.instance_uuid')
[ "$A_INST" = "$VM_A" ] && ok "A's token maps to A" || bad "A's token maps to $A_INST"
[ "$B_INST" = "$VM_B" ] && ok "B's token maps to B" || bad "B's token maps to $B_INST"
[ "$TOK_A" != "$TOK_B" ] && ok "the two instances got distinct tokens" || bad "token collision"

echo ""
echo "4. stopping an instance revokes its links"
# The lifecycle revoke: a link must not outlive the console it was issued for. (The
# console is a per-start unix socket, so a surviving link would name a dead path
# rather than another guest — step 8 covers the stops that never reach the API.)
curl -s -X POST "$NOVA_URL/v2.1/servers/$VM_A/action" -H "X-Auth-Token: $TOKEN" \
  -H "Content-Type: application/json" -d '{"os-stop":{}}' >/dev/null
sleep 1
resolves "$TOK_A" && bad "a token survived its instance being stopped" \
  || ok "tokens revoked on stop"

echo ""
echo "5. deleting an instance revokes its links"
curl -s -X DELETE "$NOVA_URL/v2.1/servers/$VM_B" -H "X-Auth-Token: $TOKEN" >/dev/null
sleep 2
resolves "$TOK_B" && bad "a token survived its instance being deleted" || ok "tokens revoked on delete"

echo ""
echo "6. a link survives a Lightstack restart (FR-009 / SC-007)"
# This is the only reason tokens are persisted rather than held in memory. Skipped
# unless the caller says how to restart, since the method is deployment-specific.
if [ -n "${LIGHTSTACK_RESTART_CMD:-}" ]; then
  VM_C=$(new_instance spec021-sec-c)
  sleep 1
  URL_C=$(console_url "$VM_C")
  TOK_C=$(token_of "$URL_C")
  eval "$LIGHTSTACK_RESTART_CMD"
  for _ in $(seq 1 30); do
    curl -sf "$KEYSTONE_URL/v3" >/dev/null 2>&1 && break
    sleep 1
  done
  TOKEN=$(auth)
  resolves "$TOK_C" && ok "a pre-restart link still resolves" \
    || bad "the link did not survive a restart — persistence is not working"
  curl -s -X DELETE "$NOVA_URL/v2.1/servers/$VM_C" -H "X-Auth-Token: $TOKEN" >/dev/null || true
else
  echo "  ⊘ skipped: set LIGHTSTACK_RESTART_CMD (e.g. 'docker restart lightstack') to check restart survival"
fi

echo ""
echo "7. expiry is enforced"
# Effective validity is console_token_ttl + console_reconcile_interval, because the
# proxy performs no expiry check — a token dies when Lightstack deletes its projection
# entry. So this asks the proxy, not the API: the API filters on expires_at itself and
# would refuse an expired token even if the reconciler never reaped one. Skipped by
# default: waiting out the default 10m TTL is too slow for CI.
if [ "${LIGHTSTACK_CHECK_EXPIRY:-0}" = "1" ]; then
  TTL_WAIT="${LIGHTSTACK_TTL_WAIT:-660}"
  VM_D=$(new_instance spec021-sec-d)
  sleep 1
  URL_D=$(console_url "$VM_D")
  # The control: a probe that never reaches the proxy refuses every token, and the
  # expiry check below would then pass for the wrong reason.
  if ! proxy_resolves "$URL_D"; then
    bad "the proxy did not resolve a fresh token — expiry cannot be tested through it"
  else
    echo "  waiting ${TTL_WAIT}s for the token to expire and be reaped..."
    sleep "$TTL_WAIT"
    proxy_resolves "$URL_D" && bad "the proxy still resolves an expired token — the reconciler is not reaping" \
      || ok "expired token refused by the proxy"
  fi
  curl -s -X DELETE "$NOVA_URL/v2.1/servers/$VM_D" -H "X-Auth-Token: $TOKEN" >/dev/null || true
else
  echo "  ⊘ skipped: set LIGHTSTACK_CHECK_EXPIRY=1 (slow — waits out the token TTL)"
fi

echo ""
echo "8. a link dies when its guest stops outside the API (the stale-token sweep)"
# A guest that powers itself off, crashes or is stopped behind Lightstack's back never
# reaches the lifecycle revoke. The reconciler compares every token with the socket its
# guest serves now, so the link must die within one reconcile interval (30s by
# default). virsh destroy stands in for an in-guest poweroff; the domain UUID is the
# server id.
VIRSH="${VIRSH:-sudo virsh}"
if $VIRSH version >/dev/null 2>&1; then
  VM_E=$(new_instance spec021-sec-e)
  sleep 1
  TOK_E=$(token_of "$(console_url "$VM_E")")
  if [ -z "$TOK_E" ]; then
    bad "no console issued for the out-of-band test instance"
  else
    $VIRSH destroy "$VM_E" >/dev/null 2>&1
    gone=0
    for _ in $(seq 1 "${LIGHTSTACK_SWEEP_WAIT:-60}"); do
      resolves "$TOK_E" || { gone=1; break; }
      sleep 1
    done
    [ "$gone" = 1 ] && ok "a guest stopped outside the API loses its links within one sweep" \
      || bad "a token outlived its guest stopping outside the API — the stale-token sweep is not running"
  fi
  curl -s -X DELETE "$NOVA_URL/v2.1/servers/$VM_E" -H "X-Auth-Token: $TOKEN" >/dev/null || true
else
  echo "  ⊘ skipped: needs virsh on this host (set VIRSH, e.g. 'virsh -c qemu:///system')"
fi

echo ""
echo "========================================"
echo "passed: $pass   failed: $fail"
echo "========================================"
[ "$fail" -eq 0 ]
