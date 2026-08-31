package hypervisor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	libvirt "github.com/digitalocean/go-libvirt"
	"github.com/digitalocean/go-libvirt/socket"
	"github.com/digitalocean/go-libvirt/socket/dialers"
	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
)

// ErrDomainNotFound reports that the hypervisor has no guest with the given id.
//
// The caller needs this distinguished from every other failure: an absent guest is a
// STATE problem the client may be able to resolve, while a connection fault is an
// outage it cannot. Collapsing both into one error is what made every libvirt failure
// a flat 500 and left cloud-storage#547 B12 unreadable from outside — the API said
// "server error" whether the guest had vanished or libvirt was simply unreachable.
//
// Compare with errors.Is; both the stub and real paths wrap it.
var ErrDomainNotFound = errors.New("hypervisor: domain not found")

// VMState represents the state of a virtual machine
type VMState string

const (
	VMStateNoState     VMState = "NOSTATE"
	VMStateRunning     VMState = "RUNNING"
	VMStateBlocked     VMState = "BLOCKED"
	VMStatePaused      VMState = "PAUSED"
	VMStateShutdown    VMState = "SHUTDOWN"
	VMStateShutoff     VMState = "SHUTOFF"
	VMStateCrashed     VMState = "CRASHED"
	VMStatePMSuspended VMState = "PMSUSPENDED"
)

// VMManager manages virtual machine operations
type VMManager struct {
	libvirtURI string
	mode       string // "stub" or "real"
	// The current libvirt connection: replaced, never reopened in place, when it
	// drops (see keepConnected).
	conn    atomic.Pointer[libvirt.Libvirt]
	mu      sync.Mutex
	stubVMs map[string]*stubVM // For stub mode

	// The reconnect loop (real mode only): closing done stops it, and stopped is
	// closed once it has returned.
	done      chan struct{}
	stopped   chan struct{}
	closeOnce sync.Once

	// consoleDir is whether the console socket directory is QEMU's yet (SPEC-021
	// FR-017a); nil until PrepareConsoleSocketDir has run. Every real start needs it.
	consoleDir atomic.Pointer[consoleDirState]
}

// stubVM represents a simulated VM in stub mode
type stubVM struct {
	uuid       string
	xml        string
	state      VMState
	powerState int
	createdAt  time.Time
}

// NewVMManager creates a new VM manager
func NewVMManager(libvirtURI, mode string) (*VMManager, error) {
	mgr := &VMManager{
		libvirtURI: libvirtURI,
		mode:       mode,
		stubVMs:    make(map[string]*stubVM),
	}

	// If real mode, connect to libvirt
	if mode == "real" {
		if err := mgr.connectLibvirt(); err != nil {
			return nil, fmt.Errorf("failed to connect to libvirt: %w", err)
		}
	}

	return mgr, nil
}

// connectLibvirt establishes a connection to libvirt
func (m *VMManager) connectLibvirt() error {
	socketPath := "/var/run/libvirt/libvirt-sock"
	// Allow override via configured URI: strip scheme to get socket path
	if m.libvirtURI != "" && m.libvirtURI != "qemu:///system" {
		// For custom URIs (e.g. qemu+unix:///path), use the path component
		// For remote URIs use TCP — not supported in this implementation
		if len(m.libvirtURI) > 0 {
			socketPath = m.libvirtURI
		}
	}

	return m.connect(dialers.NewLocal(
		dialers.WithSocket(socketPath),
		dialers.WithLocalTimeout(2*time.Second),
	), reconnectBackoffMin, reconnectBackoffMax)
}

const (
	// Reconnect backoff after a lost libvirt connection. The ceiling bounds how long
	// libvirt calls keep failing once libvirtd is back; neither value delays a caller,
	// since callers never wait for a reconnect.
	reconnectBackoffMin = time.Second
	reconnectBackoffMax = 5 * time.Second

	// How long Close waits for the reconnect loop, which may be inside a Connect to an
	// unresponsive libvirtd. Leaving it behind beats hanging shutdown.
	closeWait = 5 * time.Second
)

// connect opens the libvirt connection through dialer and keeps it open.
func (m *VMManager) connect(dialer socket.Dialer, minWait, maxWait time.Duration) error {
	l, err := dial(dialer)
	if err != nil {
		return err
	}

	m.conn.Store(l)
	m.done = make(chan struct{})
	m.stopped = make(chan struct{})
	go m.keepConnected(dialer, minWait, maxWait)
	return nil
}

// dial opens a new libvirt connection through dialer.
func dial(dialer socket.Dialer) (*libvirt.Libvirt, error) {
	l := libvirt.NewWithDialer(dialer)
	if err := l.Connect(); err != nil {
		return nil, fmt.Errorf("failed to connect to libvirt: %w", err)
	}
	return l, nil
}

// keepConnected replaces the libvirt connection whenever it drops. A libvirtd restart —
// a package upgrade, a crash — closes it, and nothing else would ever open it again:
// the pod stays up, /livez checks only the database, and every libvirt call fails
// until Lightstack itself restarts.
//
// It runs in the background so that no caller waits on a dial: while the connection
// is down, calls fail fast as before, and they succeed again within one backoff step
// of libvirtd returning. A replacement is published only once it is open. Reopening
// the dropped connection in place would race every call still holding it: go-libvirt
// reads a socket's connection state without the lock its Connect writes it under.
func (m *VMManager) keepConnected(dialer socket.Dialer, minWait, maxWait time.Duration) {
	defer close(m.stopped)
	for {
		select {
		case <-m.done:
			return
		case <-m.conn.Load().Disconnected():
		}
		log.Warn().Msg("libvirt connection lost; reconnecting")
		l, ok := m.redial(dialer, minWait, maxWait)
		if !ok {
			return
		}
		m.conn.Store(l)
		log.Info().Msg("libvirt connection re-established")
	}
}

// redial dials with capped exponential backoff until a connection opens. It reports
// false when the manager is closed first.
func (m *VMManager) redial(dialer socket.Dialer, minWait, maxWait time.Duration) (*libvirt.Libvirt, bool) {
	for wait := minWait; ; wait = min(2*wait, maxWait) {
		select {
		case <-m.done:
			return nil, false
		case <-time.After(wait):
		}
		l, err := dial(dialer)
		if err == nil {
			return l, true
		}
		log.Debug().Err(err).Dur("next_attempt_in", min(2*wait, maxWait)).Msg("libvirt reconnect failed")
	}
}

// CreateVM creates a virtual machine
func (m *VMManager) CreateVM(ctx context.Context, xml string) (string, error) {
	if m.mode == "stub" {
		return m.createVMStub(xml)
	}
	return m.createVMReal(ctx, xml)
}

// createVMStub simulates VM creation
func (m *VMManager) createVMStub(xml string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	vmUUID := uuid.New().String()
	m.stubVMs[vmUUID] = &stubVM{
		uuid:       vmUUID,
		xml:        xml,
		state:      VMStateRunning,
		powerState: 1, // Running
		createdAt:  time.Now(),
	}

	return vmUUID, nil
}

// createVMReal creates a real VM via libvirt
func (m *VMManager) createVMReal(ctx context.Context, xml string) (string, error) {
	// The domain names its console socket in a directory libvirt does not prepare;
	// refuse here rather than fail inside libvirt (SPEC-021 FR-017a).
	if err := m.consoleDirReady(); err != nil {
		return "", err
	}
	conn := m.conn.Load()
	if conn == nil {
		return "", fmt.Errorf("not connected to libvirt")
	}

	// Define the domain
	domain, err := conn.DomainDefineXML(xml)
	if err != nil {
		return "", fmt.Errorf("failed to define domain: %w", err)
	}

	// Start the domain
	if err := conn.DomainCreate(domain); err != nil {
		// Try to undefine on failure
		_ = conn.DomainUndefine(domain)
		return "", fmt.Errorf("failed to start domain: %w", err)
	}

	// Get the UUID string (domain.UUID is already a [16]byte)
	vmUUID := fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		domain.UUID[0:4], domain.UUID[4:6], domain.UUID[6:8], domain.UUID[8:10], domain.UUID[10:16])

	return vmUUID, nil
}

// DeleteVM deletes a virtual machine
func (m *VMManager) DeleteVM(ctx context.Context, vmUUID string) error {
	if m.mode == "stub" {
		return m.deleteVMStub(vmUUID)
	}
	return m.deleteVMReal(ctx, vmUUID)
}

// deleteVMStub simulates VM deletion
func (m *VMManager) deleteVMStub(vmUUID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.stubVMs[vmUUID]; !exists {
		return fmt.Errorf("%w: %s", ErrDomainNotFound, vmUUID)
	}

	delete(m.stubVMs, vmUUID)
	return nil
}

// deleteVMReal deletes a real VM via libvirt
func (m *VMManager) deleteVMReal(ctx context.Context, vmUUID string) error {
	conn := m.conn.Load()
	if conn == nil {
		return fmt.Errorf("not connected to libvirt")
	}

	// Parse UUID
	uuidBytes, err := parseUUID(vmUUID)
	if err != nil {
		return fmt.Errorf("invalid UUID: %w", err)
	}

	// Lookup domain
	domain, err := conn.DomainLookupByUUID(uuidBytes)
	if err != nil {
		return wrapLookupErr(err)
	}

	// Get domain state
	state, _, err := conn.DomainGetState(domain, 0)
	if err != nil {
		return fmt.Errorf("failed to get domain state: %w", err)
	}

	// Destroy if running
	if state == 1 { // Running
		if err := conn.DomainDestroy(domain); err != nil {
			return fmt.Errorf("failed to destroy domain: %w", err)
		}
	}

	// Undefine domain
	if err := conn.DomainUndefine(domain); err != nil {
		return fmt.Errorf("failed to undefine domain: %w", err)
	}

	return nil
}

// RebootVM reboots a virtual machine. A soft reboot asks the guest to restart itself,
// which a hung guest ignores; a hard one power-cycles the domain, which it cannot.
func (m *VMManager) RebootVM(ctx context.Context, vmUUID string, hard bool) error {
	if m.mode == "stub" {
		return m.rebootVMStub(vmUUID)
	}
	if hard {
		return m.hardRebootVMReal(vmUUID)
	}
	return m.rebootVMReal(ctx, vmUUID)
}

// rebootVMStub simulates VM reboot
func (m *VMManager) rebootVMStub(vmUUID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	vm, exists := m.stubVMs[vmUUID]
	if !exists {
		return fmt.Errorf("%w: %s", ErrDomainNotFound, vmUUID)
	}

	// Simulate reboot by keeping state as running
	vm.state = VMStateRunning
	vm.powerState = 1
	return nil
}

// rebootVMReal reboots a real VM via libvirt
func (m *VMManager) rebootVMReal(ctx context.Context, vmUUID string) error {
	conn := m.conn.Load()
	if conn == nil {
		return fmt.Errorf("not connected to libvirt")
	}

	uuidBytes, err := parseUUID(vmUUID)
	if err != nil {
		return fmt.Errorf("invalid UUID: %w", err)
	}

	domain, err := conn.DomainLookupByUUID(uuidBytes)
	if err != nil {
		return wrapLookupErr(err)
	}

	if err := conn.DomainReboot(domain, 0); err != nil {
		return fmt.Errorf("failed to reboot domain: %w", err)
	}

	return nil
}

// hardRebootVMReal power-cycles the domain: destroy it if it is active, then start it
// from its persistent definition. That is upstream's _hard_reboot, minus regenerating
// the XML, which Lightstack writes once at create. The guest gets a new qemu process,
// so it cannot ignore the reboot, and a domain that is not running simply starts, as
// upstream allows a HARD reboot of a stopped server.
func (m *VMManager) hardRebootVMReal(vmUUID string) error {
	// The domain names its console socket in a directory libvirt does not prepare;
	// refuse here rather than fail inside libvirt (SPEC-021 FR-017a).
	if err := m.consoleDirReady(); err != nil {
		return err
	}
	conn := m.conn.Load()
	if conn == nil {
		return fmt.Errorf("not connected to libvirt")
	}

	uuidBytes, err := parseUUID(vmUUID)
	if err != nil {
		return fmt.Errorf("invalid UUID: %w", err)
	}

	domain, err := conn.DomainLookupByUUID(uuidBytes)
	if err != nil {
		return wrapLookupErr(err)
	}

	// Active, not merely running: a paused or suspended guest has a qemu process too,
	// and DomainCreate refuses a domain that is still active.
	active, err := conn.DomainIsActive(domain)
	if err != nil {
		return fmt.Errorf("failed to check whether the domain is active: %w", err)
	}
	if active == 1 {
		if err := conn.DomainDestroy(domain); err != nil {
			return fmt.Errorf("failed to power off domain: %w", err)
		}
	}

	if err := conn.DomainCreate(domain); err != nil {
		return fmt.Errorf("domain powered off but failed to start: %w", err)
	}

	return nil
}

// StopVM stops a virtual machine, returning only once it is actually off.
//
// grace is how long the guest OS is given to shut down cleanly before it is forced;
// once it expires the guest is powered off the hard way, as upstream's shutdown_timeout
// does. A non-positive grace forces immediately.
//
// The grace period is a parameter rather than manager state because it is a deployment
// setting the caller already holds, and because a silent default here is precisely what
// would let a future caller reintroduce the fire-and-forget stop this replaces.
func (m *VMManager) StopVM(ctx context.Context, vmUUID string, grace time.Duration) error {
	if m.mode == "stub" {
		return m.stopVMStub(vmUUID)
	}
	return m.stopVMReal(ctx, vmUUID, grace)
}

// stopVMStub simulates VM stop
func (m *VMManager) stopVMStub(vmUUID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	vm, exists := m.stubVMs[vmUUID]
	if !exists {
		return fmt.Errorf("%w: %s", ErrDomainNotFound, vmUUID)
	}

	vm.state = VMStateShutoff
	vm.powerState = 4 // Shutoff
	return nil
}

// stopVMReal stops a real VM via libvirt: ask, wait, then insist.
//
// DomainShutdown is a REQUEST — libvirt documents it as a graceful shutdown coordinated
// with the guest OS, and the guest's own on_poweroff setting can change what it does
// with it. It returns as soon as libvirt has accepted the request, which may be long
// before the guest is off and may be forever if the guest ignores ACPI entirely.
//
// Returning nil at that point is what let a stop report success over a guest that was
// still running: the caller persisted SHUTOFF, the next start hit an already-active
// domain, and the record and reality never met again (cloud-storage#547 B12). So this
// does not return until the guest is actually off, one way or the other — the same
// shape as upstream's libvirt driver, which gives the guest shutdown_timeout seconds
// and then powers it off.
func (m *VMManager) stopVMReal(ctx context.Context, vmUUID string, grace time.Duration) error {
	conn := m.conn.Load()
	if conn == nil {
		return fmt.Errorf("not connected to libvirt")
	}

	uuidBytes, err := parseUUID(vmUUID)
	if err != nil {
		return fmt.Errorf("invalid UUID: %w", err)
	}

	domain, err := conn.DomainLookupByUUID(uuidBytes)
	if err != nil {
		return wrapLookupErr(err)
	}

	readState := func() (VMState, error) {
		state, _, err := conn.DomainGetState(domain, 0)
		if err != nil {
			return VMStateNoState, err
		}
		return libvirtState(int32(state)), nil
	}

	// Already off: the end state holds, and libvirt errors on a shutdown request to an
	// inactive domain. Turning that into a failed stop is how the reverse divergence
	// produced a 500 on an action that had nothing left to do.
	if state, err := readState(); err == nil && RunStateOf(state) == RunStateDown {
		return nil
	}

	if grace > 0 {
		if err := conn.DomainShutdown(domain); err != nil {
			return fmt.Errorf("failed to request shutdown of domain: %w", err)
		}
		if waitForShutoff(ctx, grace, shutoffPollInterval, readState) {
			return nil
		}
	}

	if err := conn.DomainDestroy(domain); err != nil {
		// The guest may have finished shutting down in the gap between giving up on it
		// and forcing it off — or gone entirely. Either is a stop that worked, and
		// reporting it as a failure would put the caller right back to guessing.
		state, stateErr := readState()
		if libvirt.IsNotFound(stateErr) || (stateErr == nil && RunStateOf(state) == RunStateDown) {
			return nil
		}
		return fmt.Errorf("guest did not shut down within %s and could not be forced off: %w", grace, err)
	}

	return nil
}

// StartVM starts a virtual machine
func (m *VMManager) StartVM(ctx context.Context, vmUUID string) error {
	if m.mode == "stub" {
		return m.startVMStub(vmUUID)
	}
	return m.startVMReal(ctx, vmUUID)
}

// startVMStub simulates VM start
func (m *VMManager) startVMStub(vmUUID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	vm, exists := m.stubVMs[vmUUID]
	if !exists {
		return fmt.Errorf("%w: %s", ErrDomainNotFound, vmUUID)
	}

	vm.state = VMStateRunning
	vm.powerState = 1 // Running
	return nil
}

// startVMReal starts a real VM via libvirt
func (m *VMManager) startVMReal(ctx context.Context, vmUUID string) error {
	// The domain names its console socket in a directory libvirt does not prepare;
	// refuse here rather than fail inside libvirt (SPEC-021 FR-017a).
	if err := m.consoleDirReady(); err != nil {
		return err
	}
	conn := m.conn.Load()
	if conn == nil {
		return fmt.Errorf("not connected to libvirt")
	}

	uuidBytes, err := parseUUID(vmUUID)
	if err != nil {
		return fmt.Errorf("invalid UUID: %w", err)
	}

	domain, err := conn.DomainLookupByUUID(uuidBytes)
	if err != nil {
		return wrapLookupErr(err)
	}

	// Already up: DomainCreate errors on an active domain, and the requested end state
	// holds. Callers refuse this case with a conflict BEFORE getting here, where they
	// can also correct the record; this covers the race between that check and this
	// call, and any caller that has no check of its own.
	if state, _, err := conn.DomainGetState(domain, 0); err == nil &&
		RunStateOf(libvirtState(int32(state))) == RunStateUp {
		return nil
	}

	if err := conn.DomainCreate(domain); err != nil {
		return fmt.Errorf("failed to start domain: %w", err)
	}

	return nil
}

// GetVMState returns the state of a virtual machine
func (m *VMManager) GetVMState(ctx context.Context, vmUUID string) (string, int, error) {
	if m.mode == "stub" {
		return m.getVMStateStub(vmUUID)
	}
	return m.getVMStateReal(ctx, vmUUID)
}

// getVMStateStub returns simulated VM state
func (m *VMManager) getVMStateStub(vmUUID string) (string, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	vm, exists := m.stubVMs[vmUUID]
	if !exists {
		return string(VMStateNoState), 0, fmt.Errorf("%w: %s", ErrDomainNotFound, vmUUID)
	}

	return string(vm.state), vm.powerState, nil
}

// getVMStateReal returns real VM state via libvirt
func (m *VMManager) getVMStateReal(ctx context.Context, vmUUID string) (string, int, error) {
	conn := m.conn.Load()
	if conn == nil {
		return string(VMStateNoState), 0, fmt.Errorf("not connected to libvirt")
	}

	uuidBytes, err := parseUUID(vmUUID)
	if err != nil {
		return string(VMStateNoState), 0, fmt.Errorf("invalid UUID: %w", err)
	}

	domain, err := conn.DomainLookupByUUID(uuidBytes)
	if err != nil {
		return string(VMStateNoState), 0, wrapLookupErr(err)
	}

	state, _, err := conn.DomainGetState(domain, 0)
	if err != nil {
		return string(VMStateNoState), 0, fmt.Errorf("failed to get domain state: %w", err)
	}

	// Convert libvirt state to OpenStack state. The power state needs mapping too —
	// libvirt's enum and nova's power_state are different numberings that happen to
	// agree on RUNNING (see novaPowerState).
	return string(libvirtState(int32(state))), novaPowerState(int32(state)), nil
}

// AttachDevice attaches a device (disk/network) to a VM
func (m *VMManager) AttachDevice(ctx context.Context, vmUUID, deviceXML string) error {
	if m.mode == "stub" {
		return m.attachDeviceStub(vmUUID, deviceXML)
	}
	return m.attachDeviceReal(ctx, vmUUID, deviceXML)
}

// attachDeviceStub simulates device attachment
func (m *VMManager) attachDeviceStub(vmUUID, deviceXML string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.stubVMs[vmUUID]; !exists {
		return fmt.Errorf("%w: %s", ErrDomainNotFound, vmUUID)
	}

	// In stub mode, we just verify the VM exists
	return nil
}

// attachDeviceReal attaches a device to a real VM via libvirt
func (m *VMManager) attachDeviceReal(ctx context.Context, vmUUID, deviceXML string) error {
	conn := m.conn.Load()
	if conn == nil {
		return fmt.Errorf("not connected to libvirt")
	}

	uuidBytes, err := parseUUID(vmUUID)
	if err != nil {
		return fmt.Errorf("invalid UUID: %w", err)
	}

	domain, err := conn.DomainLookupByUUID(uuidBytes)
	if err != nil {
		return wrapLookupErr(err)
	}

	// Attach device (libvirt flags: 0 for default, 1 for persistent, 2 for live)
	flags := uint32(3) // Both persistent and live (1 | 2)
	if err := conn.DomainAttachDeviceFlags(domain, deviceXML, flags); err != nil {
		return fmt.Errorf("failed to attach device: %w", err)
	}

	return nil
}

// DetachDevice detaches a device from a VM
func (m *VMManager) DetachDevice(ctx context.Context, vmUUID, deviceXML string) error {
	if m.mode == "stub" {
		return m.detachDeviceStub(vmUUID, deviceXML)
	}
	return m.detachDeviceReal(ctx, vmUUID, deviceXML)
}

// detachDeviceStub simulates device detachment
func (m *VMManager) detachDeviceStub(vmUUID, deviceXML string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.stubVMs[vmUUID]; !exists {
		return fmt.Errorf("%w: %s", ErrDomainNotFound, vmUUID)
	}

	// In stub mode, we just verify the VM exists
	return nil
}

// detachDeviceReal detaches a device from a real VM via libvirt
func (m *VMManager) detachDeviceReal(ctx context.Context, vmUUID, deviceXML string) error {
	conn := m.conn.Load()
	if conn == nil {
		return fmt.Errorf("not connected to libvirt")
	}

	uuidBytes, err := parseUUID(vmUUID)
	if err != nil {
		return fmt.Errorf("invalid UUID: %w", err)
	}

	domain, err := conn.DomainLookupByUUID(uuidBytes)
	if err != nil {
		return wrapLookupErr(err)
	}

	// Detach device (libvirt flags: 0 for default, 1 for persistent, 2 for live)
	flags := uint32(3) // Both persistent and live (1 | 2)
	if err := conn.DomainDetachDeviceFlags(domain, deviceXML, flags); err != nil {
		return fmt.Errorf("failed to detach device: %w", err)
	}

	return nil
}

// SuspendVM suspends a virtual machine (saves RAM to disk)
func (m *VMManager) SuspendVM(ctx context.Context, vmUUID string) error {
	if m.mode == "stub" {
		return m.suspendVMStub(vmUUID)
	}
	return m.suspendVMReal(ctx, vmUUID)
}

// suspendVMStub simulates VM suspend
func (m *VMManager) suspendVMStub(vmUUID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	vm, exists := m.stubVMs[vmUUID]
	if !exists {
		return fmt.Errorf("%w: %s", ErrDomainNotFound, vmUUID)
	}

	vm.state = VMStatePaused
	vm.powerState = 3 // Paused
	return nil
}

// suspendVMReal suspends a real VM via libvirt
func (m *VMManager) suspendVMReal(ctx context.Context, vmUUID string) error {
	conn := m.conn.Load()
	if conn == nil {
		return fmt.Errorf("not connected to libvirt")
	}

	uuidBytes, err := parseUUID(vmUUID)
	if err != nil {
		return fmt.Errorf("invalid UUID: %w", err)
	}

	domain, err := conn.DomainLookupByUUID(uuidBytes)
	if err != nil {
		return wrapLookupErr(err)
	}

	// Suspend domain (saves to disk)
	if err := conn.DomainManagedSave(domain, 0); err != nil {
		return fmt.Errorf("failed to suspend domain: %w", err)
	}

	return nil
}

// ResumeVM resumes a suspended virtual machine
func (m *VMManager) ResumeVM(ctx context.Context, vmUUID string) error {
	if m.mode == "stub" {
		return m.resumeVMStub(vmUUID)
	}
	return m.resumeVMReal(ctx, vmUUID)
}

// resumeVMStub simulates VM resume
func (m *VMManager) resumeVMStub(vmUUID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	vm, exists := m.stubVMs[vmUUID]
	if !exists {
		return fmt.Errorf("%w: %s", ErrDomainNotFound, vmUUID)
	}

	vm.state = VMStateRunning
	vm.powerState = 1 // Running
	return nil
}

// resumeVMReal resumes a real VM via libvirt
func (m *VMManager) resumeVMReal(ctx context.Context, vmUUID string) error {
	// The domain names its console socket in a directory libvirt does not prepare;
	// refuse here rather than fail inside libvirt (SPEC-021 FR-017a).
	if err := m.consoleDirReady(); err != nil {
		return err
	}
	conn := m.conn.Load()
	if conn == nil {
		return fmt.Errorf("not connected to libvirt")
	}

	uuidBytes, err := parseUUID(vmUUID)
	if err != nil {
		return fmt.Errorf("invalid UUID: %w", err)
	}

	domain, err := conn.DomainLookupByUUID(uuidBytes)
	if err != nil {
		return wrapLookupErr(err)
	}

	// Resume domain (starts from managed save)
	if err := conn.DomainCreate(domain); err != nil {
		return fmt.Errorf("failed to resume domain: %w", err)
	}

	return nil
}

// Close stops the reconnect loop, then closes the libvirt connection. In that order: a
// loop still running would take the deliberate disconnect for a lost connection and
// dial again. Safe to call more than once.
func (m *VMManager) Close() {
	m.closeOnce.Do(func() {
		if m.done != nil {
			close(m.done)
			select {
			case <-m.stopped:
			case <-time.After(closeWait):
			}
		}
		if conn := m.conn.Load(); conn != nil {
			_ = conn.Disconnect()
		}
	})
}

// wrapLookupErr classifies a domain lookup failure.
//
// libvirt distinguishes "no such domain" (ERR_NO_DOMAIN) from every other RPC
// failure, and go-libvirt exposes that as IsNotFound. Anything else — a dropped
// connection, a permission problem — stays an ordinary error so it is not mistaken for
// a state the caller can act on.
func wrapLookupErr(err error) error {
	if libvirt.IsNotFound(err) {
		return fmt.Errorf("%w: %v", ErrDomainNotFound, err)
	}
	return fmt.Errorf("looking up domain: %w", err)
}

// parseUUID converts a UUID string to bytes
func parseUUID(uuidStr string) (libvirt.UUID, error) {
	var result libvirt.UUID
	parsed, err := uuid.Parse(uuidStr)
	if err != nil {
		return result, err
	}
	copy(result[:], parsed[:])
	return result, nil
}

// libvirtState names a libvirt domain state. Typed rather than a bare string so it can
// be handed straight to RunStateOf.
func libvirtState(state int32) VMState {
	return VMState(libvirtStateToString(state))
}

// libvirtStateToString converts libvirt state to string
func libvirtStateToString(state int32) string {
	switch state {
	case 0:
		return string(VMStateNoState)
	case 1:
		return string(VMStateRunning)
	case 2:
		return string(VMStateBlocked)
	case 3:
		return string(VMStatePaused)
	case 4:
		return string(VMStateShutdown)
	case 5:
		return string(VMStateShutoff)
	case 6:
		return string(VMStateCrashed)
	case 7:
		return string(VMStatePMSuspended)
	default:
		return string(VMStateNoState)
	}
}
