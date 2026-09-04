#!/usr/bin/env bash
# SPEC-009 WS-2 (T016 / FR-008a): best-effort reconciliation — a corrupt subnet
# row must not block the control plane or other tenants. Automated: inject an
# unparseable CIDR into a THROWAWAY subnet and verify the API and the healthy
# tenant's DHCP are unaffected after one reconcile pass. DB access via LIGHTSTACK_PSQL
# (default: the lightstack-postgres podman container).
set -euo pipefail
: "${OS_AUTH_URL:?source your OpenStack env first}"
INTERVAL="${LIGHTSTACK_RECONCILE_INTERVAL:-35}"
PSQL="${LIGHTSTACK_PSQL:-podman exec -i lightstack-postgres psql -U lightstack -d lightstack}"

dnsmasq_count() { pgrep -fa dnsmasq 2>/dev/null | grep -c var/lib/lightstack/dhcp || true; }

# Baseline: a healthy tenant (a DHCP network + VM) must already be serving.
[ "$(dnsmasq_count)" -ge 1 ] || { echo "FAIL: need a healthy DHCP network+VM running first (the reconciler must already serve one)"; exit 1; }

# A throwaway network+subnet whose row we corrupt (10.99/24 — no CIDR collision).
openstack network delete ws2-bad-net >/dev/null 2>&1 || true # clear any leftover
badnet=$(openstack network create ws2-bad-net -f value -c id)
badsub=$(openstack subnet create ws2-bad-sub --network "$badnet" --subnet-range 10.99.0.0/24 -f value -c id)
cleanup() {
  $PSQL -c "DELETE FROM subnets WHERE id='$badsub';" >/dev/null 2>&1 || true
  openstack network delete "$badnet" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# Corrupt its CIDR; the next reconcile pass must skip it, not crash.
$PSQL -c "UPDATE subnets SET cidr='not-a-cidr' WHERE id='$badsub';" >/dev/null
echo "[ws2-partial] injected an unparseable CIDR into a throwaway subnet; waiting ${INTERVAL}s for a reconcile pass..."
sleep "$INTERVAL"

ok=1
if openstack token issue >/dev/null 2>&1; then
  echo "PASS: control plane stayed ready despite the bad subnet row"
else
  echo "FAIL: API not ready after a bad subnet row was reconciled"; ok=0
fi
if [ "$(dnsmasq_count)" -ge 1 ]; then
  echo "PASS: the healthy tenant's dnsmasq kept serving (bad row isolated)"
else
  echo "FAIL: a healthy dnsmasq stopped — the bad row was not isolated"; ok=0
fi
[ "$ok" -eq 1 ] || exit 1
echo "[ws2-partial] OK"
