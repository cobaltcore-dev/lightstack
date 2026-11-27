package networking

import (
	"context"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// SPEC-033 T228: compare-before-write (SC-010) rests on the renderer writing a rule as iptables-save
// prints it back, since a node's tables are read with iptables-save and compared to the rendering line
// by line. The tests that a node already as its records say is only read answer iptables-save from
// testdata/iptables-save: the tables of fwPorts, protocolPort and testRouter as a fresh node's
// convergence writes them, loaded into a scratch network namespace by the real iptables-nft-restore and
// printed back by iptables-nft-save — the nf_tables backend, which the image's iptables is.
// TestCaptureIPTablesSave recaptures them.

var updateIPTablesSave = flag.Bool("update", false, "recapture testdata/iptables-save from iptables-nft in an unprivileged network namespace")

const iptablesSaveDir = "testdata/iptables-save"

// savedFixture is a captured table as iptables-save printed it.
func savedFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(iptablesSaveDir, name+".txt"))
	if err != nil {
		t.Fatalf("read the captured %s: %v (recapture with go test ./pkg/networking -run TestCaptureIPTablesSave -update)", name, err)
	}
	return string(b)
}

// TestCaptureIPTablesSave recaptures testdata/iptables-save. It needs iptables-nft and its -save and
// -restore, and unprivileged user and network namespaces (unshare -rn); nothing on the host changes.
func TestCaptureIPTablesSave(t *testing.T) {
	if !*updateIPTablesSave {
		t.Skip("run with -update on a host with iptables-nft and unshare -rn to recapture testdata/iptables-save")
	}
	payloads := freshNodePayloads(t)
	for _, name := range []string{"iptables-restore", "ip6tables-restore", "protocols-iptables-restore",
		"protocols-ip6tables-restore", "nat", "mangle", "forward"} {
		if payloads[name] == "" {
			t.Fatalf("a fresh node's convergence wrote no %s", name)
		}
	}
	dir := t.TempDir()
	for name, body := range payloads {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	version, err := exec.Command("iptables-nft", "--version").Output()
	if err != nil {
		t.Fatalf("iptables-nft --version: %v", err)
	}
	// One scratch namespace per node: the ports' node, the router's namespace, the host's filter table.
	for _, run := range []struct {
		script string
		saves  map[string]string // fixture name: the -save command printing it
	}{
		{"iptables-nft-restore --noflush < iptables-restore\nip6tables-nft-restore --noflush < ip6tables-restore",
			map[string]string{"ports-ipv4-filter": "iptables-nft-save -t filter", "ports-ipv4-raw": "iptables-nft-save -t raw",
				"ports-ipv6-filter": "ip6tables-nft-save -t filter", "ports-ipv6-raw": "ip6tables-nft-save -t raw"}},
		{"iptables-nft-restore --noflush < protocols-iptables-restore\n" +
			"ip6tables-nft-restore --noflush < protocols-ip6tables-restore 2>/dev/null", // warns of extension headers
			map[string]string{"protocols-ipv4-filter": "iptables-nft-save -t filter", "protocols-ipv6-filter": "ip6tables-nft-save -t filter"}},
		{"iptables-nft-restore < nat\niptables-nft-restore < mangle",
			map[string]string{"router-nat": "iptables-nft-save -t nat", "router-mangle": "iptables-nft-save -t mangle"}},
		{"iptables-nft-restore --noflush < forward",
			map[string]string{"host-filter": "iptables-nft-save -t filter"}},
	} {
		script := "set -e\ncd " + dir + "\n" + run.script + "\n"
		for name, save := range run.saves {
			script += save + " > " + name + ".txt\n"
		}
		if out, err := exec.Command("unshare", "-rn", "sh", "-c", script).CombinedOutput(); err != nil {
			t.Fatalf("capture: %v\n%s", err, out)
		}
		for name := range run.saves {
			saved, err := os.ReadFile(filepath.Join(dir, name+".txt"))
			if err != nil {
				t.Fatal(err)
			}
			// iptables-save's own comments carry the date; the fixture names the version instead.
			body := "# " + strings.TrimSpace(string(version)) + ", captured by TestCaptureIPTablesSave\n"
			for _, line := range strings.SplitAfter(string(saved), "\n") {
				if !strings.HasPrefix(line, "#") {
					body += line
				}
			}
			if err := os.MkdirAll(iptablesSaveDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(iptablesSaveDir, name+".txt"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// freshNodePayloads are the restores a fresh node's convergence writes, by fixture input name: the
// ports' firewall per family, the protocol port's per family, and the router's nat, mangle and forward
// chain.
func freshNodePayloads(t *testing.T) map[string]string {
	t.Helper()
	ctx := context.Background()
	payloads := map[string]string{}
	for prefix, ports := range map[string][]FirewallPort{"": fwPorts(), "protocols-": {protocolPort()}} {
		fw := &fakeRunner{respond: fwNode{}.respond}
		if err := realModeFirewall(fw).ConvergePorts(ctx, ports); err != nil {
			t.Fatalf("converge the ports on a fresh node: %v", err)
		}
		for _, c := range restores(fw) {
			payloads[prefix+c.name] = c.stdin
		}
	}
	s := testRouter()
	rt := &fakeRunner{respond: routerNode{noForward: true}.respond}
	if err := realModeRouter(rt).ConvergeRouter(ctx, testRouterID, &s, testUplink); err != nil {
		t.Fatalf("converge the router on a fresh node: %v", err)
	}
	for _, c := range rt.calls {
		switch {
		case strings.HasPrefix(c.stdin, "*filter\n:ls-l3-fwd"):
			payloads["forward"] = c.stdin
		case strings.HasPrefix(c.stdin, "*nat"):
			payloads["nat"] = c.stdin
		case strings.HasPrefix(c.stdin, "*mangle"):
			payloads["mangle"] = c.stdin
		}
	}
	return payloads
}

// protocolPort carries one ingress rule per IP protocol, 0 to 255, in both families: each protocol as
// the renderer writes it, by name exactly where iptables prints one (SPEC-033 T249).
func protocolPort() FirewallPort {
	p := FirewallPort{ID: "ffffffff-6666-4222-8333-444455556666", MAC: "fa:16:3e:ff:00:06",
		Addresses: []string{"10.0.0.10", "fd00::10"}, Bridge: "br-net1", Zone: 4097}
	for n := range 256 {
		for _, family := range []string{"IPv4", "IPv6"} {
			p.Rules = append(p.Rules, FirewallRule{Direction: "ingress", Ethertype: family, Protocol: strconv.Itoa(n)})
		}
	}
	return p
}

// TestPortFirewallWritesEveryProtocolAsIPTablesPrintsIt: a rule of any protocol renders as iptables
// prints it back, so a node holding it compares equal and is not written again (SC-010, T249).
func TestPortFirewallWritesEveryProtocolAsIPTablesPrintsIt(t *testing.T) {
	for _, family := range []string{"IPv4", "IPv6"} {
		saved := parseSavedTable(savedFixture(t, "protocols-"+strings.ToLower(family)+"-filter"))
		for chain, rules := range portFirewallRules([]FirewallPort{protocolPort()}, nil, family)["filter"] {
			if got := saved.rules[chain]; !slices.Equal(got, rules) {
				t.Errorf("%s %s as iptables printed it:\n%s\nrendered:\n%s", family, chain,
					strings.Join(got, "\n"), strings.Join(rules, "\n"))
			}
		}
	}
}
