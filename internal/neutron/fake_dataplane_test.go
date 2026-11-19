package neutron

import (
	"slices"
	"sort"
	"sync"

	"github.com/cobaltcore-dev/lightstack/pkg/networking"
)

// fakeDataplane is the node's floating-IP dataplane in memory (SPEC-033 T009, T036; research R9). It
// keeps, per fixed IP, the rules last applied; per floating IP, its NAT rules of the shape, marked or
// not; and the uplink's addresses. It records every call in order, when the call starts, and can make
// any call block or fail through its hook.
type fakeDataplane struct {
	mu      sync.Mutex
	filters map[string][]networking.IngressRule
	nat     map[string][]fakeNAT // floating IP → its rules; one entry stands for both hooks
	addrs   map[string]bool      // the uplink's addresses
	calls   []dataplaneCall
	hook    func(dataplaneCall) error // runs before a call takes effect, outside the mutex
}

type fakeNAT struct {
	fixed  string
	marked bool
}

type dataplaneCall struct {
	Method     string
	FixedIP    string
	FloatingIP string
	Uplink     string
	Rules      []networking.IngressRule
	// Changed is false for a filter rebuild that found the filter already holding its rules: the
	// real EnsureIngressFilter writes nothing then (research R7).
	Changed bool
}

func newFakeDataplane() *fakeDataplane {
	return &fakeDataplane{
		filters: map[string][]networking.IngressRule{},
		nat:     map[string][]fakeNAT{},
		addrs:   map[string]bool{},
	}
}

func (f *fakeDataplane) EnsureIngressFilter(fixedIP, uplink string, rules []networking.IngressRule) error {
	c := dataplaneCall{Method: "EnsureIngressFilter", FixedIP: fixedIP, Uplink: uplink, Rules: slices.Clone(rules)}
	idx, err := f.enterAt(c)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	current, ok := f.filters[fixedIP]
	f.calls[idx].Changed = !ok || !sameRules(current, c.Rules)
	f.filters[fixedIP] = c.Rules
	return nil
}

func (f *fakeDataplane) EnsureFloatingIP(floatingIP, fixedIP, uplink string) ([]string, error) {
	if err := f.enter(dataplaneCall{Method: "EnsureFloatingIP", FloatingIP: floatingIP, FixedIP: fixedIP, Uplink: uplink}); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var replaced []string
	for _, r := range f.nat[floatingIP] {
		if r.fixed != fixedIP && !slices.Contains(replaced, r.fixed) {
			replaced = append(replaced, r.fixed)
		}
	}
	f.nat[floatingIP] = []fakeNAT{{fixed: fixedIP, marked: true}}
	f.addrs[floatingIP] = true // the address last, as the real one
	return replaced, nil
}

func (f *fakeDataplane) WithdrawFloatingIP(floatingIP, uplink string) ([]string, error) {
	if err := f.enter(dataplaneCall{Method: "WithdrawFloatingIP", FloatingIP: floatingIP, Uplink: uplink}); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.addrs, floatingIP) // the address first, as the real one
	var replaced []string
	for _, r := range f.nat[floatingIP] {
		if !slices.Contains(replaced, r.fixed) {
			replaced = append(replaced, r.fixed)
		}
	}
	delete(f.nat, floatingIP)
	return replaced, nil
}

func (f *fakeDataplane) RemoveIngressFilter(fixedIP string) error {
	if err := f.enter(dataplaneCall{Method: "RemoveIngressFilter", FixedIP: fixedIP}); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.filters, fixedIP)
	return nil
}

func (f *fakeDataplane) NorthSouthState(uplink string) (networking.NorthSouth, error) {
	if err := f.enter(dataplaneCall{Method: "NorthSouthState", Uplink: uplink}); err != nil {
		return networking.NorthSouth{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var state networking.NorthSouth
	floating := make([]string, 0, len(f.nat))
	for fip := range f.nat {
		floating = append(floating, fip)
	}
	sort.Strings(floating)
	for _, fip := range floating {
		for _, r := range f.nat[fip] {
			for _, hook := range []string{"PREROUTING", "OUTPUT"} {
				state.NAT = append(state.NAT, networking.FloatingNAT{Chain: hook, FloatingIP: fip, FixedIP: r.fixed, Marked: r.marked})
			}
		}
	}
	for fixed := range f.filters {
		state.Filters = append(state.Filters, fixed)
	}
	sort.Strings(state.Filters)
	for a := range f.addrs {
		state.Addresses = append(state.Addresses, a)
	}
	sort.Strings(state.Addresses)
	return state, nil
}

// enter records the call and runs the hook.
func (f *fakeDataplane) enter(c dataplaneCall) error {
	_, err := f.enterAt(c)
	return err
}

// enterAt is enter, reporting where the call was recorded.
func (f *fakeDataplane) enterAt(c dataplaneCall) (int, error) {
	f.mu.Lock()
	idx := len(f.calls)
	f.calls = append(f.calls, c)
	hook := f.hook
	f.mu.Unlock()
	if hook == nil {
		return idx, nil
	}
	return idx, hook(c)
}

func (f *fakeDataplane) setHook(h func(dataplaneCall) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hook = h
}

// failWith makes every later call fail with err.
func (f *fakeDataplane) failWith(err error) {
	f.setHook(func(dataplaneCall) error { return err })
}

// failMethod makes every later call of one method fail with err.
func (f *fakeDataplane) failMethod(method string, err error) {
	f.setHook(func(c dataplaneCall) error {
		if c.Method == method {
			return err
		}
		return nil
	})
}

// blockFirst makes the next filter rebuild for fixedIP wait until release is called. entered is
// closed once that call has started. Other calls pass straight through. release may be called more
// than once.
func (f *fakeDataplane) blockFirst(fixedIP string) (entered <-chan struct{}, release func()) {
	in, gate := make(chan struct{}), make(chan struct{})
	var first, released sync.Once
	f.setHook(func(c dataplaneCall) error {
		if c.Method != "EnsureIngressFilter" || c.FixedIP != fixedIP {
			return nil
		}
		blocked := false
		first.Do(func() { blocked = true })
		if blocked {
			close(in)
			<-gate
		}
		return nil
	})
	return in, func() { released.Do(func() { close(gate) }) }
}

// seedNAT puts a rule of the shape on the fake node, as an earlier release or an operator would have.
func (f *fakeDataplane) seedNAT(floatingIP, fixedIP string, marked bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nat[floatingIP] = append(f.nat[floatingIP], fakeNAT{fixed: fixedIP, marked: marked})
}

func (f *fakeDataplane) seedFilter(fixedIP string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.filters[fixedIP] = nil
}

func (f *fakeDataplane) seedAddress(floatingIP string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addrs[floatingIP] = true
}

// allCalls returns every call so far, in order.
func (f *fakeDataplane) allCalls() []dataplaneCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// writes returns the calls that change the node, in order, as "Method subject".
func (f *fakeDataplane) writes() []string {
	var out []string
	for _, c := range f.allCalls() {
		switch c.Method {
		case "EnsureIngressFilter":
			if c.Changed {
				out = append(out, c.Method+" "+c.FixedIP)
			}
		case "RemoveIngressFilter":
			out = append(out, c.Method+" "+c.FixedIP)
		case "EnsureFloatingIP":
			out = append(out, c.Method+" "+c.FloatingIP+"->"+c.FixedIP)
		case "WithdrawFloatingIP":
			out = append(out, c.Method+" "+c.FloatingIP)
		}
	}
	return out
}

// callsFor returns the filter rebuilds for one fixed IP, in order.
func (f *fakeDataplane) callsFor(fixedIP string) []dataplaneCall {
	var out []dataplaneCall
	for _, c := range f.allCalls() {
		if c.Method == "EnsureIngressFilter" && c.FixedIP == fixedIP {
			out = append(out, c)
		}
	}
	return out
}

// filterOf returns the rules last applied for a fixed IP, and whether there is a filter at all.
func (f *fakeDataplane) filterOf(fixedIP string) ([]networking.IngressRule, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rules, ok := f.filters[fixedIP]
	return slices.Clone(rules), ok
}

// natOf returns a floating IP's rules on the fake node.
func (f *fakeDataplane) natOf(floatingIP string) []fakeNAT {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.nat[floatingIP])
}

func (f *fakeDataplane) hasAddress(floatingIP string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.addrs[floatingIP]
}
