package neutron

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/pkg/networking"
)

// The units' fakes (SPEC-033 research R41): in memory, holding what a node would hold, recording every
// call and able to fail any of them.

// fakeHolder is a network node's units of one kind, routers or networks' DHCP: per unit, the state
// last converged. Its hook runs before a call takes effect; an error from it fails the call and
// changes nothing.
type fakeHolder[S any] struct {
	mu    sync.Mutex
	held  map[string]S
	calls []holderCall[S]
	hook  func(holderCall[S]) error
	clone func(S) S
}

// holderCall is one convergence of the unit ID: to State, or to absent when State is nil.
type holderCall[S any] struct {
	ID    string
	State *S
}

func newFakeHolder[S any](clone func(S) S) fakeHolder[S] {
	return fakeHolder[S]{held: map[string]S{}, clone: clone}
}

func (f *fakeHolder[S]) converge(id string, state *S) error {
	c := holderCall[S]{ID: id}
	if state != nil {
		s := f.clone(*state)
		c.State = &s
	}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	hook := f.hook
	f.mu.Unlock()
	if hook != nil {
		if err := hook(c); err != nil {
			return err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if c.State == nil {
		delete(f.held, id)
	} else {
		f.held[id] = *c.State
	}
	return nil
}

// failFirst fails the next n calls with err.
func (f *fakeHolder[S]) failFirst(n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hook = func(holderCall[S]) error {
		if n > 0 {
			n--
			return err
		}
		return nil
	}
}

// state is the unit as the node holds it; ok is false when it holds none.
func (f *fakeHolder[S]) state(id string) (S, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.held[id]
	if !ok {
		return s, false
	}
	return f.clone(s), true
}

// heldIDs lists the units the node holds, as a listing of its namespaces would.
func (f *fakeHolder[S]) heldIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]string, 0, len(f.held))
	for id := range f.held {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// plant makes the node hold a unit no call converged: one an earlier process left, say.
func (f *fakeHolder[S]) plant(id string, state S) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held[id] = f.clone(state)
}

// drop makes the node lose a unit without a call: a namespace deleted out of band, say.
func (f *fakeHolder[S]) drop(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.held, id)
}

func (f *fakeHolder[S]) recorded() []holderCall[S] {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// fakeRouters is a network node's routers, and the proxy entries on its uplink that no router's leg
// routes: the routers' own are their host addresses.
type fakeRouters struct {
	fakeHolder[networking.RouterState]
	strays map[string]bool
}

type routerCall = holderCall[networking.RouterState]

func newFakeRouters() *fakeRouters {
	return &fakeRouters{fakeHolder: newFakeHolder(cloneRouterState), strays: map[string]bool{}}
}

func (f *fakeRouters) ConvergeRouter(_ context.Context, routerID string, state *networking.RouterState, _ string) error {
	return f.converge(routerID, state)
}

func (f *fakeRouters) HeldRouters(context.Context) ([]string, error) { return f.heldIDs(), nil }

// RemoveStrayProxies removes the stray entries ours recognises; a router's own host addresses are
// routed by its leg and never stray.
func (f *fakeRouters) RemoveStrayProxies(_ context.Context, _ string, ours func(string) bool) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	routed := map[string]bool{}
	for _, s := range f.held {
		for _, a := range networking.PlanRouter(s).HostAddresses {
			routed[a] = true
		}
	}
	var removed []string
	for a := range f.strays {
		if !routed[a] && ours(a) {
			delete(f.strays, a)
			removed = append(removed, a)
		}
	}
	slices.Sort(removed)
	return removed, nil
}

// proxy plants a proxy entry on the uplink that no leg routes.
func (f *fakeRouters) proxy(addresses ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range addresses {
		f.strays[a] = true
	}
}

func (f *fakeRouters) proxied(address string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.strays[address]
}

// fakeDHCP is a network node's DHCP, per network.
type fakeDHCP struct {
	fakeHolder[networking.DHCPState]
}

func newFakeDHCP() *fakeDHCP {
	return &fakeDHCP{newFakeHolder(cloneDHCPState)}
}

func (f *fakeDHCP) ConvergeDHCP(_ context.Context, networkID string, state *networking.DHCPState) error {
	return f.converge(networkID, state)
}

func (f *fakeDHCP) HeldNetworks(context.Context) ([]string, error) { return f.heldIDs(), nil }

func cloneDHCPState(s networking.DHCPState) networking.DHCPState {
	out := s
	out.Subnets = slices.Clone(s.Subnets)
	for i := range out.Subnets {
		out.Subnets[i].DNS = slices.Clone(s.Subnets[i].DNS)
	}
	out.Hosts = slices.Clone(s.Hosts)
	return out
}

// fakeListeners are a network node's metadata listeners: the networks they serve. Its hook runs
// before a call takes effect, named "open <network>" or "close <network>"; an error from it fails the
// call and changes nothing.
type fakeListeners struct {
	mu    sync.Mutex
	open  map[string]bool
	calls []string
	hook  func(call string) error
}

func newFakeListeners() *fakeListeners { return &fakeListeners{open: map[string]bool{}} }

func (f *fakeListeners) Open(_ context.Context, networkID string) error {
	return f.call("open "+networkID, func() { f.open[networkID] = true })
}

func (f *fakeListeners) Close(networkID string) error {
	return f.call("close "+networkID, func() { delete(f.open, networkID) })
}

func (f *fakeListeners) call(name string, effect func()) error {
	f.mu.Lock()
	f.calls = append(f.calls, name)
	hook := f.hook
	f.mu.Unlock()
	if hook != nil {
		if err := hook(name); err != nil {
			return err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	effect()
	return nil
}

func (f *fakeListeners) Serving() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for id := range f.open {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// plant opens a listener no call opened: one an earlier pass left, say.
func (f *fakeListeners) plant(networkID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.open[networkID] = true
}

func (f *fakeListeners) serving(networkID string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.open[networkID]
}

func (f *fakeListeners) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func cloneRouterState(s networking.RouterState) networking.RouterState {
	out := s
	if s.Gateway != nil {
		g := *s.Gateway
		g.Addresses = slices.Clone(g.Addresses)
		out.Gateway = &g
	}
	out.Interfaces = slices.Clone(s.Interfaces)
	out.FloatingIPs = slices.Clone(s.FloatingIPs)
	out.PortForwardings = slices.Clone(s.PortForwardings)
	return out
}

// fakeNodeCaller carries a node call to the in-process service of that node, as the real caller
// carries it over HTTP, so two-node tests run in one process. It records every call.
type fakeNodeCaller struct {
	mu       sync.Mutex
	services map[string]*Service
	calls    []nodeCallRecord
}

type nodeCallRecord struct {
	Node string
	Req  unitRequest
}

func newFakeNodeCaller(services ...*Service) *fakeNodeCaller {
	f := &fakeNodeCaller{services: map[string]*Service{}}
	for _, s := range services {
		f.services[s.nodes.Name] = s
	}
	return f
}

func (f *fakeNodeCaller) converge(ctx context.Context, node string, req unitRequest) error {
	f.mu.Lock()
	f.calls = append(f.calls, nodeCallRecord{Node: node, Req: req})
	target, ok := f.services[node]
	f.mu.Unlock()
	if !ok {
		return fmt.Errorf("node %s is not an active node", node)
	}
	ctx, cancel := context.WithTimeout(ctx, nodeApplyDeadline)
	defer cancel()
	return target.convergeUnit(ctx, req)
}

func (f *fakeNodeCaller) recorded() []nodeCallRecord {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// fakeFirewall is a node's port firewall: the ports it holds, as last converged. Its hook runs before
// a call takes effect, given the ports the call converges to; an error from it fails the call and
// changes nothing. removedAll counts the calls that removed the whole firewall, teardown's.
type fakeFirewall struct {
	mu         sync.Mutex
	held       map[string]networking.FirewallPort
	calls      int
	removedAll int
	hook       func(ports []networking.FirewallPort) error
}

func (f *fakeFirewall) RemoveAll(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held = map[string]networking.FirewallPort{}
	f.removedAll++
	return nil
}

// plant makes the node hold a port's chains no call converged.
func (f *fakeFirewall) plant(p networking.FirewallPort) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held[p.ID] = p
}

func (f *fakeFirewall) heldPorts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for id := range f.held {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func newFakeFirewall() *fakeFirewall {
	return &fakeFirewall{held: map[string]networking.FirewallPort{}}
}

func (f *fakeFirewall) ConvergePorts(_ context.Context, ports []networking.FirewallPort) error {
	f.mu.Lock()
	f.calls++
	hook := f.hook
	f.mu.Unlock()
	if hook != nil {
		if err := hook(ports); err != nil {
			return err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held = map[string]networking.FirewallPort{}
	for _, p := range ports {
		p.Rules = slices.Clone(p.Rules)
		p.Addresses = slices.Clone(p.Addresses)
		f.held[p.ID] = p
	}
	return nil
}

// failFirst fails the next n calls with err.
func (f *fakeFirewall) failFirst(n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hook = func([]networking.FirewallPort) error {
		if n > 0 {
			n--
			return err
		}
		return nil
	}
}

// port is a port as the node's firewall holds it; ok is false when it holds none.
func (f *fakeFirewall) port(id string) (networking.FirewallPort, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.held[id]
	return p, ok
}

func (f *fakeFirewall) converged() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeBridges is a node's bridges: those made. Its hook runs before a create takes effect, given the
// bridge's name; an error from it fails the create.
type fakeBridges struct {
	mu   sync.Mutex
	made map[string]bool
	hook func(name string) error
}

func newFakeBridges() *fakeBridges { return &fakeBridges{made: map[string]bool{}} }

func (f *fakeBridges) CreateBridge(name string) error {
	f.mu.Lock()
	hook := f.hook
	f.mu.Unlock()
	if hook != nil {
		if err := hook(name); err != nil {
			return err
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.made[name] = true
	return nil
}

func (f *fakeBridges) DeleteBridge(name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.made, name)
	return nil
}

func (f *fakeBridges) ListManagedBridges() ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.made))
	for name := range f.made {
		out = append(out, name)
	}
	slices.Sort(out)
	return out, nil
}

// fakeFDB records the VXLAN forwarding rows written, each in the transaction it was given, and fails
// with err when it is set.
type fakeFDB struct {
	mu    sync.Mutex
	ports []string
	inTx  []bool
	err   error
}

func (f *fakeFDB) RecordFDB(_ context.Context, tx database.Tx, _, portID, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ports = append(f.ports, portID)
	f.inTx = append(f.inTx, tx != nil)
	return f.err
}

// fakeRetired records each removal of the retired state of earlier layouts: the uplink and the records
// it was given.
type fakeRetired struct {
	mu    sync.Mutex
	calls []retiredCall
	err   error // what the next removal answers
}

type retiredCall struct {
	uplink  string
	records networking.RetiredRecords
}

func (f *fakeRetired) Remove(_ context.Context, uplink string, records networking.RetiredRecords) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, retiredCall{uplink: uplink, records: records})
	err := f.err
	f.err = nil
	return err
}

// failNext makes the next removal answer err.
func (f *fakeRetired) failNext(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

func (f *fakeRetired) recorded() []retiredCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// fakeProjectNamespaces records the project namespaces of older deployments deleted.
type fakeProjectNamespaces struct {
	mu      sync.Mutex
	deleted []string
}

func (f *fakeProjectNamespaces) DeleteNamespace(_ context.Context, projectID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, projectID)
	return nil
}
