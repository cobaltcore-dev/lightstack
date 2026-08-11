# Contract: port creation and address assignment (US1 / F10, plus FR-023a)

**Covers**: FR-001, FR-002, FR-003, FR-004, FR-005, FR-023a
**Applies to**: `POST /v2.0/ports`, `DELETE /v2.0/ports/{id}`, and the DHCP configuration a subnet drives

## Two independent failures behind one symptom

Both report `500 failed to attach TAP to bridge`, and they need different fixes.

### Half A — stub mode: process-local bookkeeping

`BridgeManager.stubBridges` is an in-process `map[string]bool`, written by `createBridgeStub`
(`netns.go:219`) and read by the stub branch of `AttachToBridge` (`:363-368`). It is never persisted
and never rebuilt, so a port on any network created **before the current process** fails.

This is why the ticket's repro fails on a long-running compose stack: `restart: unless-stopped`, and
`networking_mode: stub` from the baked `config/o3k.yaml` (`build/package/Dockerfile:62`).

**Contract**: in stub mode the recorded network is the authority (FR-002). The attach must not consult
the in-process map at all.

### Half B — real mode: namespace mismatch

The port's TAP is the **only** object not already in the root namespace:

| Object | Namespace | Evidence |
|---|---|---|
| Network bridge | root | `network.go:479-480` — `CreateBridge(b, false, "")`, "for libvirt access" |
| dnsmasq | root | `reconciler.go:279,281` — empty `nsName` |
| Egress SNAT | root | `router.go` WS-3, "in the default namespace, where the VMs and their gateway bridge live" |
| **Port TAP** | `light-ns-<project>` | `ports.go:89-90`, `:98`, `:100`, `:115`; delete `:440`, `:471` |

And `light-ns-<project>` is never created on this path — the only `CreateNamespace` caller in the repo
is `auto_allocated_topology.go:113`.

**Contract**: the TAP is created and attached in the **root** namespace (FR-003). The reverse — moving
the bridge into the per-project namespace — is rejected: it breaks libvirt's access, orphans dnsmasq
and the SNAT rules, and would need a veth pair per network (spec A1, research R3).

Per-network L2 isolation continues to come from one bridge per network, which is unaffected.
`auto_allocated_topology.go`'s in-namespace bridge is a separate self-consistent path (it creates the
namespace first) and is left alone.

## Contract tests

| ID | Given | When | Then |
|---|---|---|---|
| **P1** | a network and subnet, same process | `POST /v2.0/ports` | `201`; reports network, MAC, and a fixed IP from the subnet |
| **P2** | a network created **before** a service restart | `POST /v2.0/ports` | `201` — **fails today** with `500 failed to attach TAP to bridge` |
| **P3** | a network absent from the in-process bridge map (unit) | create a port | succeeds; no dependence on process-lifetime state |
| **P4** | `openstack port create --network t-net p1` | CLI, stub stack | port created — the ticket's repro |
| **P5** | real mode | create a port | `ip -br link show type bridge \| grep '^br-'` and the TAP are in the **same** namespace; `ip netns list` shows no orphan `light-ns-*` for it |
| **P6** | real mode, a guest on such a port | boot it | obtains an address; reaches `169.254.169.254`; no `no interfaces have a carrier` |
| **P7** | a shared network owned by another project | tenant creates a port on it | allowed (unchanged); the device goes where **that network's** bridge is, not the caller's |
| **P8** | two tenants each with a network named `t-net` | one creates a port by name | lands on **their own** network |
| **P9** | attach fails (fault injection) | create a port | no TAP left behind, no port row left behind, and the error names the failing step (FR-005) |
| **P10** | a created port | `DELETE /v2.0/ports/{id}` | the TAP is removed from the namespace it was actually created in |
| **P11** | real mode | create then delete 3 ports | no leaked TAPs (`ip -br link` clean) |

## FR-023a — a gatewayless subnet keeps its metadata route

`pkg/networking/dhcp.go:239-241` guards **both** options on the same value:

```
{{ if .GatewayIP }}dhcp-option=3,{{ .GatewayIP }}
dhcp-option=121,169.254.169.254/32,{{ .GatewayIP }},0.0.0.0/0,{{ .GatewayIP }}{{ end }}
```

So honouring `--gateway none` (FR-023) would silently drop `dhcp-option=121` — the classless static
route that carries metadata — and reintroduce the exact guest symptom F10 was filed for. The two are
separate concerns and must be separated (clarify decision):

| Subnet | `dhcp-option=3` (router) | `dhcp-option=121` (metadata route) |
|---|---|---|
| has a gateway | emitted, via the gateway | emitted, via the gateway |
| **no gateway** | **not emitted** | **emitted, via the subnet's on-link address** |

The on-link next hop is the bridge's own address, which `EnsureBridgeIP` already assigns. Upstream
reaches metadata on-link too, so this closes a latent divergence rather than adding one.

| ID | Given | When | Then |
|---|---|---|---|
| **P12** | a gatewayless subnet, DHCP enabled | inspect the generated dnsmasq config | no `dhcp-option=3`; `dhcp-option=121` **present** |
| **P13** | a guest on a gatewayless subnet | boot it | gets an address; **reaches metadata**; has **no** default route |
| **P14** | a subnet whose gateway is removed by update | reconcile | config regenerated: router option gone, metadata route retained |
| **P15** | a subnet with a gateway | unchanged | both options emitted, as today |
