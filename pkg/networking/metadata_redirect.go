package networking

import (
	"fmt"
	"os/exec"
)

// MetadataIP is the well-known link-local address cloud-init and the EC2
// metadata datasource query for instance metadata.
const MetadataIP = "169.254.169.254"

// MetadataServicePort is the host port the O3K metadata service listens on. The
// guest hits MetadataIP:80; we DNAT that to the host on this port.
const MetadataServicePort = 8775

// MetadataRedirect programs the per-bridge DNAT that makes the link-local
// metadata address reachable from guests on a tenant bridge (SPEC-009 WS-4).
//
// Single-node model: bridges live in the host/default namespace and double as
// the tenant L3 gateway, so the redirect is a default-namespace nat rule and the
// guest reaches it via a DHCP classless-static route (169.254.169.254/32 via the
// gateway, emitted by dnsmasq). The DNAT preserves the source IP so the metadata
// service can identify the instance by its fixed IP. Mirrors RouterManager:
// mode-gated, exec-based, idempotent (check with -C before -A).
type MetadataRedirect struct {
	mode string // "stub", "iptables", or "ebpf"
}

// NewMetadataRedirect creates a metadata-redirect programmer.
func NewMetadataRedirect(mode string) *MetadataRedirect {
	return &MetadataRedirect{mode: mode}
}

// Ensure installs the DNAT rule for one bridge if it is not already present.
// gatewayIP is the bridge's own address (where the host metadata service is
// reachable, since it binds all interfaces). No-op in stub mode and idempotent
// so the reconciler can call it every pass (SPEC-009 WS-4 / T029).
func (mr *MetadataRedirect) Ensure(bridgeName, gatewayIP string) error {
	if mr.mode == "stub" {
		return nil
	}
	if bridgeName == "" || gatewayIP == "" {
		return fmt.Errorf("metadata redirect: bridge name and gateway IP are required")
	}
	dest := fmt.Sprintf("%s:%d", gatewayIP, MetadataServicePort)
	match := []string{
		"-i", bridgeName,
		"-d", MetadataIP,
		"-p", "tcp", "--dport", "80",
		"-j", "DNAT", "--to-destination", dest,
	}
	// Idempotent: -C exits 0 only if the exact rule already exists.
	check := append([]string{"-t", "nat", "-C", "PREROUTING"}, match...)
	if exec.Command("iptables", check...).Run() == nil {
		return nil
	}
	add := append([]string{"-t", "nat", "-A", "PREROUTING"}, match...)
	if output, err := exec.Command("iptables", add...).CombinedOutput(); err != nil {
		return fmt.Errorf("failed to add metadata DNAT for %s: %w, output: %s", bridgeName, err, output)
	}
	return nil
}

// Remove deletes the DNAT rule for one bridge (best-effort, for subnet teardown).
func (mr *MetadataRedirect) Remove(bridgeName, gatewayIP string) error {
	if mr.mode == "stub" || bridgeName == "" || gatewayIP == "" {
		return nil
	}
	dest := fmt.Sprintf("%s:%d", gatewayIP, MetadataServicePort)
	del := []string{
		"-t", "nat", "-D", "PREROUTING",
		"-i", bridgeName,
		"-d", MetadataIP,
		"-p", "tcp", "--dport", "80",
		"-j", "DNAT", "--to-destination", dest,
	}
	_, _ = exec.Command("iptables", del...).CombinedOutput()
	return nil
}
