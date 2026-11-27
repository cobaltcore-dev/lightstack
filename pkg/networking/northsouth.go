package networking

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// DefaultUplinkInterface returns the host's primary uplink: the interface the
// kernel would use to reach the public internet. Single-node WS-3 masquerades
// tenant egress (and hosts floating IPs) out this interface, auto-mapping the
// external network to the node uplink — no operator-created br-ex/NIC (SPEC-009
// research R10).
func DefaultUplinkInterface() (string, error) {
	// Ask the kernel directly which link reaches a public address — robust
	// across default-route representations (Dst nil vs 0.0.0.0/0, multiple
	// default routes, metrics).
	if routes, err := netlink.RouteGet(net.IPv4(1, 1, 1, 1)); err == nil {
		for _, r := range routes {
			if r.LinkIndex > 0 {
				if link, err := netlink.LinkByIndex(r.LinkIndex); err == nil {
					return link.Attrs().Name, nil
				}
			}
		}
	}
	// Fallback: scan the route table for the default route (Dst nil or 0.0.0.0/0).
	routes, err := netlink.RouteList(nil, unix.AF_INET)
	if err != nil {
		return "", fmt.Errorf("list routes: %w", err)
	}
	for _, r := range routes {
		if r.Dst == nil || r.Dst.IP.IsUnspecified() {
			if link, err := netlink.LinkByIndex(r.LinkIndex); err == nil {
				return link.Attrs().Name, nil
			}
		}
	}
	return "", fmt.Errorf("no IPv4 default route / uplink found")
}

// commandTimeout bounds each command of the node's dataplane (Article III, FR-007b). A command that
// overruns is killed and counts as failed.
const commandTimeout = time.Second

// commandRunner runs one command with stdin under ctx and returns its combined output.
type commandRunner func(ctx context.Context, name string, args []string, stdin string) ([]byte, error)

// execRunner is the real commandRunner. The context kills the command when it overruns, and
// WaitDelay bounds the wait for its output after that.
func execRunner(ctx context.Context, name string, args []string, stdin string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	cmd.WaitDelay = 100 * time.Millisecond
	return cmd.CombinedOutput()
}

// enableIPv4Forwarding turns on net.ipv4.ip_forward so the host routes tenant
// traffic to the uplink. Idempotent.
func enableIPv4Forwarding() error {
	const p = "/proc/sys/net/ipv4/ip_forward"
	if cur, err := os.ReadFile(p); err == nil && len(cur) > 0 && cur[0] == '1' {
		return nil
	}
	if err := os.WriteFile(p, []byte("1\n"), 0o644); err != nil {
		return fmt.Errorf("enable ip_forward: %w", err)
	}
	return nil
}
