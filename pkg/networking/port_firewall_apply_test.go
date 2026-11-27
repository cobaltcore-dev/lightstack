package networking

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// SPEC-033 round 2, T126 (FR-041, FR-047; research R32; contracts/port-firewall.md, Apply): a node
// reads its filter and raw tables once per family, compares chain by chain, and writes only what
// differs, in one restore per family, every command within a second. Stub mode runs nothing.

// fwNode answers the reads as a node holding, per family, the firewall of held — nil for a node
// holding none — with Lightstack's jumps from the built-in chains; or, when saved is set, with the tables
// iptables-save printed, by "<family> <table>" (testdata/iptables-save).
type fwNode struct {
	held  []FirewallPort
	saved map[string]string
}

func (n fwNode) respond(_ context.Context, c fakeCall) ([]byte, error) {
	family := ""
	switch c.name {
	case "iptables-save":
		family = "IPv4"
	case "ip6tables-save":
		family = "IPv6"
	default:
		return nil, nil
	}
	table := c.args[len(c.args)-1]
	if n.saved != nil {
		return []byte(n.saved[family+" "+table]), nil
	}
	return []byte(savedText(n.held, family, table)), nil
}

// savedText is a table as iptables-save prints it on a node holding ports' firewall, or nothing of it
// when ports is nil.
func savedText(ports []FirewallPort, family, table string) string {
	var b strings.Builder
	b.WriteString("*" + table + "\n")
	builtins := map[string][]string{"filter": {"INPUT", "FORWARD", "OUTPUT"}, "raw": {"PREROUTING", "OUTPUT"}}[table]
	for _, chain := range builtins {
		b.WriteString(":" + chain + " ACCEPT [0:0]\n")
	}
	if ports == nil {
		b.WriteString("COMMIT\n")
		return b.String()
	}
	rules := portFirewallRules(ports, nil, family)[table]
	var names []string
	for name := range rules {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		b.WriteString(":" + name + " - [0:0]\n")
	}
	if table == "filter" {
		b.WriteString("-A INPUT -j ls-filter-top\n-A FORWARD -j ls-filter-top\n")
	} else {
		b.WriteString("-A PREROUTING -j ls-ct\n")
	}
	for _, name := range names {
		for _, r := range rules[name] {
			b.WriteString(r + "\n")
		}
	}
	b.WriteString("COMMIT\n")
	return b.String()
}

func realModeFirewall(f *fakeRunner) *FirewallManager {
	m := NewFirewallManager("iptables")
	m.run = f.run
	return m
}

func restores(f *fakeRunner) []fakeCall {
	var out []fakeCall
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasSuffix(c.name, "-restore") {
			out = append(out, c)
		}
	}
	return out
}

func TestConvergePorts_AFreshNode(t *testing.T) {
	f := &fakeRunner{respond: fwNode{}.respond}
	if err := realModeFirewall(f).ConvergePorts(context.Background(), fwPorts()); err != nil {
		t.Fatalf("converge: %v", err)
	}
	want := []string{
		"ip -j link show type bridge",
		"iptables-save -t filter", "iptables-save -t raw", "iptables-restore --noflush --wait 1",
		"ip6tables-save -t filter", "ip6tables-save -t raw", "ip6tables-restore --noflush --wait 1",
	}
	if got := f.commands(); !slices.Equal(got, want) {
		t.Errorf("commands:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	r := restores(f)
	if len(r) != 2 {
		t.Fatalf("%d restores; want one per family", len(r))
	}
	empty := map[string]savedTable{"filter": parseSavedTable(savedText(nil, "IPv4", "filter")), "raw": parseSavedTable(savedText(nil, "IPv4", "raw"))}
	if want := firewallPayload(empty, portFirewallRules(fwPorts(), nil, "IPv4")); r[0].stdin != want {
		t.Errorf("the IPv4 restore:\n%s\nwant:\n%s", r[0].stdin, want)
	}
	if !strings.Contains(r[1].stdin, "-A ls-saaaaaaaa-1 -s fd00::5/128") {
		t.Errorf("the IPv6 restore lacks the port's IPv6 address:\n%s", r[1].stdin)
	}
}

// A node holding the ports' firewall, as iptables-save prints it, is only read (SPEC-033 T228).
func TestConvergePorts_UnchangedOnlyReads(t *testing.T) {
	saved := map[string]string{}
	for _, family := range []string{"IPv4", "IPv6"} {
		for _, table := range []string{"filter", "raw"} {
			saved[family+" "+table] = savedFixture(t, "ports-"+strings.ToLower(family)+"-"+table)
		}
	}
	f := &fakeRunner{respond: fwNode{saved: saved}.respond}
	if err := realModeFirewall(f).ConvergePorts(context.Background(), fwPorts()); err != nil {
		t.Fatalf("converge: %v", err)
	}
	if r := restores(f); len(r) != 0 {
		t.Errorf("a node holding its firewall was written to: %q", r[0].stdin)
	}
}

func TestConvergePorts_AChangedPortRewritesItsChainsOnly(t *testing.T) {
	f := &fakeRunner{respond: fwNode{held: fwPorts()}.respond}
	changed := fwPorts()
	changed[1].Rules = append(slices.Clone(changed[1].Rules), FirewallRule{Direction: "ingress", Ethertype: "IPv4", Protocol: "tcp", PortMin: intp(443), PortMax: intp(443)})
	if err := realModeFirewall(f).ConvergePorts(context.Background(), changed); err != nil {
		t.Fatalf("converge: %v", err)
	}
	r := restores(f)
	if len(r) != 1 || r[0].name != "iptables-restore" {
		t.Fatalf("restores %v; want the IPv4 one alone", r)
	}
	want := "*filter\n:ls-iaaaaaaaa-1 - [0:0]\n-F ls-iaaaaaaaa-1\n"
	if !strings.HasPrefix(r[0].stdin, want) || strings.Contains(r[0].stdin, "*raw") {
		t.Errorf("the restore:\n%s\nwant the port's ingress chain alone", r[0].stdin)
	}
}

func TestConvergePorts_BoundsAndStub(t *testing.T) {
	f := &fakeRunner{respond: fwNode{}.respond}
	if err := realModeFirewall(f).ConvergePorts(context.Background(), fwPorts()); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if !c.hasDeadline || c.budget > time.Second {
			t.Errorf("%s %v: deadline %v, budget %s; want at most a second", c.name, c.args, c.hasDeadline, c.budget)
		}
	}

	t.Run("a failing restore fails the convergence", func(t *testing.T) {
		f := &fakeRunner{respond: func(ctx context.Context, c fakeCall) ([]byte, error) {
			if c.name == "iptables-restore" {
				return []byte("iptables-restore: line 9 failed"), errors.New("exit status 1")
			}
			return fwNode{}.respond(ctx, c)
		}}
		if err := realModeFirewall(f).ConvergePorts(context.Background(), fwPorts()); err == nil {
			t.Fatal("a failing restore: no error")
		}
	})

	t.Run("a blocking read fails within the second", func(t *testing.T) {
		f := &fakeRunner{respond: func(ctx context.Context, c fakeCall) ([]byte, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		start := time.Now()
		if err := realModeFirewall(f).ConvergePorts(context.Background(), fwPorts()); err == nil {
			t.Fatal("a blocking read: no error")
		}
		if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
			t.Errorf("the convergence took %s; want it to give up within about a second", elapsed)
		}
	})

	t.Run("stub mode runs nothing", func(t *testing.T) {
		f := &fakeRunner{}
		m := NewFirewallManager("stub")
		m.run = f.run
		if err := m.ConvergePorts(context.Background(), fwPorts()); err != nil {
			t.Fatal(err)
		}
		if len(f.calls) != 0 {
			t.Errorf("stub mode ran %q", f.commands())
		}
	})
}

// SPEC-033 round 2, T145 (research R48): teardown removes the node's whole port firewall — every port's
// chains, the fixed chains, the routers' forward chain and the retired chains, with every jump to them —
// in one restore per family, and nothing it does not recognise.
func TestRemoveAllChains(t *testing.T) {
	node := func(_ context.Context, c fakeCall) ([]byte, error) {
		if !strings.HasSuffix(c.name, "-save") {
			return nil, nil
		}
		family := map[string]string{"iptables-save": "IPv4", "ip6tables-save": "IPv6"}[c.name]
		table := c.args[len(c.args)-1]
		text := savedText(fwPorts(), family, table)
		if table == "filter" && family == "IPv4" {
			text = strings.Replace(text, "COMMIT\n",
				":KUBE-FORWARD - [0:0]\n:ls-other - [0:0]\n:ls-l3-fwd - [0:0]\n:LS-SG-0a1b2c3d - [0:0]\n"+
					"-A FORWARD -m comment --comment lightstack -j ls-l3-fwd\n"+
					"-A FORWARD -m comment --comment \"kubernetes forwarding rules\" -j KUBE-FORWARD\n"+
					"-A FORWARD -j ls-other\n"+
					"-A ls-l3-fwd -i eth0 -o lg+ -m comment --comment lightstack -j ACCEPT\n"+
					"-A LS-SG-0a1b2c3d -j DROP\n"+
					"COMMIT\n", 1)
		}
		return []byte(text), nil
	}
	f := &fakeRunner{respond: node}
	if err := realModeFirewall(f).RemoveAll(context.Background()); err != nil {
		t.Fatalf("remove: %v", err)
	}
	want := []string{
		"iptables-save -t filter", "iptables-save -t raw", "iptables-restore --noflush --wait 1",
		"ip6tables-save -t filter", "ip6tables-save -t raw", "ip6tables-restore --noflush --wait 1",
	}
	if got := f.commands(); !slices.Equal(got, want) {
		t.Errorf("commands:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	r := restores(f)
	if len(r) != 2 {
		t.Fatalf("%d restores; want one per family", len(r))
	}
	v4 := r[0].stdin
	for _, line := range []string{
		"-D INPUT -j ls-filter-top", "-D FORWARD -j ls-filter-top",
		"-D FORWARD -m comment --comment lightstack -j ls-l3-fwd",
		"-X ls-filter-top", "-X ls-local", "-X ls-bound-in", "-X ls-bound-out", "-X ls-sg-chain", "-X ls-sg-fallback", "-X ls-l3-fwd",
		"-X ls-iaaaaaaaa-1", "-X ls-oaaaaaaaa-1", "-X ls-saaaaaaaa-1", "-X LS-SG-0a1b2c3d",
		"*raw", "-D PREROUTING -j ls-ct", "-X ls-ct",
	} {
		if !strings.Contains(v4, line+"\n") {
			t.Errorf("the IPv4 restore lacks %q:\n%s", line, v4)
		}
	}
	for _, kept := range []string{"KUBE-FORWARD", "ls-other"} {
		if strings.Contains(v4, kept) {
			t.Errorf("the IPv4 restore touches %s, which is not Lightstack's:\n%s", kept, v4)
		}
	}
	// A chain is emptied before any chain is deleted: one may jump to another.
	if strings.Index(v4, "-X ") < strings.LastIndex(v4[:strings.Index(v4, "*raw")], "-F ") {
		t.Errorf("a chain was deleted before every chain was emptied:\n%s", v4)
	}

	t.Run("a node holding none of it is only read", func(t *testing.T) {
		f := &fakeRunner{respond: fwNode{}.respond}
		if err := realModeFirewall(f).RemoveAll(context.Background()); err != nil {
			t.Fatal(err)
		}
		if r := restores(f); len(r) != 0 {
			t.Errorf("restored %q", r[0].stdin)
		}
	})

	t.Run("stub mode runs nothing", func(t *testing.T) {
		f := &fakeRunner{}
		m := NewFirewallManager("stub")
		m.run = f.run
		if err := m.RemoveAll(context.Background()); err != nil || len(f.calls) != 0 {
			t.Errorf("%v after %q", err, f.commands())
		}
	})
}

// SPEC-033 T213, T248: a guest's TAP without a bound port is dropped on the networks' bridges only —
// those the node holds, a bridge with no bound port among them, and those its ports join — so a TAP of
// another program, on a bridge of its own or Docker's (br- and twelve hex digits), keeps its traffic.
func TestConvergePorts_DropsUnboundTAPsOnTheNetworksBridgesOnly(t *testing.T) {
	links := `[{"ifname":"br-0a1b2c3d","flags":["UP"]},{"ifname":"br-1234567890ab","flags":["UP"]},{"ifname":"docker0","flags":["UP"]},{"ifname":"br-net1","flags":["UP"]}]`
	f := &fakeRunner{respond: func(ctx context.Context, c fakeCall) ([]byte, error) {
		if c.name == "ip" && strings.Join(c.args, " ") == "-j link show type bridge" {
			return []byte(links), nil
		}
		return fwNode{}.respond(ctx, c)
	}}
	ports := []FirewallPort{{ID: fwPortA, MAC: "fa:16:3e:aa:00:01", Addresses: []string{"10.0.0.5"}, Bridge: "br-net1", Zone: 4097}}
	if err := realModeFirewall(f).ConvergePorts(context.Background(), ports); err != nil {
		t.Fatalf("converge: %v", err)
	}
	lines := strings.Split(restores(f)[0].stdin, "\n")
	for _, b := range []string{"br-0a1b2c3d", "br-net1"} {
		for _, rule := range []string{
			"-A ls-local -i " + b + " -m physdev --physdev-in tap+ -j ls-bound-in",
			"-A ls-local -o " + b + " -m physdev --physdev-out tap+ --physdev-is-bridged -j ls-bound-out",
		} {
			if !slices.Contains(lines, rule) {
				t.Errorf("the restore lacks %q", rule)
			}
		}
	}
	for _, line := range lines {
		if strings.Contains(line, "tap+") && (strings.Contains(line, "br-1234567890ab") || strings.Contains(line, "docker0") || !strings.Contains(line, " br-")) {
			t.Errorf("a TAP guard beyond the networks' bridges: %q", line)
		}
	}
}
