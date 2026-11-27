package neutron

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/common"
	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/rs/zerolog/log"
)

// The firewall of the ports bound to a node (SPEC-033 FR-041, FR-042, FR-047; research R27, R32). A
// port is bound to the node its guest runs on (ports.binding_host_id, set by Nova through BindPort),
// and that node holds its chains. The unit is the node's ports together: a node converges every port
// bound to it from the records, in one restore per family, so a port's chains never depend on what
// another request last wrote. A change reaches the nodes of the ports it touches, one call per node.

// portChange is what a change touched, from which convergePorts finds the ports whose firewall it
// changes.
type portChange struct {
	ports    []string // ports whose own chains changed
	carrying []string // groups whose rules changed: the ports carrying them
	members  []string // groups whose members changed: the ports whose rules expand them
	absent   []string // ports to read as gone, wherever they are bound: a deletion's first step
}

// convergePorts converges the firewall of every bound port a change touched on its node, one call per
// node in name order, and stops at the first failure, which the caller undoes (FR-008) or re-applies
// (FR-040).
func (svc *Service) convergePorts(ctx context.Context, change portChange) error {
	byNode, err := affectedPorts(ctx, svc.pool, change)
	if err != nil {
		return err
	}
	nodes := make([]string, 0, len(byNode))
	for node := range byNode {
		nodes = append(nodes, node)
	}
	slices.Sort(nodes)
	for _, node := range nodes {
		req := unitRequest{Unit: unitPorts, IDs: byNode[node], AbsentIDs: change.absent}
		if err := svc.caller.converge(ctx, node, req); err != nil {
			return fmt.Errorf("the ports of node %s: %w", node, err)
		}
	}
	return nil
}

// affectedPorts reads the bound ports a change touched, by node: present, the ports to converge from
// their records. A node holding only absent ports is there with none, so it removes their chains.
func affectedPorts(ctx context.Context, q subnetQuerier, change portChange) (map[string][]string, error) {
	named := append(slices.Clone(change.ports), change.absent...)
	rows, err := q.Query(ctx, `
		SELECT id::text, binding_host_id FROM ports
		 WHERE binding_host_id IS NOT NULL AND binding_host_id <> ''
		   AND (id::text = ANY($1)
		        OR id IN (SELECT port_id FROM port_security_groups WHERE security_group_id::text = ANY($2))
		        OR id IN (SELECT psg.port_id FROM port_security_groups psg
		                    JOIN security_group_rules r ON r.security_group_id = psg.security_group_id
		                   WHERE r.remote_group_id::text = ANY($3)))
		 ORDER BY id`, nonNil(named), nonNil(change.carrying), nonNil(change.members))
	if err != nil {
		return nil, fmt.Errorf("read the ports a change touched: %w", err)
	}
	defer rows.Close()
	byNode := map[string][]string{}
	for rows.Next() {
		var id, node string
		if err := rows.Scan(&id, &node); err != nil {
			return nil, fmt.Errorf("read the ports a change touched: %w", err)
		}
		if _, seen := byNode[node]; !seen {
			byNode[node] = []string{}
		}
		if !slices.Contains(change.absent, id) {
			byNode[node] = append(byNode[node], id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the ports a change touched: %w", err)
	}
	return byNode, nil
}

// applyPorts converges the firewall of the ports bound to this node: all of them, from the records,
// with absent read as gone — also from the remote-group members the others' rules expand. Their
// networks' bridges are made first, where a guest's TAP is to join them.
func (svc *Service) applyPorts(ctx context.Context, absent []string) error {
	if err := svc.assignMissingZones(ctx); err != nil {
		return err
	}
	ports, err := readPortFirewall(ctx, svc.pool, svc.nodes.Name, absent)
	if err != nil {
		return err
	}
	made := map[string]bool{}
	for _, p := range ports {
		if p.Bridge == "" || made[p.Bridge] {
			continue
		}
		made[p.Bridge] = true
		if err := svc.brManager.CreateBridge(p.Bridge); err != nil {
			return fmt.Errorf("bridge %s: %w", p.Bridge, err)
		}
	}
	return svc.firewall.ConvergePorts(ctx, ports)
}

// assignMissingZones gives each network a port bound to this node is on that has no connection-tracking
// zone, one created before migration 084, the lowest free zone (R51), under the zone lock. The update
// takes the network's row lock; a zone another request assigned meanwhile stands.
func (svc *Service) assignMissingZones(ctx context.Context) error {
	networks, err := queryStrings(ctx, svc.pool, `
		SELECT DISTINCT n.id::text FROM networks n JOIN ports p ON p.network_id = n.id
		 WHERE p.binding_host_id = $1 AND n.ct_zone IS NULL`, svc.nodes.Name)
	if err != nil {
		return fmt.Errorf("read the networks without a zone: %w", err)
	}
	for _, n := range networks {
		if err := database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
			if err := database.AdvisoryLock(ctx, tx, ctZoneLock); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, "UPDATE networks SET ct_zone = "+nextZone+" WHERE id::text = $1 AND ct_zone IS NULL", n)
			return err
		}); err != nil {
			return fmt.Errorf("network %s: assign its zone: %w", n, err)
		}
	}
	return nil
}

// boundPort is a port bound to a node, as its firewall needs it.
type boundPort struct {
	id, mac, network string
	fixed            common.FixedIPs
	zone             int
}

// readPortFirewall reads the firewall of the ports bound to node (R32): each port's addresses, MAC,
// network and zone, its groups' statefulness and rules, a remote group's rule expanded to one rule per
// member address, the port's own left out. Ports in absent are read as gone. A router's or a DHCP
// port (network:*) has no chains: its leg is trusted by name.
func readPortFirewall(ctx context.Context, q subnetQuerier, node string, absent []string) ([]networking.FirewallPort, error) {
	bound, err := readBoundPorts(ctx, q, node, absent)
	if err != nil || len(bound) == 0 {
		return nil, err
	}
	ids := make([]string, 0, len(bound))
	for _, p := range bound {
		ids = append(ids, p.id)
	}
	groups, stateless, err := portGroups(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	rules, members, err := readGroupRules(ctx, q, groups, absent)
	if err != nil {
		return nil, err
	}
	out := make([]networking.FirewallPort, 0, len(bound))
	for _, p := range bound {
		fp := networking.FirewallPort{ID: p.id, MAC: p.mac, Zone: p.zone, Stateless: stateless[p.id]}
		for _, f := range p.fixed {
			fp.Addresses = append(fp.Addresses, f.IPAddress)
		}
		if p.network != "" {
			fp.Bridge = networking.BridgeNameForNetwork(p.network)
		}
		for _, g := range groups[p.id] {
			for _, r := range rules[g] {
				fp.Rules = append(fp.Rules, r.expand(members[r.remoteGroup], fp.Addresses)...)
			}
		}
		out = append(out, fp)
	}
	return out, nil
}

// readBoundPorts reads the ports bound to node that have chains: neither trusted (network:*) nor in absent.
func readBoundPorts(ctx context.Context, q subnetQuerier, node string, absent []string) ([]boundPort, error) {
	rows, err := q.Query(ctx, `
		SELECT p.id::text, COALESCE(p.mac_address, ''), p.fixed_ips, COALESCE(p.device_owner, ''),
		       COALESCE(p.network_id::text, ''), COALESCE(n.ct_zone, 0)
		  FROM ports p LEFT JOIN networks n ON n.id = p.network_id
		 WHERE p.binding_host_id = $1
		 ORDER BY p.id`, node)
	if err != nil {
		return nil, fmt.Errorf("read the ports bound to %s: %w", node, err)
	}
	defer rows.Close()
	var bound []boundPort
	for rows.Next() {
		var p boundPort
		var owner string
		if err := rows.Scan(&p.id, &p.mac, &p.fixed, &owner, &p.network, &p.zone); err != nil {
			return nil, fmt.Errorf("read the ports bound to %s: %w", node, err)
		}
		if !slices.Contains(absent, p.id) && !trustedOwner(owner) {
			bound = append(bound, p)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the ports bound to %s: %w", node, err)
	}
	return bound, nil
}

// readGroupRules reads the rules of the ports' groups, by group, and the member addresses of the remote
// groups they name, absent ports left out.
func readGroupRules(ctx context.Context, q subnetQuerier, groups map[string][]string, absent []string) (map[string][]groupRule, map[string][]string, error) {
	var groupIDs []string
	for _, gs := range groups {
		groupIDs = append(groupIDs, gs...)
	}
	rules, err := groupRules(ctx, q, groupIDs)
	if err != nil {
		return nil, nil, err
	}
	var remotes []string
	for _, rs := range rules {
		for _, r := range rs {
			if r.remoteGroup != "" {
				remotes = append(remotes, r.remoteGroup)
			}
		}
	}
	members, err := groupMembers(ctx, q, remotes, absent)
	return rules, members, err
}

// portGroups reads the groups of ports, and which ports' groups are stateless (FR-051: a port's
// groups are all of one kind).
func portGroups(ctx context.Context, q subnetQuerier, ports []string) (map[string][]string, map[string]bool, error) {
	rows, err := q.Query(ctx, `
		SELECT psg.port_id::text, sg.id::text, sg.stateful
		  FROM port_security_groups psg JOIN security_groups sg ON sg.id = psg.security_group_id
		 WHERE psg.port_id::text = ANY($1)
		 ORDER BY sg.id`, ports)
	if err != nil {
		return nil, nil, fmt.Errorf("read the ports' groups: %w", err)
	}
	defer rows.Close()
	groups, stateless := map[string][]string{}, map[string]bool{}
	for rows.Next() {
		var port, group string
		var stateful bool
		if err := rows.Scan(&port, &group, &stateful); err != nil {
			return nil, nil, fmt.Errorf("read the ports' groups: %w", err)
		}
		groups[port] = append(groups[port], group)
		if !stateful {
			stateless[port] = true
		}
	}
	return groups, stateless, rows.Err()
}

// groupRule is a security group rule as stored.
type groupRule struct {
	direction, ethertype, protocol, remotePrefix, remoteGroup string
	portMin, portMax                                          *int
}

// expand is the rule as the firewall holds it: itself, or, naming a remote group, one rule per member
// address of its family, except own, as upstream's without ipset. A remote group without members
// allows nothing.
func (r groupRule) expand(members, own []string) []networking.FirewallRule {
	rule := networking.FirewallRule{Direction: r.direction, Ethertype: r.ethertype, Protocol: r.protocol,
		PortMin: r.portMin, PortMax: r.portMax, Remote: r.remotePrefix}
	if r.remoteGroup == "" {
		return []networking.FirewallRule{rule}
	}
	var out []networking.FirewallRule
	for _, m := range members {
		a, err := netip.ParseAddr(m)
		if err != nil || slices.Contains(own, m) || a.Unmap().Is4() != (r.ethertype != "IPv6") {
			continue
		}
		rule.Remote = a.Unmap().String()
		out = append(out, rule)
	}
	return out
}

// groupRules reads the rules of groups, by group.
func groupRules(ctx context.Context, q subnetQuerier, groups []string) (map[string][]groupRule, error) {
	out := map[string][]groupRule{}
	if len(groups) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `
		SELECT security_group_id::text, direction, COALESCE(ethertype, 'IPv4'), COALESCE(protocol, ''),
		       port_range_min, port_range_max, COALESCE(remote_ip_prefix, ''), COALESCE(remote_group_id::text, '')
		  FROM security_group_rules WHERE security_group_id::text = ANY($1)
		 ORDER BY id`, groups)
	if err != nil {
		return nil, fmt.Errorf("read the groups' rules: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var group string
		var r groupRule
		var lo, hi sql.NullInt32
		if err := rows.Scan(&group, &r.direction, &r.ethertype, &r.protocol, &lo, &hi, &r.remotePrefix, &r.remoteGroup); err != nil {
			return nil, fmt.Errorf("read the groups' rules: %w", err)
		}
		if lo.Valid {
			v := int(lo.Int32)
			r.portMin = &v
		}
		if hi.Valid {
			v := int(hi.Int32)
			r.portMax = &v
		}
		out[group] = append(out[group], r)
	}
	return out, rows.Err()
}

// groupMembers reads the addresses of the ports carrying groups, by group; ports in absent are read
// as gone.
func groupMembers(ctx context.Context, q subnetQuerier, groups, absent []string) (map[string][]string, error) {
	out := map[string][]string{}
	if len(groups) == 0 {
		return out, nil
	}
	rows, err := q.Query(ctx, `
		SELECT psg.security_group_id::text, p.id::text, p.fixed_ips
		  FROM port_security_groups psg JOIN ports p ON p.id = psg.port_id
		 WHERE psg.security_group_id::text = ANY($1)
		 ORDER BY p.id`, groups)
	if err != nil {
		return nil, fmt.Errorf("read the groups' members: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var group, port string
		var fixed common.FixedIPs
		if err := rows.Scan(&group, &port, &fixed); err != nil {
			return nil, fmt.Errorf("read the groups' members: %w", err)
		}
		if slices.Contains(absent, port) {
			continue
		}
		for _, f := range fixed {
			if f.IPAddress != "" {
				out[group] = append(out[group], f.IPAddress)
			}
		}
	}
	return out, rows.Err()
}

// BindPort binds a port to node, the node its guest runs on, or unbinds it when node is "", and
// converges its firewall before it returns: on node from the records, and as gone on the node it was
// bound to before (SPEC-033 FR-041; research R27, R32). A binding that cannot be converged is put
// back, compare-and-set, the nodes are converged again, and the error is returned.
func (svc *Service) BindPort(ctx context.Context, portID, node string) error {
	var before *string
	err := database.WithTx(ctx, svc.pool, func(tx database.Tx) error {
		if err := tx.QueryRow(ctx, "SELECT binding_host_id FROM ports WHERE id::text = $1 FOR UPDATE", portID).Scan(&before); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "UPDATE ports SET binding_host_id = $1, updated_at = $2 WHERE id::text = $3", nullIfEmpty(node), time.Now(), portID)
		return err
	})
	if errors.Is(err, database.ErrNoRows) {
		return fmt.Errorf("port %s: %w", portID, err)
	}
	if err != nil {
		return fmt.Errorf("bind port %s: %w", portID, err)
	}
	old := derefOr(before, "")
	if err := svc.convergeBinding(ctx, portID, old, node); err != nil {
		ctx := context.WithoutCancel(ctx)
		if _, uerr := svc.pool.Exec(ctx,
			"UPDATE ports SET binding_host_id = $1, updated_at = $2 WHERE id::text = $3 AND COALESCE(binding_host_id, '') = $4",
			nullIfEmpty(old), time.Now(), portID, node); uerr != nil {
			log.Error().Err(uerr).Str("port_id", portID).Msg("put a port's binding back; the reconciliation pass converges its nodes")
		}
		if rerr := svc.convergeBinding(ctx, portID, node, old); rerr != nil {
			log.Error().Err(rerr).Str("port_id", portID).Msg("converge a port's nodes after its binding went back; the reconciliation pass converges them")
		}
		return fmt.Errorf("bind port %s to %q: %w", portID, node, err)
	}
	return nil
}

// RebindPort binds a port back to node after a server's deletion or detach that unbound it did not stand,
// compare-and-set: only while the port still names the server and is still unbound, so a port another request
// has detached or bound since stays as that request left it (SPEC-033 FR-040, T252). The record is the
// binding: when node cannot be converged the binding stays, the reconciliation pass converging node from it,
// and the error is returned (T261).
func (svc *Service) RebindPort(ctx context.Context, portID, instanceID, node string) error {
	if node == "" {
		return nil
	}
	tag, err := svc.pool.Exec(ctx, `
		UPDATE ports SET binding_host_id = $1, updated_at = $2
		 WHERE id::text = $3 AND device_id::text = $4 AND binding_host_id IS NULL`,
		node, time.Now(), portID, instanceID)
	if err != nil {
		return fmt.Errorf("bind port %s back to %s: %w", portID, node, err)
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	return svc.convergeBinding(ctx, portID, "", node)
}

// convergeBinding converges a port's firewall after its binding moved from one node to another, "" for
// none: on the new node first, so a guest starting there finds its chains, then on the old one, whose
// records no longer bind the port. The port is not absent: it stays a member of its groups.
func (svc *Service) convergeBinding(ctx context.Context, portID, from, to string) error {
	if to != "" {
		if err := svc.caller.converge(ctx, to, unitRequest{Unit: unitPorts, IDs: []string{portID}}); err != nil {
			return fmt.Errorf("node %s: %w", to, err)
		}
	}
	if from != "" && from != to {
		if err := svc.caller.converge(ctx, from, unitRequest{Unit: unitPorts}); err != nil {
			return fmt.Errorf("node %s: %w", from, err)
		}
	}
	return nil
}

// portWithdrawal is what deleting ports touches, read before their records go: the routers of their
// floating IPs and port forwardings, the networks whose DHCP serves them, and the groups they are
// members of.
type portWithdrawal struct {
	ports   []string
	routers map[string][]string // router → the ports' floating IPs and port forwardings through it
	dhcp    map[string][]string // network → the ports its DHCP serves
	groups  []string
}

func readPortWithdrawal(ctx context.Context, q subnetQuerier, ports []string) (portWithdrawal, error) {
	w := portWithdrawal{ports: ports, routers: map[string][]string{}, dhcp: map[string][]string{}}
	// A port forwarding goes with its internal port (ON DELETE CASCADE), as upstream deletes a port's
	// forwardings with it, so its router drops it first, as a floating IP of the port (branch review
	// T184).
	for _, query := range []string{`
		SELECT id::text, router_id::text FROM floating_ips
		 WHERE port_id::text = ANY($1) AND router_id IS NOT NULL ORDER BY id`, `
		SELECT pf.id::text, f.router_id::text FROM port_forwardings pf JOIN floating_ips f ON f.id = pf.floatingip_id
		 WHERE pf.internal_port_id::text = ANY($1) AND f.router_id IS NOT NULL ORDER BY pf.id`,
	} {
		rows, err := q.Query(ctx, query, ports)
		if err != nil {
			return w, fmt.Errorf("read the ports' floating IPs and port forwardings: %w", err)
		}
		for rows.Next() {
			var id, router string
			if err := rows.Scan(&id, &router); err != nil {
				rows.Close()
				return w, fmt.Errorf("read the ports' floating IPs and port forwardings: %w", err)
			}
			w.routers[router] = append(w.routers[router], id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return w, fmt.Errorf("read the ports' floating IPs and port forwardings: %w", err)
		}
	}
	rows, err := q.Query(ctx, "SELECT id::text, COALESCE(network_id::text, ''), fixed_ips FROM ports WHERE id::text = ANY($1) ORDER BY id", ports)
	if err != nil {
		return w, fmt.Errorf("read the ports: %w", err)
	}
	type port struct {
		id, network string
		fixed       common.FixedIPs
	}
	var read []port
	for rows.Next() {
		var p port
		if err := rows.Scan(&p.id, &p.network, &p.fixed); err != nil {
			rows.Close()
			return w, fmt.Errorf("read the ports: %w", err)
		}
		read = append(read, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return w, fmt.Errorf("read the ports: %w", err)
	}
	for _, p := range read {
		if p.network != "" && servesDHCP(ctx, q, p.fixed) {
			w.dhcp[p.network] = append(w.dhcp[p.network], p.id)
		}
	}
	if w.groups, err = queryStrings(ctx, q,
		"SELECT DISTINCT security_group_id::text FROM port_security_groups WHERE port_id::text = ANY($1)", ports); err != nil {
		return w, fmt.Errorf("read the ports' groups: %w", err)
	}
	return w, nil
}

// WithdrawPorts converges, before ports' records go (SPEC-033 FR-040, R34), every unit they touch as if
// they were gone: the routers of their floating IPs, their networks' DHCP, the firewall of their nodes
// and of every port whose rules expand their groups. When one cannot be, the units are converged from
// the records again and the error is returned. Otherwise it returns how to converge them from the
// records again, should the deletion not stand.
func (svc *Service) WithdrawPorts(ctx context.Context, ports []string) (restore func(), err error) {
	w, err := readPortWithdrawal(ctx, svc.pool, ports)
	if err != nil {
		return nil, err
	}
	restore = func() { svc.restorePorts(context.WithoutCancel(ctx), w) }
	if err := svc.withdrawPorts(ctx, w); err != nil {
		restore()
		return nil, err
	}
	return restore, nil
}

func (svc *Service) withdrawPorts(ctx context.Context, w portWithdrawal) error {
	for _, router := range sortedKeysOf(w.routers) {
		if err := svc.convergeRouterUnit(ctx, router, w.routers[router]...); err != nil {
			return err
		}
	}
	for _, network := range sortedKeysOf(w.dhcp) {
		if err := svc.convergeDHCPUnit(ctx, network, w.dhcp[network]...); err != nil {
			return err
		}
	}
	return svc.convergePorts(ctx, portChange{absent: w.ports, members: w.groups})
}

// restorePorts converges what a withdrawal touched from the records again. What it cannot apply, the
// reconciliation pass converges.
func (svc *Service) restorePorts(ctx context.Context, w portWithdrawal) {
	for _, router := range sortedKeysOf(w.routers) {
		svc.reapplyRouter(ctx, router)
	}
	for _, network := range sortedKeysOf(w.dhcp) {
		svc.reapplyDHCP(ctx, network)
	}
	if err := svc.convergePorts(ctx, portChange{ports: w.ports, members: w.groups}); err != nil {
		log.Error().Err(err).Strs("ports", w.ports).Msg("converge the ports' nodes from the records again; the reconciliation pass converges them")
	}
}

func sortedKeysOf(m map[string][]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// applyPortCreate applies a new port before the answer (SPEC-033 FR-008): its DHCP reservation on the
// network node when its addresses are DHCP's, and the firewall of every bound port whose rules expand
// one of its groups, which it joins with its addresses (R29, R32). A new port is bound to no node yet:
// Nova binds it when its guest boots.
func (svc *Service) applyPortCreate(ctx context.Context, networkID string, dhcp bool, groups []string) error {
	if dhcp {
		if err := svc.convergeDHCPUnit(ctx, networkID); err != nil {
			return err
		}
	}
	return svc.convergePorts(ctx, portChange{members: groups})
}

// reapplyPortCreate converges what a port create touched from the records again, after its undo. What
// it cannot apply, the reconciliation pass converges.
func (svc *Service) reapplyPortCreate(ctx context.Context, networkID string, dhcp bool, groups []string) {
	if dhcp {
		svc.reapplyDHCP(ctx, networkID)
	}
	if err := svc.convergePorts(context.WithoutCancel(ctx), portChange{members: groups}); err != nil {
		log.Error().Err(err).Strs("groups", groups).Msg("converge the ports of a port's groups again; the reconciliation pass converges them")
	}
}

// nullIfEmpty stores "" as NULL.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
