// Package paths centralizes o3k's node-local hostPath layout (/var/lib/o3k/*)
// so the storage, hypervisor and networking packages agree on one set of
// directories. A drift between these (the console.log path was hard-coded in two
// places) previously broke boot-from-volume — keep every /var/lib/o3k literal
// here (SPEC-009 WS-0/WS-2/WS-5).
package paths

import "path/filepath"

// Base is the node-local root for o3k's per-node state (the chart's persistent
// hostPath).
const Base = "/var/lib/o3k"

// ImagesDir is the raw image-base store (Glance local backend).
func ImagesDir() string { return filepath.Join(Base, "images") }

// VolumesDir is the local volume-backing store (Cinder local backend).
func VolumesDir() string { return filepath.Join(Base, "volumes") }

// InstancesDir holds per-instance artifacts (the qcow2 overlay + console.log).
func InstancesDir() string { return filepath.Join(Base, "instances") }

// CloudInitDir holds per-instance config-drive ISOs.
func CloudInitDir() string { return filepath.Join(Base, "cloud-init") }

// DHCPDir holds dnsmasq runtime state (configs, leases, pids).
func DHCPDir() string { return filepath.Join(Base, "dhcp") }

// ConsoleTokenDir holds the console-token projection the noVNC proxy reads: one
// file per live token, "<token>: <host>:<port>". It is a projection of the
// console_auth_tokens table, never the store of record — losing it stops tokens
// from resolving, which is the safe direction (SPEC-021 FR-023b).
func ConsoleTokenDir() string { return filepath.Join(Base, "console-tokens") }
