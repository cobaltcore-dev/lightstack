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
		PIDFile:        "/tmp/pid",
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
