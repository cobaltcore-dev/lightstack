# Contract: the router namespace, DHCP and the network node (FR-033–FR-039, FR-049; R28, R29, R35)

Real mode only. Stub mode stores and reports; nothing is programmed.

## Router `R`, on the network node

| Thing | Shape |
|---|---|
| namespace | `qrouter-<R>`; `ip_forward=1`, `arp_ignore=1`, `arp_announce=2`, `nf_conntrack_tcp_be_liberal=1` |
| external leg | veth `qg-<gw port[:11]>` in the namespace / `lg-<gw port[:11]>` on the host |
| addresses on `qg-` | gateway `G/<external prefix>`; each floating `F/32` |
| default route | via the external subnet's gateway, on `qg-` |
| internal leg, per interface | veth `qr-<port[:11]>` in the namespace, holding the subnet's gateway `g/<prefix>` / `lr-<port[:11]>` on bridge `br-<network[:8]>` |
| host, per `G` and `F` | route `<addr>/32 dev lg-…`; `ip neigh add proxy <addr> dev <uplink>`; `lg-…` `proxy_arp=1`; `FORWARD` jump to `ls-l3-fwd` allowing uplink ↔ `lg-…` |

**nat** in the namespace, every rule `-m comment --comment lightstack`:
```
ls-l3-PREROUTING  -d F/32 -j DNAT --to-destination X
ls-l3-OUTPUT      -d F/32 -j DNAT --to-destination X
ls-l3-float-snat  -s X/32 -j SNAT --to-source F
ls-l3-POSTROUTING ! -o qg-… -m conntrack ! --ctstate DNAT -j ACCEPT          (SNAT on)
ls-l3-snat        -o qg-… -j SNAT --to-source G                                (SNAT on)
ls-l3-snat        -m mark ! --mark 0x2/0xffff -m conntrack --ctstate DNAT -j SNAT --to-source G
ls-l3-PREROUTING  -j ls-l3-fip-pf
ls-l3-fip-pf      -j ls-pf-<pf id[:8]>
ls-pf-<id>        -d F/32 -p tcp -m tcp --dport 2222 -j DNAT --to-destination 10.0.0.5:22
```
**mangle**: `-i qg-… -j MARK --set-xmark 0x2/0xffff`.

**Order**: adding — namespace, legs, `qr-`/`qg-` addresses, tables (one `iptables-restore` commit per table), then floating addresses on `qg-`, then host routes and proxy entries. Removing — the reverse: proxy entries and routes first, addresses, tables, legs, namespace last.

## DHCP of network `N`, on the network node

| Thing | Shape |
|---|---|
| namespace | `qdhcp-<N>` |
| device | veth `tap<dhcp port[:11]>` in the namespace (DHCP addresses `/<prefix>` per subnet, `169.254.169.254/32`) / `ld-<dhcp port[:11]>` on `br-<N[:8]>` |
| `dnsmasq` | in the namespace; `--dhcp-range=set:subnet-<id>,<net>,static,<mask>,<lease>s` per DHCP subnet; a host entry per port with an address; option 3 the gateway; option 121 `169.254.169.254/32,<DHCP address>` and `0.0.0.0/0,<gateway>` |
| metadata | Lightstack listening on `169.254.169.254:80` inside; instance = (network `N`, source address) |

## Bounds and failure

Every command runs through the command runner under the unit's shared deadline (900 ms from the call's receipt, lock wait included; `node-apply.md`). Any failure fails the unit: the caller answers `500` and undoes (FR-008) or re-applies (FR-040).

## Reconciliation

On the network node: lists `qrouter-*`, `qdhcp-*`, `lg-*`, host proxy entries and `ls-l3-fwd`; converges each unit with a record; removes namespaces, legs, entries and listeners of units without one, after a complete read only. Compares before writing: a unit that matches is left untouched. On every other node: removes any `qrouter-*`/`qdhcp-*` it finds (a node that was the network node before), and the retired state of FR-044.

## Restarts (FR-036)

Namespaces live under the host's `/run/netns` (chart: `hostPath` `/run/netns`, `mountPropagation: Bidirectional`; Compose: `/run/netns:/run/netns:rshared`). A restarted process finds them and changes nothing that matches. Its first pass restarts each network's `dnsmasq`, which died with the old process's pod, and re-opens the metadata listeners; leases already granted run on meanwhile.
