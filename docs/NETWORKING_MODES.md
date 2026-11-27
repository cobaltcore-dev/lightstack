# Lightstack Networking Modes Deployment Guide

## Overview

Lightstack supports three networking modes, each with different performance characteristics and requirements:

1. **Stub Mode** - Testing and development (no kernel dependencies)
2. **IPTables Mode** - Traditional Linux networking with iptables security groups
3. **eBPF Mode** - accepted as configuration; today it behaves exactly as IPTables mode (`docs/EBPF_STATUS.md`)

## Architecture

The real modes share one layout, upstream's for a network node (SPEC-033); stub mode programs none of
it:

```
 every node                                   the network node, besides
 ┌──────────────────────────────────────┐     ┌───────────────────────────────────────────┐
 │ br-<network[:8]> per network,         │     │ qrouter-<router> per router:              │
 │   no address                          │     │   qg- leg to the host (gateway, floating  │
 │ the guests' TAPs, tap<port[:11]>,     │     │   addresses, NAT), qr- leg per interface  │
 │   libvirt's                           │     │   on its network's bridge                 │
 │ vxlan-<network[:8]> per network       │     │ qdhcp-<network> per network with DHCP:    │
 │   (multi-node)                        │     │   dnsmasq, the metadata listener          │
 │ the chains of the ports bound to it   │     │ proxy ARP on the uplink for the routers'  │
 │   (ls-i/ls-o/ls-s per port)           │     │   and floating addresses                  │
 └──────────────────────────────────────┘     └───────────────────────────────────────────┘
```

A request that changes host state stores it, then has the node holding that state apply it before the
answer: the network node for routers, DHCP and floating IPs, a port's node for its chains — a function
call on that node itself, an authenticated one-second HTTP call otherwise (`/internal/v1/converge`).
When that fails, the change is undone and the request answers `500`. Each node's reconciler converges
what it holds to the records, both ways.

**Key Insight**: IPTables and eBPF modes are the same today: the ports' iptables chains enforce security
groups in both (see `docs/EBPF_STATUS.md`).

---

## Mode 1: Stub Mode

### Purpose
- Development and testing without kernel dependencies
- CI/CD environments
- macOS/Windows development

### Configuration
```yaml
neutron:
  networking_mode: stub
```

### Requirements
- None! Works on any OS

### Behavior
- Simulates all networking operations in memory
- No actual network namespaces, bridges, or TAP devices created
- Perfect for API testing and development
- Full OpenStack API compatibility

### Use Cases
- Local development on macOS/Windows
- Unit testing
- API integration testing
- CI/CD pipelines

---

## Mode 2: IPTables Mode

### Purpose
- Production deployments with traditional Linux networking
- Battle-tested, well-understood security model
- Compatible with all Linux kernels (2.6+)

### Configuration
```yaml
neutron:
  networking_mode: iptables
```

### Requirements

**System:**
- Linux kernel 2.6+ (3.10+ recommended)
- `ip` command (iproute2 package)
- `iptables` command
- Root or CAP_NET_ADMIN privileges

**Packages (Ubuntu/Debian):**
```bash
sudo apt-get install -y iproute2 iptables bridge-utils dnsmasq
```

**Packages (RHEL/CentOS/Fedora):**
```bash
sudo dnf install -y iproute iptables bridge-utils dnsmasq
```

### How It Works

1. **A bridge per network, on every node**, `br-<network id[:8]>`, holding no address. Isolation comes
   from the bridge. Bridges, the guests' TAPs and the VXLAN devices live in the namespace Lightstack
   itself runs in, because libvirt has to reach them there.

2. **The guest's TAP is libvirt's**, named `tap<port id[:11]>` in the domain, as upstream names it, so
   the firewall knows which port a TAP is. Nova binds the port to the node (`binding:host_id`) and has
   its chains in force before the guest starts.

3. **DHCP and metadata on the network node**: per network with DHCP, a namespace `qdhcp-<network id>`
   holding the network's DHCP port's addresses and `169.254.169.254`, its leg `ld-…` on the bridge; one
   dnsmasq inside, with static ranges, so only ports with a reservation get a lease, and option 121
   routing the metadata address to the DHCP address. Lightstack serves metadata in the namespace itself
   and identifies a guest by its network and address.

4. **Routers on the network node**: per router, a namespace `qrouter-<router id>`. Its gateway leg
   `qg-…`/`lg-…` holds the gateway's and the floating addresses; an interface leg `qr-…`/`lr-…` per
   subnet joins that network's bridge with the subnet's gateway address. Floating-IP DNAT and SNAT, the
   router's SNAT and port forwardings are iptables rules inside the namespace. On the host, the router's
   and floating addresses are routed to `lg-…` and answered for on the uplink by proxy ARP.

5. **The port firewall, on each port's node**, as upstream's iptables driver: `ls-filter-top` from
   `FORWARD` and `INPUT`, a `physdev` dispatch per TAP to the port's chains `ls-i<port[:10]>` (ingress),
   `ls-o<port[:10]>` (egress) and `ls-s<port[:10]>` (its addresses with its MAC), one `RETURN` per rule
   of its groups, remote groups resolved to their members' addresses, replies by connection tracking —
   one zone per network, so overlapping CIDRs do not mix — and everything else dropped. A TAP without a
   bound port is dropped; the router and DHCP legs are trusted. Changes go in one `iptables-restore`
   and one `ip6tables-restore` per node.

Needed on the node: `ip` (iproute2), `iptables` and `ip6tables` with their `-save`/`-restore`, the
`br_netfilter` module (Lightstack loads it and sets `net.bridge.bridge-nf-call-iptables` and
`-ip6tables` to `1`; readiness fails while they do not hold), the `physdev`, `conntrack` and `comment`
matches, and, on the network node, `dnsmasq`.

### Pros
- Upstream's iptables driver's chains: well understood, and inspectable with `iptables-save`
- Works on every kernel with `br_netfilter`, `physdev`, `conntrack` and `comment`
- One `iptables-restore` and one `ip6tables-restore` per node per change

### Cons
- A large number of ports and rules makes long chains; Lightstack publishes no benchmark

---

## Mode 3: eBPF Mode

### Purpose
Reserved for an eBPF dataplane. Today it is IPTables mode under another name.

### Configuration
```yaml
neutron:
  networking_mode: ebpf
```

### Requirements, behaviour and performance

Exactly as IPTables mode: the ports' iptables chains enforce security groups (SPEC-033 FR-047), so the node
needs what IPTables mode needs and nothing more — no `bpftool`, no BTF — and the two modes perform alike. The
eBPF filter attached to a TAP Neutron itself made; that TAP is gone, and the filter is not wired to
libvirt's. See `docs/EBPF_STATUS.md`.

---

## Performance

The real modes enforce through the same iptables chains, so they perform alike. Lightstack publishes no
benchmark of either.

---

## Deployment Examples

### Example 1: Development on macOS (Stub Mode)
```yaml
# config/lightstack.yaml
neutron:
  networking_mode: stub
```

```bash
# No special requirements
go run cmd/lightstack/main.go --config config/lightstack.yaml
```

### Example 2: Production on Ubuntu 22.04 (IPTables Mode)
```bash
# Install dependencies
sudo apt-get update
sudo apt-get install -y iproute2 iptables bridge-utils dnsmasq

# Configure lightstack
cat > /etc/lightstack/lightstack.yaml <<EOF
neutron:
  networking_mode: iptables
EOF

# Run lightstack with systemd
sudo systemctl start lightstack
```

### Example 3: eBPF Mode
`networking_mode: ebpf` needs exactly what Example 2 installs, and behaves as IPTables mode.

---

## Switching Between Modes

To switch modes, simply:

1. Stop Lightstack
2. Update `config/lightstack.yaml` with desired `networking_mode`
3. Restart Lightstack

**Note**: Existing networks/VMs will need to be recreated when switching modes.

---

## Testing Each Mode

### Test Stub Mode (Works on any OS)
```bash
# Start lightstack
go run cmd/lightstack/main.go --config config/lightstack.yaml

# Create network
openstack network create test-network

# Verify in database
psql -d lightstack -c "SELECT id, name, status FROM networks;"
```

### Test IPTables Mode (Linux only)
```bash
# Start lightstack
sudo go run cmd/lightstack/main.go --config config/lightstack.yaml

# Create network
openstack network create test-network

# Verify bridge created (no namespace is created; the bridge is where Lightstack runs)
ip link show type bridge | grep br-
```

### Test eBPF Mode
As IPTables mode: the same bridge, and the same port chains (`iptables-save | grep -- '-A ls-'`).

---

## Troubleshooting

### Common Issues

**Issue**: `exec: "ip": executable file not found`
- **Cause**: Running iptables or eBPF mode on a non-Linux OS
- **Solution**: Use stub mode for development, or deploy on Linux

**Issue**: `/readyz` answers `503` with `bridge-netfilter unavailable`; the log says `bridge netfilter not in force`
- **Cause**: `br_netfilter` is not loaded, or `net.bridge.bridge-nf-call-iptables` or `-ip6tables` is not `1`.
  Lightstack sets both at start when it can: it needs `/lib/modules` and the privilege to load the module
- **Solution**: On the host, `modprobe br_netfilter` and
  `sysctl -w net.bridge.bridge-nf-call-iptables=1 net.bridge.bridge-nf-call-ip6tables=1`

**Issue**: `exec: "iptables-restore": executable file not found` (or `iptables-save`, `ip6tables-restore`,
`ip6tables-save`)
- **Cause**: Missing iptables package
- **Solution**: Install iptables with its `-save`/`-restore` tools for IPv4 and IPv6, and run as root

**Issue**: `permission denied` when creating namespace
- **Cause**: Insufficient privileges
- **Solution**: Run Lightstack with sudo or CAP_NET_ADMIN capability

---

## Recommendations

**Choose Stub Mode if:**
- Developing on macOS/Windows
- Running CI/CD tests
- Don't need actual networking

**Choose IPTables Mode** for every real deployment.

**eBPF Mode** gives nothing beyond IPTables mode today.

---

## Future Enhancements

### Not started: an eBPF dataplane
- [ ] XDP (eXpress Data Path) support for even faster processing
- [ ] BPF map-based rule updates (no program reload needed)
- [ ] Per-connection state tracking in BPF maps
- [ ] Integration with Cilium for advanced networking features
- [ ] eBPF-based load balancing
