# Contract: the router namespace, DHCP and the network node (FR-033–FR-039, FR-049; R28, R29, R35)

Real mode only. Stub mode stores and reports; nothing is programmed.

## Router `R`, on the network node

| Thing | Shape |
|---|---|
| namespace | `qrouter-<R>`, the whole id, as upstream's; `ip_forward=1`, `arp_ignore=1`, `arp_announce=2`, and `nf_conntrack_tcp_be_liberal=1` once the nat table has loaded connection tracking |
| external leg | veth `qg-<gw port[:11]>` in the namespace / `lg-<gw port[:11]>` on the host |
| addresses on `qg-` | gateway `G/<external prefix>`; each floating `F/32` |
| default route | via the external subnet's gateway, on `qg-`; held by next hop and device, so a gateway on a new port, whose next hop may be the same, takes it onto the new leg before the old leg goes (T316) |
| internal leg, per interface | veth `qr-<port[:11]>` in the namespace, holding every fixed IP of the port with its subnet's prefix — added by subnet, the subnet's gateway `g/<prefix>` (T315) / `lr-<port[:11]>` on bridge `br-<network[:8]>` |
| host, per `G` and `F` | route `<addr>/32 dev lg-…`; `ip neigh add proxy <addr> dev <uplink>`; `lg-…` `proxy_arp=1`; `FORWARD` jump to `ls-l3-fwd` allowing uplink ↔ `lg-…` |

**nat** in the namespace, the whole table replaced in one `iptables-restore`, every rule `-m comment --comment
lightstack`:
```
PREROUTING        -j ls-l3-PREROUTING
OUTPUT            -j ls-l3-OUTPUT
POSTROUTING       -j ls-l3-POSTROUTING
POSTROUTING       -j ls-l3-snat
ls-l3-PREROUTING  -j ls-l3-fip-pf
ls-l3-PREROUTING  -d F/32 -j DNAT --to-destination X
ls-l3-OUTPUT      -d F/32 -j DNAT --to-destination X
ls-l3-POSTROUTING ! -o qg-… -m conntrack ! --ctstate DNAT -j ACCEPT
ls-l3-snat        -j ls-l3-float-snat
ls-l3-snat        -o qg-… -j SNAT --to-source G                                (SNAT on)
ls-l3-snat        -m mark ! --mark 0x2/0xffff -m conntrack --ctstate DNAT -j SNAT --to-source G
ls-l3-float-snat  -s X/32 -j SNAT --to-source F
ls-l3-fip-pf      -j ls-pf-<pf id[:8]>
ls-pf-<id>        -d F/32 -p tcp -m tcp --dport 2222 -j DNAT --to-destination 10.0.0.5:22
```
With SNAT off only `-o qg-… -j SNAT` goes, as upstream's `_add_snat_rules`: the exception for traffic between the
router's own networks and the hairpin rule come from `external_gateway_nat_fip_rules`, which upstream adds whatever
`enable_snat` says. Without them a VM with a floating IP would reach the router's other networks from its floating
address. A router without a gateway has none of the gateway's rules. G is the gateway's first IPv4 address.

**mangle**: `PREROUTING -j ls-l3-mark`, `ls-l3-mark -i qg-… -j MARK --set-xmark 0x2/0xffff`.

**Order**: adding — namespace, legs, `qr-`/`qg-` addresses, tables (one `iptables-restore` commit per table), then
floating addresses on `qg-`, then host routes and proxy entries. Removing — the reverse: proxy entries and routes first,
addresses, tables, legs, namespace last.

## DHCP of network `N`, on the network node

| Thing | Shape |
|---|---|
| namespace | `qdhcp-<N>` |
| device | veth `tap<dhcp port[:11]>` in the namespace (DHCP addresses `/<prefix>` per subnet, `169.254.169.254/32`) / `ld-<dhcp port[:11]>` on `br-<N[:8]>` |
| `dnsmasq` | in the namespace; `--dhcp-range=set:subnet-<id>,<net>,static,<mask>,<lease>s` per DHCP subnet; a host entry per port with an address; option 3 the gateway; option 121 `169.254.169.254/32,<DHCP address>` and `0.0.0.0/0,<gateway>`; option 6 the subnet's DNS servers. A gateway or DNS server that is not an IPv4 address is left out and logged (FR-060) |
| metadata | Lightstack listening on `169.254.169.254:80` inside; instance = (network `N`, source address) |

## Bounds and failure

Every command runs through the command runner under the unit's shared deadline (900 ms from the call's receipt, lock
wait included; `node-apply.md`). Any failure fails the unit: the caller answers `500` and undoes (FR-008) or re-applies
(FR-040).

## Reconciliation

On the network node: lists `qrouter-*`, `qdhcp-*`, `lg-*`, host proxy entries and `ls-l3-fwd`; converges each unit with
a record; removes namespaces, legs, entries and listeners of units without one, after a complete read only. Compares
before writing: a unit that matches is left untouched, and one a pass cut short is finished, since its namespace's
sysctls, `lo` and each leg's bridge and up state are read too (SPEC-033 T191). On every other node: removes any
`qrouter-*`/`qdhcp-*` it finds (a node that was the network node before), and the retired state of FR-044.

## Restarts (FR-036)

Namespaces live under the host's `/run/netns` (chart: `hostPath` `/run/netns`, `mountPropagation: Bidirectional`;
Compose: `/run/netns:/run/netns:rshared`). A restarted process finds them and changes nothing that matches. Its first
pass restarts each network's `dnsmasq`, which died with the old process's pod, and re-opens the metadata listeners;
leases already granted run on meanwhile.
