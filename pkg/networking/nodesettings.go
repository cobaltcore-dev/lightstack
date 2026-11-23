package networking

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// bridgeSettings are the settings that pass bridged traffic through iptables and ip6tables, which the
// ports' chains match with physdev (SPEC-033 FR-046): under /proc/sys, as files.
var bridgeSettings = []string{"net/bridge/bridge-nf-call-iptables", "net/bridge/bridge-nf-call-ip6tables"}

// NodeSettings are the node's settings the port firewall needs (FR-046, research R35): br_netfilter
// loaded, and bridged traffic passed to iptables and ip6tables. Without them a port's chains never see
// its traffic, which then passes unfiltered.
type NodeSettings struct {
	mode string
	// run runs the node's commands, and root is where its /proc lives; tests replace both.
	run  commandRunner
	root string
}

// NewNodeSettings creates the node's settings. Stub mode sets and checks nothing.
func NewNodeSettings(mode string) *NodeSettings {
	return &NodeSettings{mode: mode, run: execRunner, root: "/"}
}

// Ensure loads br_netfilter, sets both settings to 1 and reads them back, at start. A module built into
// the kernel or loaded by the host needs no load, so a failed one counts only while the settings do not
// hold. The error names what does not hold.
func (n *NodeSettings) Ensure(ctx context.Context) error {
	if n.mode == "stub" {
		return nil
	}
	_, loadErr := runCommand(ctx, n.run, "", "modprobe", "br_netfilter")
	for _, s := range bridgeSettings {
		// A setting that cannot be written is read back below, where it is named.
		_ = os.WriteFile(n.path(s), []byte("1\n"), 0o644)
	}
	err := n.Check()
	if err != nil && loadErr != nil {
		return errors.Join(fmt.Errorf("load br_netfilter: %w", loadErr), err)
	}
	return err
}

// Check reads both settings: nil when both are 1, else an error naming each that is not.
func (n *NodeSettings) Check() error {
	if n.mode == "stub" {
		return nil
	}
	var errs []error
	for _, s := range bridgeSettings {
		name := strings.ReplaceAll(s, "/", ".")
		b, err := os.ReadFile(n.path(s))
		switch {
		case err != nil:
			errs = append(errs, fmt.Errorf("%s cannot be read: %w", name, err))
		case strings.TrimSpace(string(b)) != "1":
			errs = append(errs, fmt.Errorf("%s is %s, not 1", name, strings.TrimSpace(string(b))))
		}
	}
	return errors.Join(errs...)
}

func (n *NodeSettings) path(setting string) string {
	return filepath.Join(n.root, "proc", "sys", setting)
}
