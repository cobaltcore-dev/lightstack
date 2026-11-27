package networking

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// heldNamespace is a namespace of Lightstack's — a router's or a network's DHCP — as the node holds it:
// whether it exists, its Lightstack devices' addresses, its devices' state and the host's, where the
// outside ends of its legs are. What a pass cut short leaves undone — lo down, a leg off its bridge or
// down — shows only in the states, so they are read on every pass and written where they differ
// (SPEC-033 T191).
type heldNamespace struct {
	exists    bool
	addresses map[string][]string  // per Lightstack device in the namespace: its addresses with prefix
	links     map[string]linkState // the namespace's devices, lo included
	hostLinks map[string]linkState // the host's devices
}

// readNamespace reads the namespace ns as the node holds it.
func readNamespace(ctx context.Context, run commandRunner, ns string) (heldNamespace, error) {
	held := heldNamespace{addresses: map[string][]string{}}
	out, err := runCommand(ctx, run, "", "ip", "netns", "list")
	if err != nil {
		return held, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if name, _, _ := strings.Cut(strings.TrimSpace(line), " "); name == ns {
			held.exists = true
		}
	}
	if !held.exists {
		return held, nil
	}
	if out, err = runCommand(ctx, run, "", "ip", "-n", ns, "-j", "addr", "show"); err != nil {
		return held, err
	}
	if held.addresses, err = parseDeviceAddresses(out); err != nil {
		return held, fmt.Errorf("read the namespace's addresses: %w", err)
	}
	held.links = parseLinks(out)
	if out, err = runCommand(ctx, run, "", "ip", "-j", "link", "show"); err != nil {
		return held, err
	}
	held.hostLinks = parseLinks(out)
	return held, nil
}

// namespaceSteps are the commands that make the namespace ns whole: made when missing, lo up.
func (h heldNamespace) namespaceSteps(ns string) [][]string {
	var steps [][]string
	if !h.exists {
		steps = append(steps, []string{"netns", "add", ns})
	}
	if !h.links["lo"].up {
		steps = append(steps, []string{"-n", ns, "link", "set", "lo", "up"})
	}
	return steps
}

// legSteps are the commands that make a veth leg whole: made when its inside end is missing, its outside
// end on bridge ("" for none) and up, its inside end up, each only where the node differs.
func (h heldNamespace) legSteps(ns, outside, inside, bridge string) [][]string {
	var steps [][]string
	if _, ok := h.addresses[inside]; !ok {
		steps = append(steps, []string{"link", "add", outside, "type", "veth", "peer", "name", inside, "netns", ns})
	}
	host := h.hostLinks[outside]
	if bridge != "" && host.master != bridge {
		steps = append(steps, []string{"link", "set", outside, "master", bridge})
	}
	if !host.up {
		steps = append(steps, []string{"link", "set", outside, "up"})
	}
	if !h.links[inside].up {
		steps = append(steps, []string{"-n", ns, "link", "set", inside, "up"})
	}
	return steps
}

// linkState is a device as `ip -j link show` or `ip -j addr show` prints it: its bridge and whether it is
// up.
type linkState struct {
	master string
	up     bool
}

// parseLinks reads each device's state, by name. Output it cannot read gives none, which reads as
// nothing up: the steps run again, and they are idempotent.
func parseLinks(out []byte) map[string]linkState {
	var devices []struct {
		Name   string   `json:"ifname"`
		Master string   `json:"master"`
		Flags  []string `json:"flags"`
	}
	links := map[string]linkState{}
	if json.Unmarshal(out, &devices) == nil {
		for _, d := range devices {
			links[d.Name] = linkState{master: d.Master, up: slices.Contains(d.Flags, "UP")}
		}
	}
	return links
}
