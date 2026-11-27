# Contract: the port firewall (FR-041–FR-043, FR-046, FR-047; R32, R35)

Real mode, `iptables` and `ebpf` modes alike. Applied on the node in the port's `binding:host_id`.

## The TAP

Nova's domain XML: `<interface type='bridge'><mac address='…'/><source bridge='br-<network[:8]>'/>`
`<target dev='tap<port id[:11]>'/>…`. Neutron creates no TAP of its own. Every interface of every domain — a
booting guest's and a revived one's — names its port's, derived from the port id alone: libvirt's own `vnetN`
would match neither the port's chains nor the `tap+` guards *(SPEC-033 T173, T280)*.

## Chains, per family (`iptables`, `ip6tables`), table `filter`

```
FORWARD      -j ls-filter-top        (first)
INPUT        -j ls-filter-top        (first)
ls-filter-top -j ls-local
ls-local     -i <bridge> -m physdev --physdev-in  tap+ -j ls-bound-in                         (per bridge)
ls-local     -o <bridge> -m physdev --physdev-out tap+ --physdev-is-bridged -j ls-bound-out   (per bridge)
ls-local     -m physdev --physdev-out tapX --physdev-is-bridged -j ls-sg-chain
ls-local     -m physdev --physdev-in  tapX --physdev-is-bridged -j ls-sg-chain
ls-bound-in  -m physdev --physdev-in  tapX -j RETURN                                         (per port)
ls-bound-in  -j DROP
ls-bound-out -m physdev --physdev-out tapX -j RETURN                                         (per port)
ls-bound-out -j DROP
ls-sg-chain  -m physdev --physdev-out tapX --physdev-is-bridged -j ls-i<port[:10]>
ls-sg-chain  -m physdev --physdev-in  tapX --physdev-is-bridged -j ls-o<port[:10]>
ls-sg-chain  -j ACCEPT
ls-sg-fallback -j DROP
```
INPUT's `--physdev-in tapX` jumps straight to `ls-o<port>` (VM to host), as upstream, once the `tap+` guard has let
its TAP through.

**`ls-o<port>`** (egress): DHCP request (`-s 0.0.0.0/32 -d 255.255.255.255/32 -p udp --sport 68 --dport 67 -j RETURN`),
IPv6 neighbour discovery, `-j ls-s<port>`, DHCP request `RETURN`, DHCP server answer from the VM `DROP`, then the rule
block.

**`ls-s<port>`** (spoof): `-s <fixed IP> -m mac --mac-source <MAC> -j RETURN` (the MAC in lower case, as `iptables-save`
prints it back, so an unchanged chain compares equal) per fixed IP of the family, in IPv6 with the link-local address
its MAC gives it; `-j DROP`. A family gets the chain, and `ls-o<port>` its jump, only when the port has an address in
it, or no address at all (the MAC alone), as upstream's iptables driver: an IPv4-only port's IPv6, link-local included,
goes on to its rules (SPEC-033 T214).

**`ls-i<port>`** (ingress): DHCP answers to the port's addresses and broadcast, IPv6 ICMP as upstream, then the rule
block.

**Rule block**: `-m state --state RELATED,ESTABLISHED -j RETURN`; one `RETURN` per rule (protocol, ports or ICMP
type/code, `-s`/`-d` for `remote_ip_prefix`, one rule per remote-group member address excluding the port's own); `-m
state --state INVALID -j DROP`; `-j ls-sg-fallback`.

*(FR-059; SPEC-033 T169, T249, T277.)* A rule is written only as the renderer parses it: a remote as an address
or prefix of the rule's family, masked; a protocol as upstream's number for it — by name exactly where
`iptables -S` and `ip6tables -S` print one, the table built into iptables (`icmp` 1, `tcp` 6, `udp` 17, `dccp` 33,
`esp` 50, `ah` 51, `ipv6-icmp` 58, `ipcomp` 108, `sctp` 132, `mobility-header` 135, `udplite` 136), `ipv6-icmp`
for ICMP in an IPv6 rule, none for `any` and protocol 0, else the number: `gre` is `-p 47`, `vrrp` `-p 112`, since
the image's `iptables-restore`, without `/etc/protocols`, refuses any other name, and a number iptables prints by
name would never compare equal. Ports with `-m tcp`/`udp`/`sctp`/`dccp --dport`, UDP-Lite's with
`-m multiport --dports`, ICMP's type and code with `-m icmp`/`icmp6` — an IPv4 type 255 without a code as
`--icmp-type any`, which is how iptables reads and prints it. A rule it cannot express exactly — another family's
remote, a protocol upstream does not name, ports on a protocol without them, values out of range — is left out
and logged: a rule only allows, so leaving it out allows less, where writing it would put its owner's text into
`iptables-restore`'s input or fail the node's whole commit.

**A TAP without a bound port**, its port deleted while its guest runs (Lightstack has no event that would detach
it), has no chains, and its frames are dropped both ways: from it, which a bound port's dispatch would otherwise
send through the bound port's chain alone and accept — spoofed sources, DHCP answers — or which, addressed to this
host, INPUT would leave to the host's policy; and to it. Before any port's dispatch, every frame from a `tap+` on a
network's bridge, forwarded or delivered to this host — so its guard does not ask for `--physdev-is-bridged`, which
a frame to this host never matches — goes to `ls-bound-in`, and every frame the bridge forwards to one to
`ls-bound-out`, per network bridge the node holds or its ports join (`br-` and eight hex digits); there a bound
port's TAP returns and any other is dropped. A TAP of another program on a bridge of its own keeps its traffic
*(SPEC-033 T213, T248, T314)*. A reorder alone — bound ports' `--physdev-in` dispatch, then the
`tap+` drop — would stop the orphan's own frames but still let a bound port's reach it on that port's egress
alone: iptables cannot tell, after a chain returns, which end it checked, so each direction needs its set of
bound TAPs. *(Implementation, 2026-11-23: fail closed; upstream's agent never sees such a TAP, as Nova detaches
the interface.)*

**Trusted ports** (`network:*`), by their host-side device, after every port's dispatch: `ls-local -m physdev
--physdev-{in,out} lr+ --physdev-is-bridged -j ACCEPT` for routers' interfaces, and the same with `ld+` for networks'
DHCP ports. *(Implementation, 2026-11-23: by wildcard, as the router's `lg+` forward chain, so a router or DHCP port
needs no binding and no trigger; only Lightstack makes `lr-` and `ld-` devices, and only on the network node.)*

**Zones**, table `raw`, in the chain `ls-ct` jumped first from `PREROUTING`, so one restore replaces them without
touching anyone else's rules: `-m physdev --physdev-in tapX -j CT --zone <network ct_zone>` per port, then `-i
br-<network[:8]> -j CT --zone <same>` per bridge of its bound ports. The first `CT` a packet meets holds. A port whose
groups are stateless gets `-j CT --notrack` instead, and its chains carry no `RELATED,ESTABLISHED` and no `INVALID` rule
(FR-051), as upstream.

**Binding moves and boot**: a port whose binding moves or is cleared is converged `absent` on its old node in the same
request. At boot and attach, Nova converges the port's chains on its node before `DomainCreate`/`AttachDevice`; a
failure fails the boot (`ERROR`, fault) or the attach (`500`).

## Apply

One call per node with its affected ports (node-apply contract). The node renders every port bound to it from the
records, reads its `filter` and `raw` tables once per family (`iptables-save -t <table>`), compares chain by chain, and
replaces changed and removed chains, their dispatch and zone rules, in one `iptables-restore --noflush` and one
`ip6tables-restore --noflush` run, within the call's shared deadline. A port with no group has the block with no rule:
replies only, and its own DHCP and neighbour discovery. The node also makes its bound ports' bridges, which libvirt
needs before it starts the guest.

## Prerequisites (FR-046)

At start: `modprobe br_netfilter`; `1` into `/proc/sys/net/bridge/bridge-nf-call-iptables` and `-ip6tables`; read back.
Readiness check `bridge-netfilter` fails, naming the reason, until both hold. Chart: `/lib/modules` mounted read-only.

## Starting rules (FR-043)

New group: egress IPv4 and IPv6, no remote. `default`: those and ingress IPv4 and IPv6 with `remote_group_id` = itself.

## Reconciliation

Every node converges the chains of its bound ports, both ways; port chains (`ls-i`/`ls-o`/`ls-s` + ten characters
of a port id) of no bound port are removed after a complete read, with their dispatch and zone rules. The fixed
chains (`ls-filter-top`, `ls-local`, `ls-bound-in`, `ls-bound-out`, `ls-sg-chain`, `ls-sg-fallback`, and the raw
table's `ls-ct`) are never removed by reconciliation, only by teardown. Retired chains (`LS-SG-*`, `LS-NSI-*`) are
removed (FR-042, FR-044); given the teardown's records, so are the two `FORWARD` accepts the earlier layout's
egress SNAT wrote per subnet beside its masquerade (`-s <subnet> -o <uplink> -j ACCEPT` and its
`RELATED,ESTABLISHED` return), nothing else in them *(SPEC-033 T278)*.
