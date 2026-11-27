# eBPF Security Groups - Implementation Status

**Status**: ⚠️ **NOT WIRED** — security groups are enforced in `ebpf` mode by the ports' iptables chains, as in
`iptables` mode; the eBPF filter has no attach point and no caller (see *What eBPF mode does today*)
**Created**: 2026-03-16 (Sprint 69) · **Updated**: 2026-11-23 (cloud-storage#579, SPEC-033 FR-047)

---

## What eBPF mode does today

`networking_mode: ebpf` behaves exactly as `networking_mode: iptables` (SPEC-033 FR-047):

- **Every port is firewalled by its chains** on the node its guest runs on, in both directions and for all
  traffic — between VMs, through floating IPs and for egress — as upstream's iptables driver does: chains
  `ls-i`, `ls-o` and `ls-s` per port, built from its groups, with remote groups resolved to their members'
  addresses (FR-041; `pkg/networking/port_firewall.go`). This needs `br_netfilter`, which Lightstack loads at
  start in either mode (FR-046).
- **The eBPF filter is not used.** It attached to the TAP Neutron itself made for each port. That TAP is gone:
  a guest's TAP is libvirt's, named `tap<port id[:11]>` in the domain, and port creation's eBPF step went with
  Neutron's TAP. Nothing calls the eBPF package (`pkg/networking/ebpf`) any more — its wrapper in
  `pkg/networking`, which translated a port's rules, went with its last caller (SPEC-033 T221) — and the object never loaded in a stock build anyway (an empty object path).

Whether eBPF goes, or is re-targeted at libvirt's TAP and the ports' records, is a decision for the project; it
is not made here. Everything below describes the code as it stands, unreached, and the translation a re-target
would need again.

---

## What's Implemented ✅

### 1. eBPF C Program
**File**: `pkg/networking/ebpf/secgroup.c`

- XDP program `xdp_security_group_filter`, attached to a port's TAP device
- BPF hash map of per-port rule sets (10,000 ports max, 100 rules per port), keyed by a hash of the port's MAC
- Protocol matching: TCP, UDP, ICMP, any; destination port ranges; source IPv4 CIDRs
- Statistics (packets allowed / denied / processed)

### 2. Go Integration Layer
**File**: `pkg/networking/ebpf/secgroup_ebpf.go`

- `SecurityGroupManager` on cilium/ebpf: loads the object, programs the rule map, attaches and detaches XDP
- Statistics API

### 3. Rule Translation
**Removed** with its last caller (SPEC-033 T221): `pkg/networking/security_groups.go` and
`security_groups_ebpf.go` wrapped the package for Neutron, and `ApplySecurityGroupToPort()` turned a port's
Neutron rules into map entries. A re-target writes its own translation; it must fail closed as that one did — a
rule the filter cannot express is dropped, never widened:

- **Egress rules** are skipped; the program filters ingress only.
- **A rule whose only source is `remote_group_id`** is dropped, so it admits nothing: the filter is
  programmed once, at port creation, and cannot follow a group's members. Before cloud-storage#533 it became
  `0.0.0.0/0` and admitted every source.
- **IPv6 rules** are dropped; the program parses IPv4 only. Before, one without a prefix became IPv4
  `0.0.0.0/0`, and one with an IPv6 prefix made the port's whole rule set fail to program.

### 4. Port Lifecycle
**Not wired.** Until SPEC-033, port creation programmed a port's rules and attached the filter to Neutron's own
TAP, and port deletion detached it. Neutron no longer makes a TAP, so both steps went (FR-045, FR-047).

### 5. Build System
**File**: `Makefile`

- `make build-ebpf` - Compiles eBPF C programs with clang
- `make build-with-ebpf` - Builds Lightstack with eBPF support
- `make install-ebpf-tools` - Installs dependencies (clang, llvm, libbpf-dev)

---

## How the Filter Decides

The program runs on the TAP's receive path, so it sees the frames the **VM sends**. For each IPv4 frame it
looks up the rule set of the port the frame is **addressed to**, by destination MAC, and passes the frame
only if one of that port's ingress rules matches its protocol, source address and destination port.

- An IPv4 frame addressed to a MAC **without a rule set** is dropped — that includes the router's gateway
  MAC and broadcast, so DHCP and off-host traffic from the VM do not get through.
- Non-IPv4 frames — ARP and IPv6 among them — pass unfiltered.
- Traffic arriving from outside the host never crosses a local VM's TAP receive path, so this filter does
  not see it.

---

## What's Missing ❌

1. **An attach point.** Neutron's TAP is gone; libvirt's appears when the guest starts, on the node it is
   bound to, which nothing in the eBPF path follows.
2. **Loading.** The object path is empty, the `security_group_mode` and `ebpf_object_path` keys in
   `config/lightstack.yaml` are read by nothing, and the container image does not build `secgroup.o`.
3. **Following the records.** Rules were programmed once, at port creation: a rule change, a membership
   change or a remote group's members never reached the map. The ports' chains follow all three (FR-008b,
   FR-041).
4. **Destinations that are not local ports.** Frames to the gateway, to broadcast, or to anything off-host
   are dropped (see *How the Filter Decides*).
5. **IPv6.** IPv6 frames pass unfiltered, and IPv6 rules are dropped.
6. **Remote groups.** Rules whose only source is a remote group admit nothing.
7. **Egress rules.** Not enforced.

---

## Performance Expectations (When Complete)

| Metric | iptables (Current) | eBPF (Target) | Improvement |
|--------|-------------------|---------------|-------------|
| Rule Application | 10s (1000 rules) | 100ms | **100x** |
| Packet Filtering | 50µs/packet | 5µs/packet | **10x** |
| CPU Usage | 40% | 4% | **10x reduction** |
| Memory | 500MB | 50MB | **10x reduction** |

---

## Using eBPF

```yaml
# config/lightstack.yaml
neutron:
  networking_mode: ebpf
```

The `security_group_mode` and `ebpf_object_path` keys that `config/lightstack.yaml` also shows are **not
read**: until the loader is wired (see *What's Missing*), eBPF mode starts without the XDP filter.

```bash
# Prerequisites (Linux only)
make install-ebpf-tools

# Build eBPF programs
make build-ebpf

# Build lightstack with eBPF support
make build-with-ebpf
```

---

## References

- **eBPF C Program**: `pkg/networking/ebpf/secgroup.c`
- **Go Integration**: `pkg/networking/ebpf/secgroup_ebpf.go`
- **Makefile**: Build targets for eBPF compilation

---

## Status Summary

**Foundation**: ✅ eBPF program, its Go package, build targets; the rule translation was removed with its caller
**Integration**: ❌ Not wired since SPEC-033: no attach point, no caller
**Enforcement**: ✅ in `ebpf` mode by the ports' iptables chains, as in `iptables` mode
