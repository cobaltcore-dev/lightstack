package hypervisor

import (
	"strings"
	"testing"
)

func TestGenerateVMXML(t *testing.T) {
	spec := VMSpec{
		Name:      "test-vm",
		UUID:      "550e8400-e29b-41d4-a716-446655440000",
		MemoryMB:  2048,
		VCPUs:     2,
		ImagePath: "/var/lib/libvirt/images/test.qcow2",
		Networks: []NetworkConfig{
			{
				BridgeName: "br-test",
				MACAddress: "52:54:00:12:34:56",
			},
		},
	}

	xml := GenerateVMXML(spec)

	if xml == "" {
		t.Fatal("Generated XML is empty")
	}

	// Check that key elements are present
	requiredElements := []string{
		"<name>test-vm</name>",
		"<uuid>550e8400-e29b-41d4-a716-446655440000</uuid>",
		"<memory unit='MiB'>2048</memory>",
		"<vcpu placement='static'>2</vcpu>",
		"<disk type='file' device='disk'>",
		"<source file='/var/lib/libvirt/images/test.qcow2'/>",
		"<interface type='bridge'>",
		"<source bridge='br-test'/>",
		"<mac address='52:54:00:12:34:56'/>",
	}

	for _, element := range requiredElements {
		if !strings.Contains(xml, element) {
			t.Errorf("XML missing required element: %s", element)
		}
	}
}

func TestGenerateVMXMLMultipleNetworks(t *testing.T) {
	spec := VMSpec{
		Name:      "multi-net-vm",
		UUID:      "550e8400-e29b-41d4-a716-446655440001",
		MemoryMB:  1024,
		VCPUs:     1,
		ImagePath: "/var/lib/libvirt/images/multi.qcow2",
		Networks: []NetworkConfig{
			{
				BridgeName: "br-net1",
				MACAddress: "52:54:00:12:34:56",
			},
			{
				BridgeName: "br-net2",
				MACAddress: "52:54:00:12:34:57",
			},
		},
	}

	xml := GenerateVMXML(spec)

	// Check both networks are present
	if !strings.Contains(xml, "br-net1") {
		t.Error("XML missing first network")
	}

	if !strings.Contains(xml, "br-net2") {
		t.Error("XML missing second network")
	}
}

func TestGenerateVMXMLWithVolumes(t *testing.T) {
	spec := VMSpec{
		Name:      "vm-with-volumes",
		UUID:      "550e8400-e29b-41d4-a716-446655440002",
		MemoryMB:  1024,
		VCPUs:     1,
		ImagePath: "/var/lib/libvirt/images/root.qcow2",
		Volumes: []VolumeConfig{
			{
				RBDPool:  "volumes",
				RBDImage: "volume-123",
				Device:   "vdb",
			},
		},
		Networks: []NetworkConfig{
			{
				BridgeName: "br-test",
				MACAddress: "52:54:00:12:34:56",
			},
		},
	}

	xml := GenerateVMXML(spec)

	// Check RBD volume is present
	requiredElements := []string{
		"<disk type='network' device='disk'>",
		"<source protocol='rbd' name='volumes/volume-123'>",
		"<target dev='vdb' bus='virtio'/>",
	}

	for _, element := range requiredElements {
		if !strings.Contains(xml, element) {
			t.Errorf("XML missing required volume element: %s", element)
		}
	}
}

func TestGenerateVMXMLWithRBDImage(t *testing.T) {
	spec := VMSpec{
		Name:      "rbd-vm",
		UUID:      "550e8400-e29b-41d4-a716-446655440003",
		MemoryMB:  1024,
		VCPUs:     1,
		ImagePath: "rbd:images/cirros",
		Networks: []NetworkConfig{
			{
				BridgeName: "br-test",
				MACAddress: "52:54:00:12:34:56",
			},
		},
	}

	xml := GenerateVMXML(spec)

	// Check RBD boot disk is present
	if !strings.Contains(xml, "<source protocol='rbd' name='images/cirros'>") {
		t.Error("XML missing RBD boot disk")
	}
}

func TestDefaultCloudInitConfig(t *testing.T) {
	hostname := "test-vm"
	sshKey := "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQtest"

	config := DefaultCloudInitConfig(hostname, sshKey)

	if config == nil {
		t.Fatal("Expected non-nil cloud-init config")
	}

	if !strings.Contains(config.MetaData, hostname) {
		t.Error("MetaData missing hostname")
	}

	if !strings.Contains(config.UserData, sshKey) {
		t.Error("UserData missing SSH key")
	}

	if !strings.Contains(config.UserData, "#cloud-config") {
		t.Error("UserData missing cloud-config header")
	}
}

// SPEC-021 FR-017 / FR-018a: the guest console must be reachable only through
// the token-validating proxy, and a second viewer must not evict the first.
// Both are one attribute each, and both fail silently if dropped — a wildcard
// listen just quietly works, and allow-exclusive just quietly disconnects
// someone. Pin them.
func TestDomainXMLConsoleIsUnixSocketAndShared(t *testing.T) {
	spec := VMSpec{
		UUID:     "11111111-2222-3333-4444-555555555555",
		Name:     "console-xml-test",
		MemoryMB: 512,
		VCPUs:    1,
	}
	xml := GenerateVMXML(spec)

	start := strings.Index(xml, "<graphics")
	end := strings.Index(xml, "</graphics>")
	if start < 0 || end < start {
		t.Fatalf("no graphics element; got:\n%s", xml)
	}
	graphics := xml[start:end]

	// A per-start UNIX socket, and no TCP listener at all: nothing on the network can
	// reach the guest, and no port exists for a later guest to inherit.
	if !strings.Contains(graphics, "<listen type='socket'/>") {
		t.Errorf("graphics must listen on a libvirt-generated unix socket; got:\n%s", graphics)
	}
	for _, tcp := range []string{"port=", "autoport", "listen='", "type='address'", "0.0.0.0", "127.0.0.1"} {
		if strings.Contains(graphics, tcp) {
			t.Errorf("graphics must not configure a TCP listener (found %q); got:\n%s", tcp, graphics)
		}
	}
	// No fixed path either: libvirt puts the socket in the domain's per-start private
	// directory, so the path changes on every start and a stale token names nothing.
	if strings.Contains(graphics, "socket='") {
		t.Errorf("graphics must leave the socket path to libvirt; got:\n%s", graphics)
	}
	if !strings.Contains(graphics, "sharePolicy='force-shared'") {
		t.Errorf("graphics must set sharePolicy='force-shared' so a second viewer does not evict the first; got:\n%s", graphics)
	}
}
