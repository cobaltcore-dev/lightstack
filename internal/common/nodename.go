package common

import "os"

// NodeName is the name this node reserves block-storage rows under while it creates them,
// so the startup sweep touches only its own (FR-033): LIGHTSTACK_NODE_NAME when set, the
// host name otherwise. It is independent of compute.node_id, whose "auto" value changes
// with every start.
func NodeName() (string, error) {
	if name := os.Getenv("LIGHTSTACK_NODE_NAME"); name != "" {
		return name, nil
	}
	return os.Hostname()
}
