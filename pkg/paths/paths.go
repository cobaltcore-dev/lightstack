// Package paths centralizes Lightstack's node-local hostPath layout (/var/lib/lightstack/*)
// so the storage, hypervisor and networking packages agree on one set of
// directories. A drift between these (the console.log path was hard-coded in two
// places) previously broke boot-from-volume — keep every /var/lib/lightstack literal
// here (SPEC-009 WS-0/WS-2/WS-5).
package paths

import "path/filepath"

// DefaultBase is the node-local root used when state_path is not configured.
const DefaultBase = "/var/lib/lightstack"

// base is the node-local root for Lightstack's per-node state (the chart's persistent
// hostPath). Set once at startup, before any service is built, and only read after.
var base = DefaultBase

// SetBase sets the node-local root from the configured state_path. It must be the same
// path the host sees: libvirtd opens the paths written into the domain XML on the host,
// so the chart mounts realMode.hostStoragePath at that same path inside the pod.
func SetBase(dir string) { base = dir }

// Base is the node-local root.
func Base() string { return base }

// ImagesDir is the raw image-base store (Glance local backend).
func ImagesDir() string { return filepath.Join(base, "images") }

// VolumesDir is the local volume-backing store (Cinder local backend).
func VolumesDir() string { return filepath.Join(base, "volumes") }

// InstancesDir holds per-instance artifacts (the qcow2 overlay + console.log).
func InstancesDir() string { return filepath.Join(base, "instances") }

// CloudInitDir holds per-instance config-drive ISOs.
func CloudInitDir() string { return filepath.Join(base, "cloud-init") }

// DHCPDir holds dnsmasq runtime state (configs, leases, pids).
func DHCPDir() string { return filepath.Join(base, "dhcp") }

// ConsoleTokenDir holds the console-token projection the noVNC proxy reads: one
// file per live token, "<token>: <host>:<port>". It is a projection of the
// console_auth_tokens table, never the store of record — losing it stops tokens
// from resolving, which is the safe direction (SPEC-021 FR-023b).
func ConsoleTokenDir() string { return filepath.Join(base, "console-tokens") }
