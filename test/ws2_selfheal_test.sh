#!/usr/bin/env bash
# SPEC-009 WS-2 (T016 / SC-010): the network reconciler respawns a dnsmasq that
# was killed out-of-band, within one reconcile interval. Fully automated — run
# on the host against a running real-mode O3K (networking_mode != stub) that has
# at least one DHCP-enabled subnet (create a network+subnet+VM first).
set -euo pipefail
INTERVAL="${O3K_RECONCILE_INTERVAL:-35}"   # reconcile_interval (default 30s) + slack

count() { pgrep -fa dnsmasq 2>/dev/null | grep -c "var/lib/o3k/dhcp" || true; }

before=$(count)
[ "$before" -ge 1 ] || { echo "FAIL: no o3k dnsmasq running — a real-mode network+subnet+VM must exist (the reconciler serves it) before self-heal can be tested"; exit 1; }

echo "[ws2-selfheal] $before o3k dnsmasq running; killing them..."
sudo pkill -f "var/lib/o3k/dhcp" || true
sleep 2
echo "[ws2-selfheal] after kill: $(count) running; waiting ${INTERVAL}s for the reconciler..."
sleep "$INTERVAL"

after=$(count)
if [ "$after" -ge 1 ]; then
  echo "PASS: reconciler respawned dnsmasq ($after running)"
else
  echo "FAIL: dnsmasq was not respawned within ${INTERVAL}s"; exit 1
fi
echo "[ws2-selfheal] OK"
