# Contract: the floating-IP dataplane (real mode, per node)

> **Superseded in round 2** (2026-11-20): the floating-IP NAT moves into the router namespace
> (`router-namespace-dataplane.md`) and the filter is retired for the port firewall (`port-firewall.md`; FR-042). This
> file records round 1's design, which tasks T001–T052 implemented.

Not an API. This pins what a node holds for each floating IP and when, so the host run and the tests can check it.

## Content

### The filter, per fixed IP that a floating IP reaches

Chain `LS-NSI-<fixed IP, dots as dashes>` in table `filter`:

```text
-A LS-NSI-<ip> -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT
-A LS-NSI-<ip> [-s <cidr>/<len>] -p <proto> [-m <proto> --dport <n>[:<m>] | -m icmp --icmp-type <n>] -j ACCEPT
   … one per IPv4 ingress rule without a remote group, over every group of the port …
-A LS-NSI-<ip> -j DROP
```

The rules are rendered in iptables' canonical form, so `iptables -S LS-NSI-<ip>` prints them back unchanged. The jump
`-A FORWARD -i <uplink> -d <fixed IP> -j LS-NSI-<ip>` is present whenever the chain is.

### The NAT, per associated floating IP

```text
-A PREROUTING -d <floating IP>/32 -m comment --comment lightstack-fip -j DNAT --to-destination <fixed IP>
-A OUTPUT -d <floating IP>/32 -m comment --comment lightstack-fip -j DNAT --to-destination <fixed IP>
```

These are in table `nat`, together with the address `<floating IP>/32` on the uplink. All of it is present exactly while
a record associates the floating IP with that fixed IP: one rule per hook, never two targets.

- **The shape**: `-d <F>/32 [-m comment --comment lightstack-fip] -j DNAT --to-destination <X>` in `PREROUTING` or
  `OUTPUT`, with nothing else in the rule. A port forwarding's rule, with `-p` and `--dport`, is not of the shape.
- **Lightstack's rules**: the marked ones, and the unmarked ones of the shape for an address that has a record or a
  marked rule. Earlier releases wrote them unmarked.
- Only Lightstack's rules are ever removed. Any rule of the shape, Lightstack's or not, keeps the filter of the fixed IP
  it reaches.

## Order

Traffic to a floating IP is only ever forwarded through its port's filter:

| Change | Steps, in this order |
|---|---|
| Associate (create on a port, or update) | the filter; the NAT; the address |
| Move to another port | the new fixed IP's filter; the NAT re-pointed in one commit; the old fixed IP's filter removed, unless a record or any rule of the shape still reaches it |
| Disassociate, or delete | the address; the NAT; the filter, with the same exception |

## Replacement

- A filter is rebuilt in one `iptables-restore --noflush --wait 1` commit. At every moment the chain holds the complete
  old rules or the complete new ones, each ending in `DROP`.
- A floating IP's NAT changes in one commit to table `nat`. That commit deletes every rule for the address that points
  elsewhere, marked or of the earlier unmarked shape, and appends the marked rule where it is missing.
- A filter is removed in one commit: every `FORWARD` jump to it, then a flush, then the chain.
- A failed commit applies nothing; the old state stays whole. A chain, jump, rule or address that is already gone counts
  as removed.

## When it changes

| Trigger | Where | When the node cannot apply it |
|---|---|---|
| A membership change through either API | The node serving the request, before it answers | `500` and the change is undone (see the API contracts) |
| A rule added to or deleted from a group | The serving node, for every port carrying the group that has a floating IP | `500` and the change is undone |
| A floating IP created on a port, associated, moved, disassociated or deleted | The serving node | `500` and the change is undone |
| A port or a server deleted while a floating IP is associated with it | The serving node, after the transaction that deletes the port and disassociates the floating IP | Logged; the deletion stands, and reconciliation completes the removal within one pass |
| Reconciliation, at startup and every 30 s | Every node | Logged; the next pass tries again |

Every change holds the node's floating-IP lock for one address at a time, from reading the records to applying them. So
a reconciliation pass never applies rules it read before a change the node has since applied.

## Reconciliation

- A pass lists the node first: every rule of the shape, marked or not, every `LS-NSI-*` chain, and the uplink's
  addresses. Then it reads every floating IP's record, associated or not.
- It converges every floating IP that has a record or a marked rule, where the listing differs. An unassociated one is
  withdrawn, its unmarked rules included. It then removes each `LS-NSI-*` chain whose fixed IP no association names and
  no rule of the shape still reaches.
- A floating IP with only unmarked rules and no record is left alone, and so is the filter its rules reach.
- After an incomplete read of the records it removes nothing, and still ensures what it read.
- A filter or NAT that already matches its records is left untouched. A filter is compared with `iptables -S`; the NAT
  and the addresses are compared with the pass's listing, so a missing address is added back.

## Bounds

- Each command runs under a one-second deadline and is killed when it overruns: `iptables -S`, `iptables -t nat -S`,
  `iptables-restore`, the jump's check and append, and `ip -o addr show`, `ip addr add` and `ip addr del`.
- Acquiring the node's floating-IP lock has a one-second deadline.
- On the request path, missing either is a failed change: `500`, and the change is undone.
- In a reconciliation pass, missing the lock skips that address until the next pass, because the holder is changing it
  anyway.
- A real-mode node without an uplink: a request's change fails (`500`, undone); a reconciliation pass logs an error and
  skips its floating-IP step.

## Stub mode

Nothing is programmed, and every change succeeds at once. The records are stored and reported as in real mode.
