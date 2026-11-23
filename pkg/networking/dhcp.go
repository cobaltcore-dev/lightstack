package networking

import (
	"os"
	"path/filepath"
	"sync"

	"github.com/cobaltcore-dev/lightstack/pkg/paths"
)

// DHCPManager converges networks' DHCP on the network node: a namespace, a device and a dnsmasq per
// network (ConvergeDHCP, SPEC-033 R29). Its files live under the state path: a configuration and a
// lease file per network.
type DHCPManager struct {
	mode        string // "stub", "iptables", or "ebpf"
	leasePath   string
	configPath  string
	mu          sync.Mutex
	runningPIDs map[string]int // networkID -> the PID of the dnsmasq this process started

	// run, spawn and alive reach the node: commands, long-running processes and their liveness.
	// Tests replace them so the convergence is testable without root.
	run   commandRunner
	spawn func(args []string) (int, error)
	alive func(pid int) bool
}

// NewDHCPManager creates a DHCP manager; in real mode it makes its directories.
func NewDHCPManager(mode string) *DHCPManager {
	mgr := &DHCPManager{
		mode:        mode,
		runningPIDs: make(map[string]int),
		run:         execRunner,
		spawn:       spawnProcess,
		alive:       processAlive,
	}

	if mode != "stub" {
		// Both iptables and eBPF use real DHCP
		baseDir := paths.DHCPDir()
		_ = os.MkdirAll(baseDir, 0755)
		_ = os.MkdirAll(filepath.Join(baseDir, "leases"), 0755)
		_ = os.MkdirAll(filepath.Join(baseDir, "configs"), 0755)

		mgr.leasePath = filepath.Join(baseDir, "leases")
		mgr.configPath = filepath.Join(baseDir, "configs")
	}

	return mgr
}
