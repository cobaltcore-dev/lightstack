# eBPF Security Groups - Implementation Status

**Status**: ⚠️ **PARTIAL IMPLEMENTATION** — wired into the port lifecycle, but the eBPF object does not load
in a stock build, so eBPF mode currently adds no east-west filtering (see *What eBPF mode does today*)
**Created**: 2026-03-16 (Sprint 69) · **Updated**: 2026-10-01 (cloud-storage#533)

---

## What eBPF mode does today

`networking_mode: ebpf` currently behaves like `networking_mode: iptables`:

- The **north-south** ingress filter applies in both modes. It is the per-fixed-IP iptables chain built from
  `port_security_groups` when a floating IP is associated, and rebuilt when a group's rules change
  (`RouterManager.EnsureIngressFilter`, `pkg/networking/northsouth.go`).
- The **east-west** XDP filter does not exist. `neutron.NewService` creates the security-group manager with
  an empty object path (`internal/neutron/network.go:40`), `ebpf.LoadCollectionSpec("")` fails, Neutron
  logs `failed to initialize security group manager` and runs without a manager, and `CreatePort` skips the
  eBPF block: no rules are programmed and no filter is attached.

Everything below describes the code path that runs once the object loads.

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
**File**: `pkg/networking/security_groups_ebpf.go`

`ApplySecurityGroupToPort()` turns a port's Neutron rules into map entries. It fails closed — a rule the
filter cannot express is dropped, never widened:

- **Egress rules** are skipped; the program filters ingress only.
- **A rule whose only source is `remote_group_id`** is dropped, so it admits nothing: the filter is
  programmed once, at port creation, and cannot follow a group's members. Before cloud-storage#533 it became
  `0.0.0.0/0` and admitted every source.
- **IPv6 rules** are dropped; the program parses IPv4 only. Before, one without a prefix became IPv4
  `0.0.0.0/0`, and one with an IPv6 prefix made the port's whole rule set fail to program.

### 4. Port Lifecycle
**File**: `internal/neutron/ports.go`

- **Create**: `security_groups` is accepted and stored in `port_security_groups` (migrations 006 and 053).
  In eBPF mode, for a port with a fixed IP, the rules of its groups are fetched and programmed, and the XDP
  filter is attached to the TAP **whether or not programming succeeded** — attaching only on success left
  everything that port sent unchecked. If the attach itself fails, the port is **not created**: the request
  answers `500` and the port row and TAP are rolled back, so no interface exists whose traffic skips the
  filter.
- **Delete**: the XDP filter is detached and the port's rule set removed.

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
  not see it; the north-south iptables filter covers floating-IP ingress.

---

## What's Missing ❌

1. **Loading.** The object path is hard-coded empty (`internal/neutron/network.go:40`), the
   `security_group_mode` and `ebpf_object_path` keys in `config/lightstack.yaml` are read by nothing, and the
   container image does not build `secgroup.o`.
2. **Rule synchronization.** Creating or deleting a security-group rule refreshes only the north-south filter
   (`refreshIngressFiltersForSG`); the eBPF map keeps the rules the port was created with.
3. **Security-group changes on an existing port.** Rules are programmed once, at port creation.
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
- **Security Group Manager**: `pkg/networking/security_groups.go`
- **Security Group Helpers**: `pkg/networking/security_groups_ebpf.go`
- **Port lifecycle**: `internal/neutron/ports.go` (`CreatePort`, `DeletePort`)
- **North-south filter**: `pkg/networking/northsouth.go`
- **Makefile**: Build targets for eBPF compilation

---

## Status Summary

**Foundation**: ✅ eBPF program, Go wrapper, fail-closed rule translation, build targets
**Integration**: ⚠️ Port create and delete wired; no rule sync, no security-group changes on existing ports
**Usability**: ❌ Does not load in a stock build — eBPF mode currently equals iptables mode
