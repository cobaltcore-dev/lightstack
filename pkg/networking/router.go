package networking

// RouterManager converges routers on the network node (router_namespace.go).
type RouterManager struct {
	mode string // "stub" or "real"
	// run executes the commands (SPEC-033 research R6). It is a field so the real-mode command
	// sequence is testable without root; NewRouterManager sets execRunner.
	run commandRunner
	// enableForwarding turns on the host's IPv4 forwarding, which a router's gateway needs. A field
	// for the same reason as run; NewRouterManager sets enableIPv4Forwarding.
	enableForwarding func() error
}

// NewRouterManager creates a new router manager
func NewRouterManager(mode string) *RouterManager {
	return &RouterManager{
		mode:             mode,
		run:              execRunner,
		enableForwarding: enableIPv4Forwarding,
	}
}
