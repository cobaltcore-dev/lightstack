#!/usr/bin/env bash
# SPEC-009 WS-2 (T016 / SC-003): a VM keeps its IP across an O3K control-plane
# restart. The dev host runs o3k in the foreground, so the restart is manual;
# this two-phase script records the before-state to a file and verifies the
# after-state — no need to copy/paste the (quote-laden) addresses string.
#   1) ./test/ws2_restart_survival_test.sh record <server>
#   2) restart the o3k process (Ctrl-C in T1, then re-run it)
#   3) ./test/ws2_restart_survival_test.sh verify <server>
set -euo pipefail
: "${OS_AUTH_URL:?source your OpenStack env first}"
cmd="${1:-record}"; vm="${2:-vm1}"
statefile="${TMPDIR:-/tmp}/ws2-${vm}.addr"

case "$cmd" in
  record)
    ip=$(openstack server show "$vm" -f value -c addresses)
    printf '%s' "$ip" > "$statefile"
    echo "[ws2] $vm addresses before restart: $ip"
    echo "[ws2] recorded to $statefile"
    echo "Now restart o3k (Ctrl-C in T1, then re-run §6), then run: $0 verify $vm"
    ;;
  verify)
    # Default to the recorded file; an explicit 3rd arg overrides it.
    if [ -n "${3:-}" ]; then
      want="$3"
    elif [ -f "$statefile" ]; then
      want=$(cat "$statefile")
    else
      echo "FAIL: no recorded state at $statefile — run '$0 record $vm' first"; exit 1
    fi
    got=$(openstack server show "$vm" -f value -c addresses)
    echo "[ws2] $vm addresses before restart: $want"
    echo "[ws2] $vm addresses after  restart: $got"
    if [ "$got" = "$want" ]; then
      echo "PASS: same IP retained across the control-plane restart"
    else
      echo "FAIL: addresses changed ('$want' -> '$got')"; exit 1
    fi
    if sudo pgrep -fa dnsmasq 2>/dev/null | grep -q "var/lib/o3k/dhcp"; then
      echo "PASS: dnsmasq respawned by the reconciler (with MAC->IP reservation)"
    else
      echo "WARN: no o3k dnsmasq found after restart"
    fi
    ;;
  *) echo "usage: $0 record|verify <server>"; exit 1 ;;
esac
