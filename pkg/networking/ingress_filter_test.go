package networking

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// SPEC-033 T008: a floating IP's ingress filter is rendered in iptables' canonical form, replaced in
// one iptables-restore commit, and left alone when unchanged (research R6, R7;
// contracts/floating-ip-dataplane.md). Every command runs under a one-second deadline (FR-007b).

const testChain = "LS-NSI-10-0-0-5"

func TestRenderIngressFilter(t *testing.T) {
	const (
		head = "-A " + testChain + " -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT"
		drop = "-A " + testChain + " -j DROP"
	)
	cases := []struct {
		name  string
		rules []IngressRule
		want  []string // the lines between the conntrack line and DROP
	}{
		{"no rules", nil, nil},
		{"tcp single port", []IngressRule{{Protocol: "tcp", PortMin: 22}},
			[]string{"-p tcp -m tcp --dport 22 -j ACCEPT"}},
		{"tcp port range", []IngressRule{{Protocol: "tcp", PortMin: 80, PortMax: 90}},
			[]string{"-p tcp -m tcp --dport 80:90 -j ACCEPT"}},
		{"min == max is one port", []IngressRule{{Protocol: "tcp", PortMin: 443, PortMax: 443}},
			[]string{"-p tcp -m tcp --dport 443 -j ACCEPT"}},
		{"udp", []IngressRule{{Protocol: "udp", PortMin: 53}},
			[]string{"-p udp -m udp --dport 53 -j ACCEPT"}},
		{"icmp type", []IngressRule{{Protocol: "icmp", PortMin: 8}},
			[]string{"-p icmp -m icmp --icmp-type 8 -j ACCEPT"}},
		{"protocol only", []IngressRule{{Protocol: "tcp"}},
			[]string{"-p tcp -j ACCEPT"}},
		{"source CIDR before -p", []IngressRule{{Protocol: "tcp", PortMin: 22, Remote: "10.0.0.0/8"}},
			[]string{"-s 10.0.0.0/8 -p tcp -m tcp --dport 22 -j ACCEPT"}},
		{"source CIDR as its network", []IngressRule{{Protocol: "tcp", PortMin: 22, Remote: "10.1.2.3/8"}},
			[]string{"-s 10.0.0.0/8 -p tcp -m tcp --dport 22 -j ACCEPT"}},
		{"host source as /32", []IngressRule{{Protocol: "tcp", PortMin: 22, Remote: "192.0.2.7"}},
			[]string{"-s 192.0.2.7/32 -p tcp -m tcp --dport 22 -j ACCEPT"}},
		{"any source drops -s", []IngressRule{{Protocol: "tcp", PortMin: 22, Remote: "0.0.0.0/0"}},
			[]string{"-p tcp -m tcp --dport 22 -j ACCEPT"}},
		{"an empty rule accepts everything", []IngressRule{{}},
			[]string{"-j ACCEPT"}},
		{"protocol in upper case", []IngressRule{{Protocol: "TCP", PortMin: 22}},
			[]string{"-p tcp -m tcp --dport 22 -j ACCEPT"}},
		{"protocol by number", []IngressRule{{Protocol: "6", PortMin: 22}, {Protocol: "17", PortMin: 53}, {Protocol: "1", PortMin: 8}},
			[]string{"-p icmp -m icmp --icmp-type 8 -j ACCEPT", "-p tcp -m tcp --dport 22 -j ACCEPT", "-p udp -m udp --dport 53 -j ACCEPT"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want := []string{head}
			for _, l := range c.want {
				want = append(want, "-A "+testChain+" "+l)
			}
			want = append(want, drop)
			if got := RenderIngressFilter(testChain, c.rules); !slices.Equal(got, want) {
				t.Errorf("RenderIngressFilter(%+v)\n got  %q\n want %q", c.rules, got, want)
			}
		})
	}

	// The comparison with the kernel must not depend on the order the database returns the rules in,
	// and a rule two groups share is one allowance.
	t.Run("order-independent, and a shared rule once", func(t *testing.T) {
		a := []IngressRule{{Protocol: "tcp", PortMin: 22}, {Protocol: "udp", PortMin: 53}, {Protocol: "tcp", PortMin: 22}}
		b := []IngressRule{{Protocol: "udp", PortMin: 53}, {Protocol: "tcp", PortMin: 22}}
		if ga, gb := RenderIngressFilter(testChain, a), RenderIngressFilter(testChain, b); !slices.Equal(ga, gb) || len(ga) != 4 {
			t.Errorf("renderings differ or keep the duplicate:\n %q\n %q", ga, gb)
		}
	})
}

func TestRestorePayload(t *testing.T) {
	lines := RenderIngressFilter(testChain, []IngressRule{{Protocol: "tcp", PortMin: 22}})
	want := "*filter\n" +
		":" + testChain + " - [0:0]\n" +
		"-F " + testChain + "\n" +
		"-A " + testChain + " -m conntrack --ctstate RELATED,ESTABLISHED -j ACCEPT\n" +
		"-A " + testChain + " -p tcp -m tcp --dport 22 -j ACCEPT\n" +
		"-A " + testChain + " -j DROP\n" +
		"COMMIT\n"
	if got := RestorePayload(testChain, lines); got != want {
		t.Errorf("RestorePayload\n got  %q\n want %q", got, want)
	}
}

func TestChainMatches(t *testing.T) {
	desired := RenderIngressFilter(testChain, []IngressRule{{Protocol: "tcp", PortMin: 22}, {Protocol: "udp", PortMin: 53}})
	listed := append([]string{"-N " + testChain}, desired...)

	without := func(i int) []string { return slices.Delete(slices.Clone(listed), i, i+1) }
	reordered := slices.Clone(listed)
	reordered[2], reordered[3] = reordered[3], reordered[2]

	cases := []struct {
		name    string
		current []string
		want    bool
	}{
		{"equal, the -N line ignored", listed, true},
		{"equal without the -N line", desired, true},
		{"an extra rule", append(slices.Clone(listed[:len(listed)-1]), "-A "+testChain+" -p tcp -m tcp --dport 80 -j ACCEPT", listed[len(listed)-1]), false},
		{"a missing rule", without(2), false},
		{"reordered rules", reordered, false},
		{"a missing DROP", without(len(listed) - 1), false},
		{"empty output, no chain", nil, false},
	}
	for _, c := range cases {
		if got := ChainMatches(c.current, desired); got != c.want {
			t.Errorf("%s: ChainMatches = %v, want %v\n current %q", c.name, got, c.want, c.current)
		}
	}
}

// fakeRunner records the commands a RouterManager runs and answers them through respond.
type fakeRunner struct {
	mu      sync.Mutex
	calls   []fakeCall
	respond func(ctx context.Context, c fakeCall) ([]byte, error)
}

type fakeCall struct {
	name        string
	args        []string
	stdin       string
	hasDeadline bool
	budget      time.Duration // time left until the deadline when the command started
}

func (f *fakeRunner) run(ctx context.Context, name string, args []string, stdin string) ([]byte, error) {
	c := fakeCall{name: name, args: slices.Clone(args), stdin: stdin}
	if d, ok := ctx.Deadline(); ok {
		c.hasDeadline, c.budget = true, time.Until(d)
	}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	f.mu.Unlock()
	if f.respond == nil {
		return nil, nil
	}
	return f.respond(ctx, c)
}

func (f *fakeRunner) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.name+" "+strings.Join(c.args, " "))
	}
	return out
}

func realModeRouter(f *fakeRunner) *RouterManager {
	rm := NewRouterManager("iptables")
	rm.run = f.run
	return rm
}

const (
	testFixedIP = "10.0.0.5"
	testUplink  = "eth0"
)

var (
	testRules   = []IngressRule{{Protocol: "tcp", PortMin: 22}}
	listCmd     = "iptables -t filter -S " + testChain
	restoreCmd  = "iptables-restore --noflush --wait 1"
	jumpRule    = "FORWARD -i " + testUplink + " -d " + testFixedIP + " -j " + testChain
	checkJump   = "iptables -t filter -C " + jumpRule
	appendJump  = "iptables -t filter -A " + jumpRule
	errNoChain  = errors.New("exit status 1")
	errRejected = errors.New("iptables-restore: line 4 failed")
)

// listing answers `iptables -S` with the chain as the kernel would print it.
func listing(lines []string) []byte {
	return []byte(strings.Join(append([]string{"-N " + testChain}, lines...), "\n") + "\n")
}

func TestEnsureIngressFilter_RealMode(t *testing.T) {
	desired := RenderIngressFilter(testChain, testRules)

	t.Run("unchanged: no restore, only the jump checked", func(t *testing.T) {
		f := &fakeRunner{respond: func(_ context.Context, c fakeCall) ([]byte, error) {
			if c.name+" "+strings.Join(c.args, " ") == listCmd {
				return listing(desired), nil
			}
			return nil, nil // the jump is there
		}}
		if err := realModeRouter(f).EnsureIngressFilter(testFixedIP, testUplink, testRules); err != nil {
			t.Fatalf("EnsureIngressFilter: %v", err)
		}
		if got, want := f.commands(), []string{listCmd, checkJump}; !slices.Equal(got, want) {
			t.Errorf("commands\n got  %q\n want %q", got, want)
		}
	})

	for _, tc := range []struct {
		name string
		list func() ([]byte, error)
	}{
		{"changed", func() ([]byte, error) {
			return listing(RenderIngressFilter(testChain, []IngressRule{{Protocol: "tcp", PortMin: 80}})), nil
		}},
		{"missing", func() ([]byte, error) {
			return []byte("iptables: No chain/target/match by that name.\n"), errNoChain
		}},
	} {
		t.Run(tc.name+": one restore with the payload, then the jump", func(t *testing.T) {
			f := &fakeRunner{respond: func(_ context.Context, c fakeCall) ([]byte, error) {
				switch c.name + " " + strings.Join(c.args, " ") {
				case listCmd:
					return tc.list()
				case checkJump:
					return []byte("iptables: Bad rule (does a matching rule exist in that chain?).\n"), errNoChain
				}
				return nil, nil
			}}
			if err := realModeRouter(f).EnsureIngressFilter(testFixedIP, testUplink, testRules); err != nil {
				t.Fatalf("EnsureIngressFilter: %v", err)
			}
			if got, want := f.commands(), []string{listCmd, restoreCmd, checkJump, appendJump}; !slices.Equal(got, want) {
				t.Fatalf("commands\n got  %q\n want %q", got, want)
			}
			if got, want := f.calls[1].stdin, RestorePayload(testChain, desired); got != want {
				t.Errorf("restore stdin\n got  %q\n want %q", got, want)
			}
		})
	}

	t.Run("a failed restore is returned, and nothing follows it", func(t *testing.T) {
		f := &fakeRunner{respond: func(_ context.Context, c fakeCall) ([]byte, error) {
			switch c.name + " " + strings.Join(c.args, " ") {
			case listCmd:
				return nil, errNoChain
			case restoreCmd:
				return []byte("iptables-restore: line 4 failed\n"), errRejected
			}
			return nil, nil
		}}
		err := realModeRouter(f).EnsureIngressFilter(testFixedIP, testUplink, testRules)
		if !errors.Is(err, errRejected) {
			t.Fatalf("EnsureIngressFilter: got %v, want the restore's error", err)
		}
		if got, want := f.commands(), []string{listCmd, restoreCmd}; !slices.Equal(got, want) {
			t.Errorf("commands\n got  %q\n want %q", got, want)
		}
	})

	t.Run("every command carries a deadline of one second at most", func(t *testing.T) {
		f := &fakeRunner{respond: func(_ context.Context, c fakeCall) ([]byte, error) {
			if c.name == "iptables" && slices.Contains(c.args, "-S") {
				return nil, errNoChain
			}
			if c.name == "iptables" && slices.Contains(c.args, "-C") {
				return nil, errNoChain
			}
			return nil, nil
		}}
		if err := realModeRouter(f).EnsureIngressFilter(testFixedIP, testUplink, testRules); err != nil {
			t.Fatalf("EnsureIngressFilter: %v", err)
		}
		if len(f.calls) != 4 {
			t.Fatalf("commands: %q; want the list, the restore, the check and the append", f.commands())
		}
		for _, c := range f.calls {
			if !c.hasDeadline || c.budget > time.Second {
				t.Errorf("%s %v: deadline=%v, budget=%v; want a deadline of one second at most", c.name, c.args, c.hasDeadline, c.budget)
			}
		}
	})

	t.Run("a command that blocks fails the call within about one second", func(t *testing.T) {
		f := &fakeRunner{respond: func(ctx context.Context, _ fakeCall) ([]byte, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		start := time.Now()
		err := realModeRouter(f).EnsureIngressFilter(testFixedIP, testUplink, testRules)
		elapsed := time.Since(start)
		if err == nil {
			t.Fatal("EnsureIngressFilter with a blocking runner: got nil, want an error")
		}
		if elapsed > 1500*time.Millisecond {
			t.Errorf("EnsureIngressFilter took %v; want about one second", elapsed)
		}
	})
}

func TestEnsureIngressFilter_StubModeRunsNothing(t *testing.T) {
	f := &fakeRunner{}
	rm := NewRouterManager("stub")
	rm.run = f.run
	if err := rm.EnsureIngressFilter(testFixedIP, testUplink, testRules); err != nil {
		t.Fatalf("EnsureIngressFilter in stub mode: %v", err)
	}
	if len(f.calls) != 0 {
		t.Errorf("stub mode ran %q; want nothing", f.commands())
	}
}
