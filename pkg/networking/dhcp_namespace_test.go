package networking

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// SPEC-033 round 2, T119 (FR-037; research R29; contracts/router-namespace-dataplane.md): a network's
// DHCP on the network node — dnsmasq's configuration, the namespace and its device, and the real-mode
// convergence — rendered from its records.

const (
	testDHCPNetwork = "4d5e6f70-0000-4000-8000-000000000004"
	testDHCPPort    = "ee55ff667788-4b82-9d34-e5f6a7b8c9d0"
	testSubnetA     = "aaaa0000-0000-4000-8000-00000000000a"
	testSubnetB     = "bbbb0000-0000-4000-8000-00000000000b"
	testQDHCP       = "qdhcp-" + testDHCPNetwork
	testDHCPDevice  = "tapee55ff66778"
	testDHCPHostEnd = "ld-ee55ff66778"
)

// testDHCP is a network with two DHCP subnets, the second without a gateway, and two ports.
func testDHCP() DHCPState {
	return DHCPState{
		NetworkID: testDHCPNetwork,
		PortID:    testDHCPPort,
		Bridge:    BridgeNameForNetwork(testDHCPNetwork),
		Subnets: []DHCPSubnet{
			{ID: testSubnetB, CIDR: "10.81.0.0/24", Address: "10.81.0.2", DNS: nil},
			{ID: testSubnetA, CIDR: "10.80.0.0/24", Address: "10.80.0.2", Gateway: "10.80.0.1", DNS: []string{"192.0.2.53", "192.0.2.54"}},
		},
		Hosts: []DHCPHost{
			{MAC: "fa:16:3e:00:00:02", IP: "10.81.0.7"},
			{MAC: "fa:16:3e:00:00:01", IP: "10.80.0.5"},
		},
	}
}

func TestRenderDNSMasq(t *testing.T) {
	want := strings.Join([]string{
		"# dnsmasq of network " + testDHCPNetwork + ", Lightstack's",
		"interface=" + testDHCPDevice,
		"bind-interfaces",
		"except-interface=lo",
		"port=0",
		"dhcp-leasefile=/state/dhcp/leases/" + testDHCPNetwork + ".leases",
		"dhcp-range=set:subnet-" + testSubnetA + ",10.80.0.0,static,255.255.255.0,86400s",
		"dhcp-option=tag:subnet-" + testSubnetA + ",option:router,10.80.0.1",
		"dhcp-option=tag:subnet-" + testSubnetA + ",option:classless-static-route,169.254.169.254/32,10.80.0.2,0.0.0.0/0,10.80.0.1",
		"dhcp-option=tag:subnet-" + testSubnetA + ",option:dns-server,192.0.2.53,192.0.2.54",
		"dhcp-range=set:subnet-" + testSubnetB + ",10.81.0.0,static,255.255.255.0,86400s",
		"dhcp-option=tag:subnet-" + testSubnetB + ",option:classless-static-route,169.254.169.254/32,10.81.0.2",
		"dhcp-host=fa:16:3e:00:00:01,10.80.0.5",
		"dhcp-host=fa:16:3e:00:00:02,10.81.0.7",
		"",
	}, "\n")
	if got := renderDNSMasq(testDHCP(), "/state/dhcp/leases/"+testDHCPNetwork+".leases"); got != want {
		t.Errorf("dnsmasq configuration:\n%s\nwant:\n%s", got, want)
	}
}

func TestPlanDHCP(t *testing.T) {
	p := planDHCP(testDHCP())
	want := dhcpPlan{
		Namespace: testQDHCP,
		Device:    testDHCPDevice,
		HostEnd:   testDHCPHostEnd,
		Bridge:    BridgeNameForNetwork(testDHCPNetwork),
		Addresses: []string{"10.80.0.2/24", "10.81.0.2/24", MetadataIP + "/32"},
	}
	if p.Namespace != want.Namespace || p.Device != want.Device || p.HostEnd != want.HostEnd || p.Bridge != want.Bridge ||
		!slices.Equal(p.Addresses, want.Addresses) {
		t.Errorf("plan %+v; want %+v", p, want)
	}
}

// dhcpNode answers the runner's reads as a node holding the network's DHCP in state would, or as a
// node without it when state is nil. half holds it as a pass cut short leaves it: the namespace and its
// device made, lo and both ends of the device down and the host's end on no bridge.
type dhcpNode struct {
	state *DHCPState
	half  bool
}

func (n dhcpNode) respond(_ context.Context, c fakeCall) ([]byte, error) {
	cmd := c.name + " " + strings.Join(c.args, " ")
	switch cmd {
	case "ip netns list":
		if n.state == nil {
			return []byte(""), nil
		}
		return []byte(testQDHCP + " (id: 9)\n"), nil
	case "ip -n " + testQDHCP + " -j addr show":
		var info []string
		for _, a := range planDHCP(*n.state).Addresses {
			ip, prefix, _ := strings.Cut(a, "/")
			info = append(info, `{"family":"inet","local":"`+ip+`","prefixlen":`+prefix+`}`)
		}
		flags := `"flags":["BROADCAST","MULTICAST","UP","LOWER_UP"]`
		if n.half {
			flags = `"flags":["BROADCAST","MULTICAST"]`
		}
		return []byte(`[{"ifname":"lo",` + flags + `,"addr_info":[]},{"ifname":"` + testDHCPDevice + `",` + flags + `,"addr_info":[` + strings.Join(info, ",") + `]}]`), nil
	case "ip -j link show":
		if n.half {
			return []byte(`[{"ifname":"` + testDHCPHostEnd + `","flags":["BROADCAST","MULTICAST"]}]`), nil
		}
		return []byte(`[{"ifname":"` + testDHCPHostEnd + `","master":"` + BridgeNameForNetwork(testDHCPNetwork) + `","flags":["BROADCAST","MULTICAST","UP","LOWER_UP"]}]`), nil
	}
	if strings.HasPrefix(cmd, "pkill -f ") {
		return nil, errors.New("exit status 1") // none matched
	}
	return nil, nil
}

// fakeSpawner starts no process: it records the command lines and hands out pids, which stay alive
// until killed.
type fakeSpawner struct {
	mu      sync.Mutex
	started [][]string
	alive   map[int]bool
	next    int
	fail    error
}

func (s *fakeSpawner) spawn(args []string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return 0, s.fail
	}
	s.started = append(s.started, slices.Clone(args))
	s.next++
	if s.alive == nil {
		s.alive = map[int]bool{}
	}
	s.alive[1000+s.next] = true
	return 1000 + s.next, nil
}

func (s *fakeSpawner) isAlive(pid int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.alive[pid]
}

// realModeDHCP is a DHCP manager in real mode whose commands, processes and files are the test's.
func realModeDHCP(t *testing.T, f *fakeRunner, s *fakeSpawner) *DHCPManager {
	t.Helper()
	dir := t.TempDir()
	m := NewDHCPManager("iptables")
	m.run, m.spawn, m.alive = f.run, s.spawn, s.isAlive
	m.configPath, m.leasePath = filepath.Join(dir, "configs"), filepath.Join(dir, "leases")
	for _, d := range []string{m.configPath, m.leasePath} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

func dhcpWrites(f *fakeRunner) []string {
	var out []string
	for _, c := range f.commands() {
		if c != "ip netns list" && !strings.Contains(c, " -j ") && !strings.HasPrefix(c, "pkill ") {
			out = append(out, c)
		}
	}
	return out
}

func TestConvergeDHCP_Adding(t *testing.T) {
	f, s := &fakeRunner{respond: dhcpNode{}.respond}, &fakeSpawner{}
	m := realModeDHCP(t, f, s)
	state := testDHCP()
	if err := m.ConvergeDHCP(context.Background(), testDHCPNetwork, &state); err != nil {
		t.Fatalf("converge: %v", err)
	}
	want := []string{
		"ip netns add " + testQDHCP,
		"ip -n " + testQDHCP + " link set lo up",
		"ip link add " + testDHCPHostEnd + " type veth peer name " + testDHCPDevice + " netns " + testQDHCP,
		"ip link set " + testDHCPHostEnd + " master " + BridgeNameForNetwork(testDHCPNetwork),
		"ip link set " + testDHCPHostEnd + " up",
		"ip -n " + testQDHCP + " link set " + testDHCPDevice + " up",
		"ip -n " + testQDHCP + " addr replace 10.80.0.2/24 dev " + testDHCPDevice,
		"ip -n " + testQDHCP + " addr replace 10.81.0.2/24 dev " + testDHCPDevice,
		"ip -n " + testQDHCP + " addr replace " + MetadataIP + "/32 dev " + testDHCPDevice,
	}
	if got := dhcpWrites(f); !slices.Equal(got, want) {
		t.Errorf("writes:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	conf := filepath.Join(m.configPath, testDHCPNetwork+".conf")
	written, err := os.ReadFile(conf)
	if err != nil || string(written) != renderDNSMasq(state, filepath.Join(m.leasePath, testDHCPNetwork+".leases")) {
		t.Errorf("the configuration on disk (%v):\n%s\nwant the rendered one", err, written)
	}
	wantStart := []string{"ip", "netns", "exec", testQDHCP, "dnsmasq", "--conf-file=" + conf, "--no-daemon", "--log-facility=-"}
	if len(s.started) != 1 || !slices.Equal(s.started[0], wantStart) {
		t.Errorf("started %q; want one dnsmasq inside the namespace: %q", s.started, wantStart)
	}
	if !slices.Contains(f.commands(), "pkill -f "+conf) {
		t.Error("no dnsmasq left over from an earlier process was looked for before the start")
	}
}

func TestConvergeDHCP_UnchangedOnlyReads(t *testing.T) {
	state := testDHCP()
	f, s := &fakeRunner{respond: dhcpNode{}.respond}, &fakeSpawner{}
	m := realModeDHCP(t, f, s)
	if err := m.ConvergeDHCP(context.Background(), testDHCPNetwork, &state); err != nil {
		t.Fatal(err)
	}
	f2 := &fakeRunner{respond: dhcpNode{state: &state}.respond}
	m.run = f2.run
	if err := m.ConvergeDHCP(context.Background(), testDHCPNetwork, &state); err != nil {
		t.Fatal(err)
	}
	if got := dhcpWrites(f2); len(got) != 0 {
		t.Errorf("a network's DHCP as its records say was written to:\n%s", strings.Join(got, "\n"))
	}
	if len(s.started) != 1 {
		t.Errorf("dnsmasq started %d times; want once", len(s.started))
	}
}

// SPEC-033 T191: a pass cut short leaves the namespace and its device half made; the next pass
// reads what they lack and finishes it — lo, the host's end on the network's bridge, both ends up —
// writing nothing else.
func TestConvergeDHCP_FinishesWhatAPassCutShort(t *testing.T) {
	state := testDHCP()
	f, s := &fakeRunner{respond: dhcpNode{}.respond}, &fakeSpawner{}
	m := realModeDHCP(t, f, s)
	if err := m.ConvergeDHCP(context.Background(), testDHCPNetwork, &state); err != nil {
		t.Fatal(err)
	}
	f2 := &fakeRunner{respond: dhcpNode{state: &state, half: true}.respond}
	m.run = f2.run
	if err := m.ConvergeDHCP(context.Background(), testDHCPNetwork, &state); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"ip -n " + testQDHCP + " link set lo up",
		"ip link set " + testDHCPHostEnd + " master " + BridgeNameForNetwork(testDHCPNetwork),
		"ip link set " + testDHCPHostEnd + " up",
		"ip -n " + testQDHCP + " link set " + testDHCPDevice + " up",
	}
	if got := dhcpWrites(f2); !slices.Equal(got, want) {
		t.Errorf("writes:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestConvergeDHCP_ChangedHostsRestartDNSMasq(t *testing.T) {
	state := testDHCP()
	f, s := &fakeRunner{respond: dhcpNode{}.respond}, &fakeSpawner{}
	m := realModeDHCP(t, f, s)
	if err := m.ConvergeDHCP(context.Background(), testDHCPNetwork, &state); err != nil {
		t.Fatal(err)
	}
	lease := filepath.Join(m.leasePath, testDHCPNetwork+".leases")
	if err := os.WriteFile(lease, []byte("granted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed := testDHCP()
	changed.Hosts = append(changed.Hosts, DHCPHost{MAC: "fa:16:3e:00:00:03", IP: "10.80.0.9"})
	f2 := &fakeRunner{respond: dhcpNode{state: &state}.respond}
	m.run = f2.run
	if err := m.ConvergeDHCP(context.Background(), testDHCPNetwork, &changed); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(f2.commands(), "kill 1001") {
		t.Errorf("commands %q; want the running dnsmasq stopped", f2.commands())
	}
	if len(s.started) != 2 {
		t.Errorf("dnsmasq started %d times; want again after the change", len(s.started))
	}
	if got, err := os.ReadFile(lease); err != nil || string(got) != "granted\n" {
		t.Errorf("the lease file after the restart: %q, %v; want the granted leases kept", got, err)
	}
}

func TestConvergeDHCP_Removing(t *testing.T) {
	state := testDHCP()
	f, s := &fakeRunner{respond: dhcpNode{}.respond}, &fakeSpawner{}
	m := realModeDHCP(t, f, s)
	if err := m.ConvergeDHCP(context.Background(), testDHCPNetwork, &state); err != nil {
		t.Fatal(err)
	}
	f2 := &fakeRunner{respond: dhcpNode{state: &state}.respond}
	m.run = f2.run
	if err := m.ConvergeDHCP(context.Background(), testDHCPNetwork, nil); err != nil {
		t.Fatalf("converge to absent: %v", err)
	}
	want := []string{"kill 1001", "ip link del " + testDHCPHostEnd, "ip netns delete " + testQDHCP}
	if got := dhcpWrites(f2); !slices.Equal(got, want) {
		t.Errorf("writes %q; want %q", got, want)
	}
	for _, file := range []string{filepath.Join(m.configPath, testDHCPNetwork+".conf"), filepath.Join(m.leasePath, testDHCPNetwork+".leases")} {
		if _, err := os.Stat(file); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is still there (%v)", file, err)
		}
	}
}

func TestConvergeDHCP_BoundsAndStub(t *testing.T) {
	state := testDHCP()
	f, s := &fakeRunner{respond: dhcpNode{}.respond}, &fakeSpawner{}
	if err := realModeDHCP(t, f, s).ConvergeDHCP(context.Background(), testDHCPNetwork, &state); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if !c.hasDeadline || c.budget > time.Second {
			t.Errorf("%s %v: deadline %v, budget %s; want at most a second", c.name, c.args, c.hasDeadline, c.budget)
		}
	}

	t.Run("a failing command fails the convergence, and dnsmasq is not started", func(t *testing.T) {
		f := &fakeRunner{respond: func(ctx context.Context, c fakeCall) ([]byte, error) {
			if slices.Contains(c.args, "veth") {
				return []byte("RTNETLINK answers: File exists"), errors.New("exit status 2")
			}
			return dhcpNode{}.respond(ctx, c)
		}}
		s := &fakeSpawner{}
		if err := realModeDHCP(t, f, s).ConvergeDHCP(context.Background(), testDHCPNetwork, &state); err == nil {
			t.Fatal("a failing leg: no error")
		}
		if len(s.started) != 0 {
			t.Error("dnsmasq was started after the leg failed")
		}
	})

	t.Run("a failing start fails the convergence", func(t *testing.T) {
		f, s := &fakeRunner{respond: dhcpNode{}.respond}, &fakeSpawner{fail: errors.New("exec: no dnsmasq")}
		if err := realModeDHCP(t, f, s).ConvergeDHCP(context.Background(), testDHCPNetwork, &state); err == nil {
			t.Fatal("a failing start: no error")
		}
	})

	t.Run("stub mode runs nothing", func(t *testing.T) {
		f, s := &fakeRunner{}, &fakeSpawner{}
		m := NewDHCPManager("stub")
		m.run, m.spawn, m.alive = f.run, s.spawn, s.isAlive
		if err := m.ConvergeDHCP(context.Background(), testDHCPNetwork, &state); err != nil {
			t.Fatal(err)
		}
		if err := m.ConvergeDHCP(context.Background(), testDHCPNetwork, nil); err != nil {
			t.Fatal(err)
		}
		if len(f.calls) != 0 || len(s.started) != 0 {
			t.Errorf("stub mode ran %q and started %q", f.commands(), s.started)
		}
	})
}

// SPEC-033 round 2, T142: the network node lists the networks whose DHCP it holds, by their
// namespaces, and a dnsmasq that is gone — its process died with an earlier one of Lightstack's — is
// started again with the namespace left as it is.

func TestHeldNetworks(t *testing.T) {
	f := &fakeRunner{respond: func(context.Context, fakeCall) ([]byte, error) {
		return []byte("qdhcp-" + testDHCPNetwork + " (id: 9)\nqdhcp-other\nqrouter-" + testDHCPNetwork + "\ncni-1\n"), nil
	}}
	got, err := realModeDHCP(t, f, &fakeSpawner{}).HeldNetworks(context.Background())
	if err != nil || !slices.Equal(got, []string{testDHCPNetwork}) {
		t.Errorf("held = %q, %v; want %q", got, err, testDHCPNetwork)
	}

	t.Run("a failed listing is an error", func(t *testing.T) {
		f := &fakeRunner{respond: func(context.Context, fakeCall) ([]byte, error) { return nil, errors.New("exit status 1") }}
		if _, err := realModeDHCP(t, f, &fakeSpawner{}).HeldNetworks(context.Background()); err == nil {
			t.Fatal("no error")
		}
	})

	t.Run("stub mode holds none and runs nothing", func(t *testing.T) {
		f := &fakeRunner{}
		m := NewDHCPManager("stub")
		m.run = f.run
		if got, err := m.HeldNetworks(context.Background()); err != nil || len(got) != 0 || len(f.calls) != 0 {
			t.Errorf("= %q, %v after %q", got, err, f.commands())
		}
	})
}

func TestConvergeDHCP_AfterARestartStartsDNSMasqAgain(t *testing.T) {
	state := testDHCP()
	f, s := &fakeRunner{respond: dhcpNode{}.respond}, &fakeSpawner{}
	m := realModeDHCP(t, f, s)
	if err := m.ConvergeDHCP(context.Background(), testDHCPNetwork, &state); err != nil {
		t.Fatal(err)
	}
	// The next process: the namespace, its device and the configuration are there; the dnsmasq it
	// does not know of died with the one before.
	f2, s2 := &fakeRunner{respond: dhcpNode{state: &state}.respond}, &fakeSpawner{}
	next := NewDHCPManager("iptables")
	next.run, next.spawn, next.alive = f2.run, s2.spawn, s2.isAlive
	next.configPath, next.leasePath = m.configPath, m.leasePath
	if err := next.ConvergeDHCP(context.Background(), testDHCPNetwork, &state); err != nil {
		t.Fatal(err)
	}
	if got := dhcpWrites(f2); len(got) != 0 {
		t.Errorf("the namespace was written to:\n%s", strings.Join(got, "\n"))
	}
	conf := filepath.Join(m.configPath, testDHCPNetwork+".conf")
	if !slices.Contains(f2.commands(), "pkill -f "+conf) || len(s2.started) != 1 {
		t.Errorf("commands %q, started %q; want any dnsmasq left on the configuration stopped and one started",
			f2.commands(), s2.started)
	}
}

// SPEC-033 T172 (FR-060): a subnet's DNS servers and gateway went
// into dnsmasq's configuration as stored, and dnsmasq starts as root, so a newline in either wrote a
// directive. The renderer writes only what parses as an IPv4 address and leaves the rest out.
func TestRenderDNSMasqWritesOnlyAddresses(t *testing.T) {
	state := testDHCP()
	state.Subnets[0].Gateway = "10.81.0.1\nconf-file=/etc/shadow"
	state.Subnets[1].DNS = []string{"192.0.2.53", "192.0.2.54\ndhcp-script=/bin/sh", "2001:db8::53", "dns.example",
		"010.0.0.1", "192.0.2.55"}
	want := strings.Join([]string{
		"# dnsmasq of network " + testDHCPNetwork + ", Lightstack's",
		"interface=" + testDHCPDevice,
		"bind-interfaces",
		"except-interface=lo",
		"port=0",
		"dhcp-leasefile=/state/dhcp/leases/" + testDHCPNetwork + ".leases",
		"dhcp-range=set:subnet-" + testSubnetA + ",10.80.0.0,static,255.255.255.0,86400s",
		"dhcp-option=tag:subnet-" + testSubnetA + ",option:router,10.80.0.1",
		"dhcp-option=tag:subnet-" + testSubnetA + ",option:classless-static-route,169.254.169.254/32,10.80.0.2,0.0.0.0/0,10.80.0.1",
		"dhcp-option=tag:subnet-" + testSubnetA + ",option:dns-server,192.0.2.53,192.0.2.55",
		"dhcp-range=set:subnet-" + testSubnetB + ",10.81.0.0,static,255.255.255.0,86400s",
		"dhcp-option=tag:subnet-" + testSubnetB + ",option:classless-static-route,169.254.169.254/32,10.81.0.2",
		"dhcp-host=fa:16:3e:00:00:01,10.80.0.5",
		"dhcp-host=fa:16:3e:00:00:02,10.81.0.7",
		"",
	}, "\n")
	if got := renderDNSMasq(state, "/state/dhcp/leases/"+testDHCPNetwork+".leases"); got != want {
		t.Errorf("dnsmasq configuration:\n%s\nwant:\n%s", got, want)
	}
}

// SPEC-033 T247: a DHCP subnet whose range is not an IPv4 prefix — an IPv6 CIDR stored as IPv4 before
// the API refused one — is left out of the network's DHCP, with its ports' reservations, and so is one
// whose DHCP address is not an IPv4 address in its range: dnsmasq's IPv4 range has no mask for a /64,
// and a renderer that panics takes the network node's reconciler, and the process, with it.
func TestDHCPLeavesOutSubnetsThatAreNotIPv4(t *testing.T) {
	state := testDHCP()
	state.Subnets = append(state.Subnets,
		DHCPSubnet{ID: "cccc0000-0000-4000-8000-00000000000c", CIDR: "2001:db8:93::/64", Address: "2001:db8:93::2"},
		DHCPSubnet{ID: "dddd0000-0000-4000-8000-00000000000d", CIDR: "10.82.0.0/24", Address: "2001:db8:93::3"},
		DHCPSubnet{ID: "eeee0000-0000-4000-8000-00000000000e", CIDR: "10.83.0.0/24", Address: "10.99.0.2"})
	// A port's reservation on such a subnet goes with it: dnsmasq refuses an IPv6 dhcp-host unbracketed.
	state.Hosts = append(state.Hosts, DHCPHost{MAC: "fa:16:3e:00:00:03", IP: "2001:db8:93::5"},
		DHCPHost{MAC: "fa:16:3e:00:00:04", IP: "10.83.0.5"})
	var rendered string
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("rendering panicked: %v", r)
			}
		}()
		rendered = renderDNSMasq(state, "/state/dhcp/leases/"+testDHCPNetwork+".leases")
	}()
	if want := renderDNSMasq(testDHCP(), "/state/dhcp/leases/"+testDHCPNetwork+".leases"); rendered != want {
		t.Errorf("dnsmasq configuration:\n%s\nwant the IPv4 subnets' alone:\n%s", rendered, want)
	}
	if got, want := planDHCP(state).Addresses, planDHCP(testDHCP()).Addresses; !slices.Equal(got, want) {
		t.Errorf("addresses %q; want the IPv4 subnets' alone, %q", got, want)
	}
	if got := netmask(64); got != "255.255.255.255" {
		t.Errorf("netmask(64) = %q; want it held to an IPv4 mask", got)
	}
}
