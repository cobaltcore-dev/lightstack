package hypervisor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/cobaltcore-dev/o3k/pkg/paths"
	"github.com/rs/zerolog/log"
)

// VMSpec defines VM specifications
type VMSpec struct {
	UUID      string
	Name      string
	VCPUs     int
	MemoryMB  int
	DiskGB    int
	ImagePath string // Path to image (RBD or local)
	Networks  []NetworkConfig
	Volumes   []VolumeConfig
	CloudInit *CloudInitConfig
}

// NetworkConfig defines network interface configuration
type NetworkConfig struct {
	PortID     string
	MACAddress string
	BridgeName string
}

// VolumeConfig defines volume attachment configuration
type VolumeConfig struct {
	VolumeID string
	RBDPool  string
	RBDImage string
	Device   string // vda, vdb, etc.
}

// CloudInitConfig defines cloud-init configuration
type CloudInitConfig struct {
	MetaData string
	UserData string
}

// GenerateVMXML generates libvirt XML for a VM
func GenerateVMXML(spec VMSpec) string {
	var sb strings.Builder

	// Domain header
	fmt.Fprintf(&sb, `<domain type='kvm'>
  <name>%s</name>
  <uuid>%s</uuid>
  <memory unit='MiB'>%d</memory>
  <currentMemory unit='MiB'>%d</currentMemory>
  <vcpu placement='static'>%d</vcpu>

  <os>
    <type arch='x86_64' machine='pc-i440fx-2.12'>hvm</type>
    <boot dev='hd'/>
  </os>

  <features>
    <acpi/>
    <apic/>
    <pae/>
  </features>

  <cpu mode='host-model'>
    <model fallback='allow'/>
  </cpu>

  <clock offset='utc'>
    <timer name='rtc' tickpolicy='catchup'/>
    <timer name='pit' tickpolicy='delay'/>
    <timer name='hpet' present='no'/>
  </clock>

  <on_poweroff>destroy</on_poweroff>
  <on_reboot>restart</on_reboot>
  <on_crash>restart</on_crash>

  <devices>
    <emulator>/usr/bin/qemu-system-x86_64</emulator>
`,
		spec.Name, spec.UUID, spec.MemoryMB, spec.MemoryMB, spec.VCPUs)

	// Boot disk (RBD-backed or local)
	if strings.HasPrefix(spec.ImagePath, "rbd:") {
		// RBD image format: rbd:pool/image
		parts := strings.SplitN(strings.TrimPrefix(spec.ImagePath, "rbd:"), "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			log.Error().Str("image_path", spec.ImagePath).Msg("invalid RBD image path: expected rbd:pool/image")
			return ""
		}
		pool := parts[0]
		image := parts[1]

		fmt.Fprintf(&sb, `
    <disk type='network' device='disk'>
      <driver name='qemu' type='qcow2' cache='writeback'/>
      <source protocol='rbd' name='%s/%s'>
        <host name='127.0.0.1' port='6789'/>
      </source>
      <target dev='vda' bus='virtio'/>
    </disk>
`, pool, image)
	} else {
		// Local file
		fmt.Fprintf(&sb, `
    <disk type='file' device='disk'>
      <driver name='qemu' type='qcow2' cache='writeback'/>
      <source file='%s'/>
      <target dev='vda' bus='virtio'/>
    </disk>
`, spec.ImagePath)
	}

	// Attached volumes
	for i, vol := range spec.Volumes {
		device := vol.Device
		if device == "" {
			device = fmt.Sprintf("vd%c", 'b'+i) // vdb, vdc, etc.
		}

		fmt.Fprintf(&sb, `
    <disk type='network' device='disk'>
      <driver name='qemu' type='raw'/>
      <source protocol='rbd' name='%s/%s'>
        <host name='127.0.0.1' port='6789'/>
      </source>
      <target dev='%s' bus='virtio'/>
    </disk>
`, vol.RBDPool, vol.RBDImage, device)
	}

	// Network interfaces
	for _, net := range spec.Networks {
		fmt.Fprintf(&sb, `
    <interface type='bridge'>
      <mac address='%s'/>
      <source bridge='%s'/>
      <model type='virtio'/>
    </interface>
`, net.MACAddress, net.BridgeName)
	}

	// Serial console — capture output to a per-instance log file so
	// os-getConsoleOutput / `openstack console log show` returns real boot
	// output (SPEC-009 WS-0).
	fmt.Fprintf(&sb, `
    <serial type='pty'>
      <log file='%s' append='off'/>
      <target port='0'/>
    </serial>
    <console type='pty'>
      <target type='serial' port='0'/>
    </console>
`, ConsoleLogPath(spec.UUID))

	// VNC graphics (SPEC-021).
	//
	// listen='127.0.0.1' is load-bearing, not tidiness: the console proxy is the
	// only thing that should be able to reach a guest's framebuffer, and it does
	// so by sharing the host network namespace. Listening on 0.0.0.0 — as this
	// did before F1 was fixed — makes the console-token check bypassable by
	// anything that can reach the node.
	//
	// sharePolicy='force-shared' lets several viewers watch one guest at once.
	// libvirt's default (allow-exclusive) silently disconnects the previous
	// viewer when a second one connects, which reads as a bug during exactly the
	// support scenario a console exists for.
	sb.WriteString(`
    <graphics type='vnc' port='-1' autoport='yes' listen='127.0.0.1' sharePolicy='force-shared'>
      <listen type='address' address='127.0.0.1'/>
    </graphics>
`)

	// Cloud-init (if provided)
	if spec.CloudInit != nil {
		fmt.Fprintf(&sb, `
    <disk type='file' device='cdrom'>
      <driver name='qemu' type='raw'/>
      <source file='%s'/>
      <target dev='hdc' bus='ide'/>
      <readonly/>
    </disk>
`, CloudInitISOPath(spec.UUID))
	}

	// Close devices and domain
	sb.WriteString(`  </devices>
</domain>
`)

	return sb.String()
}

// GenerateCloudInitISO generates cloud-init ISO content
func GenerateCloudInitISO(uuid string, config *CloudInitConfig) (string, error) {
	if config == nil {
		return "", nil // No cloud-init requested
	}

	isoDir := paths.CloudInitDir()
	isoPath := CloudInitISOPath(uuid)

	// Create directory if it doesn't exist
	if err := os.MkdirAll(isoDir, 0755); err != nil {
		return "", fmt.Errorf("failed to create cloud-init directory: %w", err)
	}

	// Create temporary directory for cloud-init files
	tmpDir, err := os.MkdirTemp("", "cloud-init-"+uuid)
	if err != nil {
		return "", fmt.Errorf("failed to create temp directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	// Write meta-data file
	metaDataPath := filepath.Join(tmpDir, "meta-data")
	if err := os.WriteFile(metaDataPath, []byte(config.MetaData), 0644); err != nil {
		return "", fmt.Errorf("failed to write meta-data: %w", err)
	}

	// Write user-data file
	userDataPath := filepath.Join(tmpDir, "user-data")
	if err := os.WriteFile(userDataPath, []byte(config.UserData), 0644); err != nil {
		return "", fmt.Errorf("failed to write user-data: %w", err)
	}

	// Generate ISO using genisoimage (or mkisofs as fallback)
	// Try genisoimage first (Debian/Ubuntu)
	cmd := exec.Command("genisoimage",
		"-output", isoPath,
		"-volid", "cidata",
		"-joliet",
		"-rock",
		metaDataPath,
		userDataPath,
	)

	_, err = cmd.CombinedOutput()
	if err != nil {
		// Try mkisofs as fallback (older systems)
		cmd = exec.Command("mkisofs",
			"-output", isoPath,
			"-volid", "cidata",
			"-joliet",
			"-rock",
			metaDataPath,
			userDataPath,
		)
		output, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("failed to create ISO (genisoimage/mkisofs not available): %w, output: %s", err, output)
		}
	}

	return isoPath, nil
}

// NewCloudInitConfig builds a config-drive payload from an SSH key and/or the
// instance's user_data (SPEC-009 WS-4 / T028). With no user_data it preserves
// the proven default (#cloud-config carrying the key). With user_data it uses
// that verbatim as the user-data and carries the SSH key via the OpenStack-
// standard meta-data public-keys, so a script-style user_data does not drop the
// key. Either input may be empty.
func NewCloudInitConfig(hostname, sshKey, userData string) *CloudInitConfig {
	if userData == "" {
		return DefaultCloudInitConfig(hostname, sshKey)
	}
	var meta strings.Builder
	fmt.Fprintf(&meta, "instance-id: %s\nlocal-hostname: %s\n", hostname, hostname)
	if sshKey != "" {
		fmt.Fprintf(&meta, "public-keys:\n  0: |\n    %s\n", sshKey)
	}
	return &CloudInitConfig{MetaData: meta.String(), UserData: userData}
}

// DefaultCloudInitConfig returns default cloud-init configuration
func DefaultCloudInitConfig(hostname, sshKey string) *CloudInitConfig {
	metaData := fmt.Sprintf(`instance-id: %s
local-hostname: %s
`, hostname, hostname)

	userData := `#cloud-config
packages:
  - curl
  - vim
runcmd:
  - echo "O3K VM booted successfully" > /var/log/o3k.log
`

	if sshKey != "" {
		userData += fmt.Sprintf(`
ssh_authorized_keys:
  - %s
`, sshKey)
	}

	return &CloudInitConfig{
		MetaData: metaData,
		UserData: userData,
	}
}

// DiskSpec defines disk device configuration
type DiskSpec struct {
	Device   string // e.g., /dev/vdb, /dev/vdc
	Type     string // "network" for RBD, "file" for local
	Source   string // RBD: "pool/image", File: "/path/to/file.qcow2"
	Protocol string // "rbd" for Ceph RBD
}

// GenerateDiskXML generates libvirt XML for a disk device
func GenerateDiskXML(spec DiskSpec) string {
	var sb strings.Builder

	// Extract device letter from path (e.g., "/dev/vdb" -> "vdb")
	device := spec.Device
	device = strings.TrimPrefix(device, "/dev/")

	if spec.Type == "network" && spec.Protocol == "rbd" {
		// RBD network disk
		fmt.Fprintf(&sb, `<disk type='network' device='disk'>
  <driver name='qemu' type='raw' cache='writeback'/>
  <source protocol='rbd' name='%s'>
    <host name='127.0.0.1' port='6789'/>
  </source>
  <target dev='%s' bus='virtio'/>
</disk>`, spec.Source, device)
	} else {
		// Local file disk
		fmt.Fprintf(&sb, `<disk type='file' device='disk'>
  <driver name='qemu' type='qcow2' cache='writeback'/>
  <source file='%s'/>
  <target dev='%s' bus='virtio'/>
</disk>`, spec.Source, device)
	}

	return sb.String()
}
