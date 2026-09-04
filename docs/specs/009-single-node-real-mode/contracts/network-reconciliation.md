# Contract: Network-State Reconciliation & Durability (WS-2)

External-observable contract: a running VM's networking and disk survive a control-plane (pod) restart, and runtime drift self-heals. Verified by restarting the Lightstack process/pod and observing from the host + the guest.

## Behavioral contracts

| ID | Given | When | Then |
|----|-------|------|------|
| C-WS2-1 | a running VM with a DHCP-assigned IP | the Lightstack control plane is restarted/upgraded | the VM keeps the **same** IP and connectivity; no manual action. |
| C-WS2-2 | a running VM | its dnsmasq is killed out-of-band | within one resync interval, dnsmasq is respawned and the VM's networking is restored. |
| C-WS2-3 | a running VM | the control plane is restarted **twice** | no duplicate netns/bridges/taps/leases/rules accumulate (idempotent). |
| C-WS2-4 | two tenants, one with corrupt/un-rebuildable network state | the control plane restarts | the API still becomes ready; the healthy tenant is unaffected; the bad one is logged and retried next tick. |
| C-WS2-5 | a VM with data written to its disk + an attached local volume | the control plane restarts | disk and volume data are intact (persisted on the node hostPath). |

## Contract tests (RED-first)

- `test/ws2_restart_survival_test.sh` (host, real mode): C-WS2-1/3/5 — note the VM's IP (dnsmasq lease / `virsh domifaddr`), restart lightstack, re-check IP + `ping` from a peer netns; write a sentinel to disk/volume before, verify after; run restart twice and diff `ip netns`/`brctl`/iptables counts.
- `test/ws2_selfheal_test.sh`: C-WS2-2 — `kill` the dnsmasq PID, wait one interval, assert a new dnsmasq serves the lease.
- `test/ws2_partial_failure_test.sh`: C-WS2-4 — inject a bad row (e.g. a port referencing a missing subnet), restart, assert `/healthz`/`/readyz` ready and the other tenant's VM reachable; assert a log line for the failed resource.

## Notes
- Reconciler models the existing VXLAN coordinator (`Start(ctx)` initial + ticker); interval is `neutron.reconcile_interval` (default 30 s).
- IP stability comes from DB-driven dnsmasq MAC→IP host reservations (research R5), not lease-file preservation.
- Out of scope: VM auto-resume after a full host reboot (networking is rebuilt; the stopped domain is not auto-started in v1).
