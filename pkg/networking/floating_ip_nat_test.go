package networking

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// SPEC-033 T035 (research R23; contracts/floating-ip-dataplane.md): a floating IP's NAT is marked,
// changed in one nat-table commit, and its address is added last and removed first. A filter is
// removed in one commit. The node listing returns every rule of the shape, marked or not, every
// LS-NSI chain and the uplink's addresses. Every command runs under a one-second deadline.

const (
	fipF = "203.0.113.5"
	fipX = "10.0.0.5"
	fipY = "10.0.0.9"
)

func marked(chain, f, x string) string {
	return "-A " + chain + " -d " + f + "/32 -m comment --comment lightstack-fip -j DNAT --to-destination " + x
}

func unmarked(chain, f, x string) string {
	return "-A " + chain + " -d " + f + "/32 -j DNAT --to-destination " + x
}

func TestParseFloatingNAT(t *testing.T) {
	lines := []string{
		"-P PREROUTING ACCEPT",
		"-N DOCKER",
		marked("PREROUTING", fipF, fipX),
		unmarked("PREROUTING", "203.0.113.6", fipY),
		// A port forwarding of the same address is not of the shape.
		"-A PREROUTING -d " + fipF + "/32 -p tcp -m tcp --dport 80 -j DNAT --to-destination 10.0.0.5:8080",
		// Nor is any other DNAT.
		"-A PREROUTING -d 198.51.100.0/24 -j DNAT --to-destination 10.0.0.7",
		"-A PREROUTING -i eth0 -d 203.0.113.8/32 -j DNAT --to-destination 10.0.0.8",
		"-A PREROUTING -j DOCKER",
	}
	got := parseFloatingNAT("PREROUTING", lines)
	want := []FloatingNAT{
		{Chain: "PREROUTING", FloatingIP: fipF, FixedIP: fipX, Marked: true},
		{Chain: "PREROUTING", FloatingIP: "203.0.113.6", FixedIP: fipY, Marked: false},
	}
	if !slices.Equal(got, want) {
		t.Errorf("parseFloatingNAT\n got  %+v\n want %+v", got, want)
	}
}

func TestNATPayloads(t *testing.T) {
	both := func(f func(chain string) FloatingNAT) []FloatingNAT {
		return []FloatingNAT{f("PREROUTING"), f("OUTPUT")}
	}
	toX := both(func(c string) FloatingNAT { return FloatingNAT{c, fipF, fipX, true} })
	toYMarked := both(func(c string) FloatingNAT { return FloatingNAT{c, fipF, fipY, true} })
	toXUnmarked := both(func(c string) FloatingNAT { return FloatingNAT{c, fipF, fipX, false} })
	addLines := []string{
		"-A PREROUTING -d " + fipF + "/32 -m comment --comment lightstack-fip -j DNAT --to-destination " + fipX,
		"-A OUTPUT -d " + fipF + "/32 -m comment --comment lightstack-fip -j DNAT --to-destination " + fipX,
	}

	for _, tc := range []struct {
		name     string
		current  []FloatingNAT
		want     []string // the payload's rule lines, in order; none means no payload
		replaced []string
	}{
		{"none: an -A per hook", nil, addLines, nil},
		{"marked to Y: deleted and added", toYMarked, []string{
			"-D PREROUTING -d " + fipF + "/32 -m comment --comment lightstack-fip -j DNAT --to-destination " + fipY,
			addLines[0],
			"-D OUTPUT -d " + fipF + "/32 -m comment --comment lightstack-fip -j DNAT --to-destination " + fipY,
			addLines[1],
		}, []string{fipY}},
		{"unmarked to X: replaced by the marked rule", toXUnmarked, []string{
			"-D PREROUTING -d " + fipF + "/32 -j DNAT --to-destination " + fipX,
			addLines[0],
			"-D OUTPUT -d " + fipF + "/32 -j DNAT --to-destination " + fipX,
			addLines[1],
		}, nil},
		{"marked to X already: no payload", toX, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload, replaced := natAssociatePayload(tc.current, fipF, fipX)
			if len(tc.want) == 0 {
				if payload != "" {
					t.Errorf("payload %q; want none", payload)
				}
			} else if want := "*nat\n" + strings.Join(tc.want, "\n") + "\nCOMMIT\n"; payload != want {
				t.Errorf("payload\n got  %q\n want %q", payload, want)
			}
			if !slices.Equal(replaced, tc.replaced) {
				t.Errorf("replaced %v; want %v", replaced, tc.replaced)
			}
		})
	}

	t.Run("a withdrawal deletes every rule of the address, marked or not", func(t *testing.T) {
		current := []FloatingNAT{
			{"PREROUTING", fipF, fipX, true}, {"PREROUTING", fipF, fipY, false}, {"OUTPUT", fipF, fipX, true},
		}
		payload, replaced := natWithdrawPayload(current)
		want := "*nat\n" +
			"-D PREROUTING -d " + fipF + "/32 -m comment --comment lightstack-fip -j DNAT --to-destination " + fipX + "\n" +
			"-D PREROUTING -d " + fipF + "/32 -j DNAT --to-destination " + fipY + "\n" +
			"-D OUTPUT -d " + fipF + "/32 -m comment --comment lightstack-fip -j DNAT --to-destination " + fipX + "\n" +
			"COMMIT\n"
		if payload != want {
			t.Errorf("payload\n got  %q\n want %q", payload, want)
		}
		if !slices.Equal(replaced, []string{fipX, fipY}) {
			t.Errorf("replaced %v; want [%s %s]", replaced, fipX, fipY)
		}
		if p, r := natWithdrawPayload(nil); p != "" || r != nil {
			t.Errorf("nothing to withdraw: %q, %v; want no payload", p, r)
		}
	})

	t.Run("a filter's removal: its jumps, then a flush, then the chain", func(t *testing.T) {
		chain := nsIngressChain(fipX)
		jumps := []string{"-A FORWARD -d " + fipX + "/32 -i eth0 -j " + chain, "-A FORWARD -d " + fipX + "/32 -i eth1 -j " + chain}
		want := "*filter\n" +
			"-D FORWARD -d " + fipX + "/32 -i eth0 -j " + chain + "\n" +
			"-D FORWARD -d " + fipX + "/32 -i eth1 -j " + chain + "\n" +
			"-F " + chain + "\n-X " + chain + "\nCOMMIT\n"
		if got := filterRemovalPayload(chain, jumps); got != want {
			t.Errorf("payload\n got  %q\n want %q", got, want)
		}
	})
}

// natNode is a node for the fake runner: its two nat hooks, its filter chains and the uplink's
// addresses, answered as iptables and ip print them.
type natNode struct {
	prerouting, output []string
	chains             map[string][]string // chain → its -A lines
	forward            []string
	addrs              []string         // CIDRs on the uplink
	fail               map[string]error // command prefix → error
}

func (n *natNode) respond(_ context.Context, c fakeCall) ([]byte, error) {
	cmd := c.name + " " + strings.Join(c.args, " ")
	for prefix, err := range n.fail {
		if strings.HasPrefix(cmd, prefix) {
			return []byte("failed"), err
		}
	}
	lines := func(ls []string) ([]byte, error) { return []byte(strings.Join(ls, "\n") + "\n"), nil }
	switch {
	case cmd == "iptables -t nat -S PREROUTING":
		return lines(append([]string{"-P PREROUTING ACCEPT"}, n.prerouting...))
	case cmd == "iptables -t nat -S OUTPUT":
		return lines(append([]string{"-P OUTPUT ACCEPT"}, n.output...))
	case cmd == "iptables -t filter -S FORWARD":
		return lines(append([]string{"-P FORWARD DROP"}, n.forward...))
	case cmd == "iptables -t filter -S":
		out := []string{"-P INPUT ACCEPT", "-P FORWARD DROP", "-P OUTPUT ACCEPT"}
		for chain := range n.chains {
			out = append(out, "-N "+chain)
		}
		return lines(append(out, n.forward...))
	case strings.HasPrefix(cmd, "iptables -t filter -S LS-NSI-"):
		chain := c.args[len(c.args)-1]
		rules, ok := n.chains[chain]
		if !ok {
			return []byte("iptables: No chain/target/match by that name.\n"), errors.New("exit status 1")
		}
		return lines(append([]string{"-N " + chain}, rules...))
	case strings.HasPrefix(cmd, "ip -o addr show dev "):
		var out []string
		for _, a := range n.addrs {
			out = append(out, "2: eth0    inet "+a+" scope global eth0\\       valid_lft forever preferred_lft forever")
		}
		return lines(out)
	}
	return nil, nil // a write: iptables-restore, ip addr add or del
}

func realNAT(n *natNode) (*RouterManager, *fakeRunner) {
	f := &fakeRunner{respond: n.respond}
	rm := NewRouterManager("iptables")
	rm.run = f.run
	rm.enableForwarding = func() error { return nil }
	return rm, f
}

// writes returns the commands that change the node.
func writes(f *fakeRunner) []string {
	var out []string
	for _, c := range f.commands() {
		if strings.HasPrefix(c, "iptables-restore") || strings.HasPrefix(c, "ip addr add") || strings.HasPrefix(c, "ip addr del") {
			out = append(out, c)
		}
	}
	return out
}

func countPrefix(cmds []string, prefix string) int {
	n := 0
	for _, c := range cmds {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func TestEnsureFloatingIP_RealMode(t *testing.T) {
	t.Run("nothing there: one restore, then the address", func(t *testing.T) {
		rm, f := realNAT(&natNode{})
		replaced, err := rm.EnsureFloatingIP(fipF, fipX, "eth0")
		if err != nil {
			t.Fatalf("EnsureFloatingIP: %v", err)
		}
		cmds := f.commands()
		if countPrefix(cmds, "iptables -t nat -S PREROUTING") != 1 || countPrefix(cmds, "iptables -t nat -S OUTPUT") != 1 {
			t.Errorf("each hook must be read once: %q", cmds)
		}
		if got, want := writes(f), []string{"iptables-restore --noflush --wait 1", "ip addr add " + fipF + "/32 dev eth0"}; !slices.Equal(got, want) {
			t.Errorf("writes %q; want %q, the address after the NAT", got, want)
		}
		if len(replaced) != 0 {
			t.Errorf("replaced %v; want none", replaced)
		}
	})

	t.Run("in place: no write", func(t *testing.T) {
		rm, f := realNAT(&natNode{
			prerouting: []string{marked("PREROUTING", fipF, fipX)},
			output:     []string{marked("OUTPUT", fipF, fipX)},
			addrs:      []string{"192.0.2.10/24", fipF + "/32"},
		})
		if _, err := rm.EnsureFloatingIP(fipF, fipX, "eth0"); err != nil {
			t.Fatalf("EnsureFloatingIP: %v", err)
		}
		if got := writes(f); len(got) != 0 {
			t.Errorf("writes %q; want none", got)
		}
	})

	t.Run("a move reports the old target", func(t *testing.T) {
		rm, f := realNAT(&natNode{
			prerouting: []string{marked("PREROUTING", fipF, fipY)},
			output:     []string{marked("OUTPUT", fipF, fipY)},
			addrs:      []string{fipF + "/32"},
		})
		replaced, err := rm.EnsureFloatingIP(fipF, fipX, "eth0")
		if err != nil {
			t.Fatalf("EnsureFloatingIP: %v", err)
		}
		if !slices.Equal(replaced, []string{fipY}) {
			t.Errorf("replaced %v; want [%s]", replaced, fipY)
		}
		if got := writes(f); !slices.Equal(got, []string{"iptables-restore --noflush --wait 1"}) {
			t.Errorf("writes %q; want one restore and no address change", got)
		}
	})
}

func TestWithdrawFloatingIP_RealMode(t *testing.T) {
	t.Run("the address first, then the NAT", func(t *testing.T) {
		rm, f := realNAT(&natNode{
			prerouting: []string{marked("PREROUTING", fipF, fipX), unmarked("PREROUTING", fipF, fipY)},
			output:     []string{marked("OUTPUT", fipF, fipX)},
			addrs:      []string{fipF + "/32"},
		})
		replaced, err := rm.WithdrawFloatingIP(fipF, "eth0")
		if err != nil {
			t.Fatalf("WithdrawFloatingIP: %v", err)
		}
		if got, want := writes(f), []string{"ip addr del " + fipF + "/32 dev eth0", "iptables-restore --noflush --wait 1"}; !slices.Equal(got, want) {
			t.Errorf("writes %q; want %q", got, want)
		}
		if !slices.Equal(replaced, []string{fipX, fipY}) {
			t.Errorf("replaced %v; want [%s %s]", replaced, fipX, fipY)
		}
	})

	t.Run("nothing there: no write, no error, nothing reported", func(t *testing.T) {
		rm, f := realNAT(&natNode{addrs: []string{"192.0.2.10/24"}})
		replaced, err := rm.WithdrawFloatingIP(fipF, "eth0")
		if err != nil || len(replaced) != 0 {
			t.Fatalf("WithdrawFloatingIP: %v, %v; want nothing", replaced, err)
		}
		if got := writes(f); len(got) != 0 {
			t.Errorf("writes %q; want none", got)
		}
	})
}

func TestRemoveIngressFilter_RealMode(t *testing.T) {
	chain := nsIngressChain(fipX)
	t.Run("one restore", func(t *testing.T) {
		rm, f := realNAT(&natNode{
			chains:  map[string][]string{chain: {"-A " + chain + " -j DROP"}},
			forward: []string{"-A FORWARD -d " + fipX + "/32 -i eth0 -j " + chain},
		})
		if err := rm.RemoveIngressFilter(fipX); err != nil {
			t.Fatalf("RemoveIngressFilter: %v", err)
		}
		if got := writes(f); !slices.Equal(got, []string{"iptables-restore --noflush --wait 1"}) {
			t.Fatalf("writes %q; want one restore", got)
		}
		restore := f.calls[len(f.calls)-1]
		want := filterRemovalPayload(chain, []string{"-A FORWARD -d " + fipX + "/32 -i eth0 -j " + chain})
		if restore.stdin != want {
			t.Errorf("restore stdin\n got  %q\n want %q", restore.stdin, want)
		}
	})

	t.Run("a missing chain: no write and no error", func(t *testing.T) {
		rm, f := realNAT(&natNode{})
		if err := rm.RemoveIngressFilter(fipX); err != nil {
			t.Fatalf("RemoveIngressFilter: %v", err)
		}
		if got := writes(f); len(got) != 0 {
			t.Errorf("writes %q; want none", got)
		}
	})
}

func TestNorthSouthState_RealMode(t *testing.T) {
	rm, _ := realNAT(&natNode{
		prerouting: []string{marked("PREROUTING", fipF, fipX), unmarked("PREROUTING", "203.0.113.6", fipY)},
		output:     []string{marked("OUTPUT", fipF, fipX)},
		chains:     map[string][]string{nsIngressChain(fipX): nil, "DOCKER": nil},
		addrs:      []string{"192.0.2.10/24", fipF + "/32"},
	})
	got, err := rm.NorthSouthState("eth0")
	if err != nil {
		t.Fatalf("NorthSouthState: %v", err)
	}
	wantNAT := []FloatingNAT{
		{"PREROUTING", fipF, fipX, true}, {"PREROUTING", "203.0.113.6", fipY, false}, {"OUTPUT", fipF, fipX, true},
	}
	if !slices.Equal(got.NAT, wantNAT) {
		t.Errorf("NAT %+v; want %+v", got.NAT, wantNAT)
	}
	if !slices.Equal(got.Filters, []string{fipX}) {
		t.Errorf("filters %v; want [%s]", got.Filters, fipX)
	}
	if !slices.Equal(got.Addresses, []string{"192.0.2.10", fipF}) {
		t.Errorf("addresses %v; want [192.0.2.10 %s]", got.Addresses, fipF)
	}
}

func TestFloatingIPDataplane_ErrorsAndDeadlines(t *testing.T) {
	boom := errors.New("exit status 4")
	calls := map[string]func(rm *RouterManager) error{
		"EnsureFloatingIP": func(rm *RouterManager) error { _, err := rm.EnsureFloatingIP(fipF, fipX, "eth0"); return err },
		"WithdrawFloatingIP": func(rm *RouterManager) error {
			_, err := rm.WithdrawFloatingIP(fipF, "eth0")
			return err
		},
		"RemoveIngressFilter": func(rm *RouterManager) error { return rm.RemoveIngressFilter(fipX) },
		"NorthSouthState":     func(rm *RouterManager) error { _, err := rm.NorthSouthState("eth0"); return err },
	}
	chain := nsIngressChain(fipX)
	present := func() *natNode {
		return &natNode{
			prerouting: []string{marked("PREROUTING", fipF, fipY)},
			output:     []string{marked("OUTPUT", fipF, fipY)},
			chains:     map[string][]string{chain: nil},
			forward:    []string{"-A FORWARD -d " + fipX + "/32 -i eth0 -j " + chain},
			addrs:      []string{fipF + "/32"},
		}
	}
	for name, call := range calls {
		t.Run(name+": every error is returned", func(t *testing.T) {
			for _, prefix := range []string{"iptables -t nat -S", "iptables-restore", "ip ", "iptables -t filter -S"} {
				n := present()
				n.fail = map[string]error{prefix: boom}
				rm, f := realNAT(n)
				err := call(rm)
				used := false
				for _, c := range f.commands() {
					if strings.HasPrefix(c, prefix) {
						used = true
					}
				}
				if used && !errors.Is(err, boom) {
					t.Errorf("failing %q: got %v, want the command's error", prefix, err)
				}
			}
		})
		t.Run(name+": every command has a deadline of one second at most", func(t *testing.T) {
			rm, f := realNAT(present())
			_ = call(rm)
			for _, c := range f.calls {
				if !c.hasDeadline || c.budget > time.Second {
					t.Errorf("%s %v: deadline=%v budget=%v", c.name, c.args, c.hasDeadline, c.budget)
				}
			}
		})
		t.Run(name+": a blocking command fails within about one second", func(t *testing.T) {
			rm, f := realNAT(present())
			f.respond = func(ctx context.Context, _ fakeCall) ([]byte, error) {
				<-ctx.Done()
				return nil, ctx.Err()
			}
			start := time.Now()
			err := call(rm)
			if err == nil {
				t.Fatal("got nil; want an error")
			}
			if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
				t.Errorf("took %v; want about one second", elapsed)
			}
		})
	}
}

func TestFloatingIPDataplane_StubMode(t *testing.T) {
	f := &fakeRunner{}
	rm := NewRouterManager("stub")
	rm.run = f.run
	if r, err := rm.EnsureFloatingIP(fipF, fipX, ""); err != nil || r != nil {
		t.Errorf("EnsureFloatingIP: %v, %v", r, err)
	}
	if r, err := rm.WithdrawFloatingIP(fipF, ""); err != nil || r != nil {
		t.Errorf("WithdrawFloatingIP: %v, %v", r, err)
	}
	if err := rm.RemoveIngressFilter(fipX); err != nil {
		t.Errorf("RemoveIngressFilter: %v", err)
	}
	state, err := rm.NorthSouthState("")
	if err != nil || len(state.NAT) != 0 || len(state.Filters) != 0 || len(state.Addresses) != 0 {
		t.Errorf("NorthSouthState: %+v, %v; want an empty listing", state, err)
	}
	if len(f.calls) != 0 {
		t.Errorf("stub mode ran %q", f.commands())
	}
}
