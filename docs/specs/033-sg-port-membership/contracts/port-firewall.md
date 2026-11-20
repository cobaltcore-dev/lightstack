# Contract: the port firewall (FR-041–FR-043, FR-046, FR-047; R32, R35)

Real mode, `iptables` and `ebpf` modes alike. Applied on the node in the port's `binding:host_id`.

## The TAP

Nova's domain XML: `<interface type='bridge'><mac address='…'/><source bridge='br-<network[:8]>'/><target dev='tap<port id[:11]>'/>…`. Neutron creates no TAP of its own.

## Chains, per family (`iptables`, `ip6tables`), table `filter`

```
FORWARD      -j ls-filter-top        (first)
INPUT        -j ls-filter-top        (first)
ls-filter-top -j ls-local
ls-local     -m physdev --physdev-out tapX --physdev-is-bridged -j ls-sg-chain
ls-local     -m physdev --physdev-in  tapX --physdev-is-bridged -j ls-sg-chain
ls-sg-chain  -m physdev --physdev-out tapX --physdev-is-bridged -j ls-i<port[:10]>
ls-sg-chain  -m physdev --physdev-in  tapX --physdev-is-bridged -j ls-o<port[:10]>
ls-sg-chain  -j ACCEPT
ls-sg-fallback -j DROP
```
INPUT's `--physdev-in tapX` jumps straight to `ls-o<port>` (VM to host), as upstream.

**`ls-o<port>`** (egress): DHCP request (`-s 0.0.0.0/32 -d 255.255.255.255/32 -p udp --sport 68 --dport 67 -j RETURN`), IPv6 neighbour discovery, `-j ls-s<port>`, DHCP request `RETURN`, DHCP server answer from the VM `DROP`, then the rule block.

**`ls-s<port>`** (spoof): `-s <fixed IP> -m mac --mac-source <MAC> -j RETURN` per fixed IP; `-j DROP`.

**`ls-i<port>`** (ingress): DHCP answers to the port's addresses and broadcast, IPv6 ICMP as upstream, then the rule block.

**Rule block**: `-m state --state RELATED,ESTABLISHED -j RETURN`; one `RETURN` per rule (protocol, ports or ICMP type/code, `-s`/`-d` for `remote_ip_prefix`, one rule per remote-group member address excluding the port's own); `-m state --state INVALID -j DROP`; `-j ls-sg-fallback`.

**Trusted ports** (`network:*`), by their host-side device: `ls-local -m physdev --physdev-{in,out} lr-<port[:11]> --physdev-is-bridged -j ACCEPT` for a router interface, and the same with `ld-<port[:11]>` for a DHCP port.

**Zones**, table `raw`: `PREROUTING -m physdev --physdev-in tapX -j CT --zone <network ct_zone>` and `-i br-<network[:8]> -j CT --zone <same>`. A port whose groups are stateless gets `-j CT --notrack` instead, and its chains carry no `RELATED,ESTABLISHED` and no `INVALID` rule (FR-051), as upstream.

**Binding moves and boot**: a port whose binding moves or is cleared is converged `absent` on its old node in the same request. At boot and attach, Nova converges the port's chains on its node before `DomainCreate`/`AttachDevice`; a failure fails the boot (`ERROR`, fault) or the attach (`500`).

## Apply

One call per node with its affected ports (node-apply contract). The node compares each chain with `-S`, and replaces changed and removed chains, their dispatch and zone rules, in one `iptables-restore --noflush` and one `ip6tables-restore --noflush` commit, within the call's shared deadline. A port with no group has the block with no rule: replies only, and its own DHCP and neighbour discovery.

## Prerequisites (FR-046)

At start: `modprobe br_netfilter`; `1` into `/proc/sys/net/bridge/bridge-nf-call-iptables` and `-ip6tables`; read back. Readiness check `bridge-netfilter` fails, naming the reason, until both hold. Chart: `/lib/modules` mounted read-only.

## Starting rules (FR-043)

New group: egress IPv4 and IPv6, no remote. `default`: those and ingress IPv4 and IPv6 with `remote_group_id` = itself.

## Reconciliation

Every node converges the chains of its bound ports, both ways; port chains (`ls-i`/`ls-o`/`ls-s` + ten characters of a port id) of no bound port are removed after a complete read, with their dispatch and zone rules. The fixed chains (`ls-filter-top`, `ls-local`, `ls-sg-chain`, `ls-sg-fallback`) are never removed by reconciliation, only by teardown. Retired chains (`LS-SG-*`, `LS-NSI-*`) are removed (FR-042, FR-044).
