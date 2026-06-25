# Contract: North-South Networking (WS-3, single node)

External interface = OpenStack Network (Neutron) v2.0 routers, external networks, floating IPs, and security groups, via `openstack` CLI / gophercloud / Terraform. Reuses the existing router SNAT + floating-IP DNAT code (activated in real mode, auto-wired to the node uplink).

## Behavioral contracts

| ID | Given | When | Then |
|----|-------|------|------|
| C-WS3-1 | a VM on a tenant network attached to a router with an external gateway | the VM initiates outbound traffic | it reaches an external host (e.g. `ping`/`curl 1.1.1.1`), masqueraded out the node's primary uplink — no operator-created bridge/NIC. |
| C-WS3-2 | a VM and an operator-configured floating-IP pool (external/provider network) | allocate + associate a floating IP | an external client reaches the VM at the floating IP (on ports its SG allows); the VM's egress appears from the floating IP. |
| C-WS3-3 | a VM with a floating IP but no inbound SG rule for a port | external client connects to that port | blocked; after `openstack security group rule create ... --dst-port P`, the connection succeeds. |
| C-WS3-4 | a VM with an associated floating IP | disassociate / release it | external access via that address stops; the address returns to the pool. |
| C-WS3-TF | a Terraform config with `openstack_networking_floatingip_v2` + association | `terraform apply` | floating IP allocate/associate/disassociate/release behave as upstream, unmodified. |

## Contract tests (RED-first)

- `test/ws3_egress_test.sh` (host, real mode): C-WS3-1 — create network/subnet/router/ext-gw, boot a VM, from the guest reach an external host; assert a MASQUERADE rule on the uplink.
- `test/ws3_floatingip_test.sh`: C-WS3-2/4 — allocate/associate a FIP; from outside the node reach the VM; disassociate and assert access stops; assert the DNAT/SNAT rules appear/disappear.
- `test/ws3_secgroup_test.sh`: C-WS3-3 — FIP reachable only on allowed ports; opening an SG rule enables a port.
- `test/terraform/ws3/`: C-WS3-TF.

## Notes
- Egress requires a router + external gateway (standard OpenStack); a router-less network has no egress. "No operator setup" means O3K auto-maps the external network to the uplink (default-route iface) — no `br-ex`/NIC config (research R10).
- Floating-IP pool is a standard external/provider network; the operator supplies the range + routing (MetalLB / routed-or-L2 subnet) — documented in quickstart.
- SGs apply to north-south via standard netfilter ordering (DNAT in PREROUTING, then the SG filter chain sees the fixed IP).
- IPv4 only. Routing a FIP to a *hosting* node is multi-node (out of scope).
