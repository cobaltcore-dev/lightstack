package networking

import (
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"text/template"

	"github.com/cobaltcore-dev/lightstack/pkg/paths"
)

// DHCPManager manages DHCP servers (dnsmasq)
type DHCPManager struct {
	mode        string // "stub", "iptables", or "ebpf"
	leasePath   string
	configPath  string
	mu          sync.Mutex
	restartMu   sync.Mutex      // serializes dnsmasq reloads so concurrent port allocs don't interleave kill/start
	runningPIDs map[string]int  // networkID -> PID
	stubDHCP    map[string]bool // For stub mode
}

// NewDHCPManager creates a new DHCP manager
func NewDHCPManager(mode string) *DHCPManager {
	mgr := &DHCPManager{
		mode:        mode,
		runningPIDs: make(map[string]int),
		stubDHCP:    make(map[string]bool),
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

// DHCPConfig represents DHCP configuration
type DHCPConfig struct {
	NetworkID      string
	BridgeName     string
	SubnetCIDR     string
	GatewayIP      string
	DNSServers     []string
	LeaseFile      string
	DHCPRangeStart string
	DHCPRangeEnd   string
	LeaseTime      string
	// MetadataNextHop is the address guests should route 169.254.169.254 through when the subnet has
	// no gateway — the bridge's own on-link address. The metadata route and the default route are
	// separate concerns: a subnet created with `--gateway none` is declining a default route, not
	// declining instance metadata (SPEC-024 / cloud-storage#546 FR-023a). Ignored when GatewayIP is
	// set, which already serves as the next hop for both.
	MetadataNextHop string

	// HostReservations pins MAC->IP so a respawned dnsmasq hands each VM the
	// same IP its port already has (SPEC-009 WS-2: stable IP across restart).
	HostReservations []DHCPHostReservation
}

// DHCPHostReservation pins a MAC address to a fixed IP (dnsmasq dhcp-host).
type DHCPHostReservation struct {
	MAC string
	IP  string
}

// StartDHCP starts a DHCP server for a network
func (m *DHCPManager) StartDHCP(config DHCPConfig) error {
	if m.mode == "stub" {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.stubDHCP[config.NetworkID] = true
		return nil
	}

	leaseFile := filepath.Join(m.leasePath, config.NetworkID+".leases")
	configFile := filepath.Join(m.configPath, config.NetworkID+".conf")

	config.LeaseFile = leaseFile

	// Create dnsmasq config file
	if err := m.writeDNSMasqConfig(configFile, config); err != nil {
		return err
	}

	// Build dnsmasq command — load config file and run in foreground for process tracking
	args := []string{
		"--conf-file=" + configFile,
		"--no-daemon",
		"--log-facility=-", // Log to stderr
	}

	// Start dnsmasq in the namespace Lightstack itself runs in, alongside the bridge it serves. No namespace
	// parameter: the bridge cannot be anywhere else (see BridgeManager.CreateBridge), so neither can
	// this (SPEC-024 / cloud-storage#546 F10).
	cmd := exec.Command("dnsmasq", args...)

	// Surface dnsmasq's own log/errors (it logs to stderr via --log-facility=-)
	// so a failed launch is not silent.
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	// Start in background
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start dnsmasq for network %s: %w", config.NetworkID, err)
	}

	m.mu.Lock()
	m.runningPIDs[config.NetworkID] = cmd.Process.Pid
	m.mu.Unlock()

	// Reap the process when it exits (killed/crashed) so it doesn't become a
	// zombie; the reconciler restarts it on the next pass if it died (WS-2).
	go func() { _ = cmd.Wait() }()
	return nil
}

// killProcess terminates EVERY dnsmasq bound to a network's config and clears its
// tracked PID, leaving the lease file intact so a later StartDHCP preserves
// existing leases. Killing all of them stops orphans from a previous Lightstack
// accumulating into multiple dnsmasq on one bridge (SPEC-009 WS-2).
//
// Matching on the config path in the cmdline is the whole mechanism, and is the
// stronger one: it reaches orphans no tracked PID names. There is deliberately no
// pid-file fallback — dnsmasq runs --no-daemon, which suppresses writing one, so a
// fallback that read it could never fire (branch review 2026-08-28, SRC-4).
func (m *DHCPManager) killProcess(networkID string) {
	_ = exec.Command("pkill", "-f", filepath.Join(m.configPath, networkID+".conf")).Run()
	m.mu.Lock()
	delete(m.runningPIDs, networkID)
	m.mu.Unlock()
}

// StopDHCP stops a DHCP server and removes its lease/config (a full teardown).
func (m *DHCPManager) StopDHCP(networkID string) error {
	if m.mode == "stub" {
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.stubDHCP, networkID)
		return nil
	}
	m.killProcess(networkID)
	_ = os.Remove(filepath.Join(m.leasePath, networkID+".leases"))
	_ = os.Remove(filepath.Join(m.configPath, networkID+".conf"))
	return nil
}

// RestartDHCP reloads dnsmasq for a network with a fresh config WITHOUT dropping
// existing leases: it kills the running process (keeping the lease file) and
// starts a new one. The whole kill+start is serialized so concurrent port
// allocations on the same network can't interleave and leave dnsmasq dead
// (SPEC-009 WS-2).
func (m *DHCPManager) RestartDHCP(config DHCPConfig) error {
	if m.mode == "stub" {
		return m.StartDHCP(config)
	}
	m.restartMu.Lock()
	defer m.restartMu.Unlock()
	m.killProcess(config.NetworkID) // keep the lease file so leases survive the reload
	return m.StartDHCP(config)
}

// EnsureDHCP guarantees exactly one dnsmasq serves the network without
// double-starting. If one is already tracked as running it is a no-op; otherwise
// it first kills any dnsmasq still bound to this network's config file — an orphan
// left by a previous Lightstack instance, since config files live on the hostPath and
// survive a control-plane restart — then starts a fresh one. Serialized against RestartDHCP
// (and itself) via restartMu so a periodic reconcile and a concurrent
// port-allocation reload can't each spawn a dnsmasq on the same bridge
// (SPEC-009 WS-2).
func (m *DHCPManager) EnsureDHCP(config DHCPConfig) error {
	if m.mode == "stub" {
		return m.StartDHCP(config)
	}
	m.restartMu.Lock()
	defer m.restartMu.Unlock()
	if m.IsRunning(config.NetworkID) {
		return nil // already serving and tracked — idempotent
	}
	m.killProcess(config.NetworkID) // reap a stale orphan from a previous Lightstack before starting
	return m.StartDHCP(config)
}

// ServedNetworks returns the networks this manager runs a DHCP server for, so the reconciler can stop
// the ones whose network was deleted (SPEC-009 WS-2). It lists before it reads the networks, and
// stops what that read no longer has; see NetworkReconciler.pruneDeletedNetworks for why that order.
func (m *DHCPManager) ServedNetworks() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mode == "stub" {
		return slices.Collect(maps.Keys(m.stubDHCP))
	}
	return slices.Collect(maps.Keys(m.runningPIDs))
}

// writeDNSMasqConfig writes dnsmasq configuration file
func (m *DHCPManager) writeDNSMasqConfig(configFile string, config DHCPConfig) error {
	tmpl := `# dnsmasq configuration for network {{ .NetworkID }}
interface={{ .BridgeName }}
bind-interfaces
port=0
dhcp-range={{ .DHCPRangeStart }},{{ .DHCPRangeEnd }},{{ .LeaseTime }}
dhcp-leasefile={{ .LeaseFile }}
{{ if .GatewayIP }}dhcp-option=3,{{ .GatewayIP }}
dhcp-option=121,{{ metadataIP }}/32,{{ .GatewayIP }},0.0.0.0/0,{{ .GatewayIP }}
{{ else if .MetadataNextHop }}dhcp-option=121,{{ metadataIP }}/32,{{ .MetadataNextHop }}{{ end }}
{{ range .DNSServers }}dhcp-option=6,{{ . }}
{{ end }}
{{ range .HostReservations }}dhcp-host={{ .MAC }},{{ .IP }}
{{ end }}
`

	// metadataIP comes from the constant rather than a literal because this template and the iptables
	// DNAT in metadata_redirect.go are two halves of one path: the rule intercepts the address, this
	// tells guests to route to it. Spelled separately they can diverge with nothing failing to compile
	// and nothing logged — guests would simply route somewhere no rule catches, which is the silent
	// breakage the reconciler's destination comparison exists to find (SPEC-024 R10c).
	t, err := template.New("dnsmasq").Funcs(template.FuncMap{
		"metadataIP": func() string { return MetadataIP },
	}).Parse(tmpl)
	if err != nil {
		return err
	}

	f, err := os.Create(configFile)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	return t.Execute(f, config)
}

// IsRunning checks if DHCP server is running for a network
func (m *DHCPManager) IsRunning(networkID string) bool {
	// Stub mode keeps its own bookkeeping and spawns nothing, so runningPIDs is always empty there.
	// Answering from it made this report "not serving" for a network StartDHCP had just recorded as
	// served — a lie no caller happened to read, because both call sites sit on non-stub paths.
	if m.mode == "stub" {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.stubDHCP[networkID]
	}

	m.mu.Lock()
	pid, exists := m.runningPIDs[networkID]
	m.mu.Unlock()
	if !exists {
		return false
	}

	// Check if process is still running using signal 0
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}

	return proc.Signal(syscall.Signal(0)) == nil
}
