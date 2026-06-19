package networking

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"text/template"
)

// DHCPManager manages DHCP servers (dnsmasq)
type DHCPManager struct {
	mode        string // "stub", "iptables", or "ebpf"
	leasePath   string
	configPath  string
	pidPath     string
	mu          sync.Mutex
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
		baseDir := "/var/lib/o3k/dhcp"
		_ = os.MkdirAll(baseDir, 0755)
		_ = os.MkdirAll(filepath.Join(baseDir, "leases"), 0755)
		_ = os.MkdirAll(filepath.Join(baseDir, "configs"), 0755)
		_ = os.MkdirAll(filepath.Join(baseDir, "pids"), 0755)

		mgr.leasePath = filepath.Join(baseDir, "leases")
		mgr.configPath = filepath.Join(baseDir, "configs")
		mgr.pidPath = filepath.Join(baseDir, "pids")
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
	PIDFile        string
	DHCPRangeStart string
	DHCPRangeEnd   string
	LeaseTime      string
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
func (m *DHCPManager) StartDHCP(config DHCPConfig, nsName string) error {
	if m.mode == "stub" {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.stubDHCP[config.NetworkID] = true
		return nil
	}

	leaseFile := filepath.Join(m.leasePath, config.NetworkID+".leases")
	pidFile := filepath.Join(m.pidPath, config.NetworkID+".pid")
	configFile := filepath.Join(m.configPath, config.NetworkID+".conf")

	config.LeaseFile = leaseFile
	config.PIDFile = pidFile

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

	// Start dnsmasq in namespace
	var cmd *exec.Cmd
	if nsName != "" {
		fullArgs := append([]string{"netns", "exec", nsName, "dnsmasq"}, args...)
		cmd = exec.Command("ip", fullArgs...)
	} else {
		cmd = exec.Command("dnsmasq", args...)
	}

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

// StopDHCP stops a DHCP server
func (m *DHCPManager) StopDHCP(networkID string) error {
	if m.mode == "stub" {
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.stubDHCP, networkID)
		return nil
	}

	pidFile := filepath.Join(m.pidPath, networkID+".pid")

	// Read PID from file
	pidData, err := os.ReadFile(pidFile)
	if err != nil {
		return nil // Already stopped
	}

	var pid int
	_, _ = fmt.Sscanf(string(pidData), "%d", &pid)

	// Kill process
	if pid > 0 {
		proc, err := os.FindProcess(pid)
		if err == nil {
			_ = proc.Kill()
		}
	}

	// Clean up files
	_ = os.Remove(pidFile)
	_ = os.Remove(filepath.Join(m.leasePath, networkID+".leases"))
	_ = os.Remove(filepath.Join(m.configPath, networkID+".conf"))

	m.mu.Lock()
	delete(m.runningPIDs, networkID)
	m.mu.Unlock()
	return nil
}

// writeDNSMasqConfig writes dnsmasq configuration file
func (m *DHCPManager) writeDNSMasqConfig(configFile string, config DHCPConfig) error {
	tmpl := `# dnsmasq configuration for network {{ .NetworkID }}
interface={{ .BridgeName }}
bind-interfaces
port=0
dhcp-range={{ .DHCPRangeStart }},{{ .DHCPRangeEnd }},{{ .LeaseTime }}
dhcp-leasefile={{ .LeaseFile }}
pid-file={{ .PIDFile }}
{{ if .GatewayIP }}dhcp-option=3,{{ .GatewayIP }}{{ end }}
{{ range .DNSServers }}dhcp-option=6,{{ . }}
{{ end }}
{{ range .HostReservations }}dhcp-host={{ .MAC }},{{ .IP }}
{{ end }}
`

	t, err := template.New("dnsmasq").Parse(tmpl)
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
