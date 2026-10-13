package common

import (
	"os"
	"testing"
)

// The node name block-storage rows are reserved under: LIGHTSTACK_NODE_NAME when set,
// the host name otherwise (FR-033).
func TestNodeName(t *testing.T) {
	t.Setenv("LIGHTSTACK_NODE_NAME", "node-a")
	if got, err := NodeName(); err != nil || got != "node-a" {
		t.Errorf("NodeName() = %q, %v; want node-a", got, err)
	}
	t.Setenv("LIGHTSTACK_NODE_NAME", "")
	host, err := os.Hostname()
	if err != nil {
		t.Skip("no host name on this machine")
	}
	if got, err := NodeName(); err != nil || got != host {
		t.Errorf("NodeName() = %q, %v; want the host name %q", got, err, host)
	}
}
