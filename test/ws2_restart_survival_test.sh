#!/usr/bin/env bash
# SPEC-009 WS-2 (T016 / SC-003): a VM keeps its IP across an O3K control-plane
# restart. The dev host runs o3k in the foreground, so the restart is manual;
# this two-phase script records the before-state and verifies the after-state.
#   1) ./test/ws2_restart_survival_test.sh record <server>
#   2) restart the o3k process (Ctrl-C, then re-run it)
#   3) ./test/ws2_restart_survival_test.sh verify <server> '<addresses-from-step-1>'
set -euo pipefail
: "${OS_AUTH_URL:?source your OpenStack env first}"
cmd="${1:-record}"; vm="${2:-vm1}"

case "$cmd" in
  record)
    ip=$(openstack server show "$vm" -f value -c addresses)
    echo "[ws2] $vm addresses before restart: $ip"
    echo "Now restart o3k, then run: $0 verify $vm '$ip'"
    ;;
  verify)
    want="${3:?expected addresses string from the record step}"
    got=$(openstack server show "$vm" -f value -c addresses)
    echo "[ws2] $vm addresses after restart: $got"
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
  *) echo "usage: $0 record|verify <server> [expected-addresses]"; exit 1 ;;
esac
