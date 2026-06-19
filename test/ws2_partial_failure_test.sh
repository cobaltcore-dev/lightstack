#!/usr/bin/env bash
# SPEC-009 WS-2 (T016 / FR-008a): a corrupt tenant network row must not block the
# control plane or other tenants. Reconciliation is best-effort per resource.
# Guided test (it mutates the DB, so it asks you to confirm each step):
set -euo pipefail
cat <<'STEPS'
[ws2-partial] Guided test — best-effort reconciliation:

  1) Have a healthy VM + DHCP subnet running.
  2) Inject a bad subnet row (unparseable CIDR):
       docker exec -i o3k-postgres psql -U o3k -d o3k -c \
         "UPDATE subnets SET cidr='not-a-cidr' WHERE id=(SELECT id FROM subnets ORDER BY created_at LIMIT 1);"
  3) Restart o3k, then confirm:
       openstack token issue                 # API becomes ready despite the bad row
       # o3k log shows: reconcile: bad subnet CIDR; skipping  (for the bad subnet)
       # other subnets' dnsmasq still (re)start (pgrep -fa dnsmasq | grep var/lib/o3k/dhcp)
  4) Restore the original CIDR:
       docker exec -i o3k-postgres psql -U o3k -d o3k -c \
         "UPDATE subnets SET cidr='10.0.0.0/24' WHERE cidr='not-a-cidr';"

PASS = API ready + healthy tenant unaffected + only the bad subnet skipped.
STEPS
