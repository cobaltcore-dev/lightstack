package networking

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// SPEC-024 (cloud-storage#546) FR-023a — a gatewayless subnet keeps its metadata route.
//
// The template used to guard BOTH options on the same value:
//
//	{{ if .GatewayIP }}dhcp-option=3,{{ .GatewayIP }}
//	dhcp-option=121,169.254.169.254/32,{{ .GatewayIP }},…{{ end }}
//
// option 3 is the default route; option 121 is the classless static route that carries
// 169.254.169.254 — the metadata service. Honouring "no gateway" (FR-023) would therefore have
// silently dropped metadata as well, reintroducing the exact guest symptom F10 was filed for
// ("169.254.169.254 request failed"). The two are separate concerns: declining a default route is not
// declining instance metadata.

// renderDHCPConfig writes a config to a temp file through the real writer and returns its contents.
func renderDHCPConfig(t *testing.T, cfg DHCPConfig) string {
	t.Helper()

	m := NewDHCPManager("stub")
	path := filepath.Join(t.TempDir(), "dnsmasq.conf")
	if err := m.writeDNSMasqConfig(path, cfg); err != nil {
		t.Fatalf("write dnsmasq config: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read dnsmasq config: %v", err)
	}
	return string(raw)
}

func baseDHCPConfig() DHCPConfig {
	return DHCPConfig{
		NetworkID:      "net-1",
		BridgeName:     "br-abcdef12",
		DHCPRangeStart: "10.0.0.100",
		DHCPRangeEnd:   "10.0.0.200",
		LeaseTime:      "12h",
		LeaseFile:      "/tmp/leases",
	}
}

// TestGatewaylessSubnetKeepsMetadataRoute — contract P12.
func TestGatewaylessSubnetKeepsMetadataRoute(t *testing.T) {
	cfg := baseDHCPConfig()
	cfg.GatewayIP = ""
	cfg.MetadataNextHop = "10.0.0.1" // the bridge's own on-link address

	out := renderDHCPConfig(t, cfg)

	if strings.Contains(out, "dhcp-option=3,") {
		t.Errorf("a gatewayless subnet must advertise no default route (option 3); got:\n%s", out)
	}
	if !strings.Contains(out, "dhcp-option=121,169.254.169.254/32,10.0.0.1") {
		t.Errorf("the metadata route (option 121) must survive a gatewayless subnet — this is FR-023a; got:\n%s", out)
	}
	// Without a gateway there is no default route to push, so the 0.0.0.0/0 leg must be absent.
	if strings.Contains(out, "0.0.0.0/0") {
		t.Errorf("a gatewayless subnet must not push a default route via option 121; got:\n%s", out)
	}
}

// TestSubnetWithGatewayKeepsBothOptions — contract P15: the existing behaviour must not regress.
func TestSubnetWithGatewayKeepsBothOptions(t *testing.T) {
	cfg := baseDHCPConfig()
	cfg.GatewayIP = "10.0.0.1"

	out := renderDHCPConfig(t, cfg)

	if !strings.Contains(out, "dhcp-option=3,10.0.0.1") {
		t.Errorf("a subnet with a gateway must still advertise the default route; got:\n%s", out)
	}
	if !strings.Contains(out, "dhcp-option=121,169.254.169.254/32,10.0.0.1,0.0.0.0/0,10.0.0.1") {
		t.Errorf("a subnet with a gateway must still push metadata and the default route via option 121; got:\n%s", out)
	}
}

// TestNoMetadataRouteWithoutANextHop: if neither a gateway nor an on-link next hop is known there is
// nothing to point at, and emitting a malformed option would break dnsmasq's startup for the whole
// network. Absent is the correct rendering.
func TestNoMetadataRouteWithoutANextHop(t *testing.T) {
	cfg := baseDHCPConfig()
	cfg.GatewayIP = ""
	cfg.MetadataNextHop = ""

	out := renderDHCPConfig(t, cfg)

	if strings.Contains(out, "dhcp-option=121") {
		t.Errorf("without a next hop there is nothing to route through; got:\n%s", out)
	}
	if strings.Contains(out, "dhcp-option=3,") {
		t.Errorf("no gateway means no default route; got:\n%s", out)
	}
}

// Branch review 2026-08-28, SRC-3 — the metadata route must be rendered from MetadataIP, not from a
// literal repeated in the template.
//
// This template and the iptables DNAT in metadata_redirect.go are two halves of one path: the rule
// intercepts 169.254.169.254 on the bridge, this option tells guests to route there. Spelled
// independently they can diverge — change the constant and the interception moves while DHCP keeps
// advertising the old address, with nothing failing to compile and nothing logged. Guests would route
// to an address no rule catches, which is exactly the invisible breakage the reconciler's
// destination comparison was added to find.
//
// The assertion is deliberately indirect: rather than checking for the literal (which would pass just
// as well if the template hardcoded it), it renders with both code paths and requires each to contain
// MetadataIP. If someone reverts the template to a literal and the constant later changes, this fails.
func TestMetadataRouteIsRenderedFromTheConstant(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  DHCPConfig
	}{
		{"with a gateway", DHCPConfig{
			NetworkID: "n1", BridgeName: "br-n1", SubnetCIDR: "10.0.0.0/24", GatewayIP: "10.0.0.1",
			DHCPRangeStart: "10.0.0.10", DHCPRangeEnd: "10.0.0.250", LeaseTime: "24h",
		}},
		{"gatewayless, via the bridge address", DHCPConfig{
			NetworkID: "n2", BridgeName: "br-n2", SubnetCIDR: "10.0.1.0/24", MetadataNextHop: "10.0.1.1",
			DHCPRangeStart: "10.0.1.10", DHCPRangeEnd: "10.0.1.250", LeaseTime: "24h",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := renderDHCPConfig(t, tc.cfg)
			want := "dhcp-option=121," + MetadataIP + "/32,"
			if !strings.Contains(out, want) {
				t.Errorf("rendered config does not route %s; want a line containing %q, got:\n%s",
					MetadataIP, want, out)
			}
		})
	}
}
