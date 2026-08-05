package hypervisor

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"time"

	"github.com/digitalocean/go-libvirt"
)

// consoleReadTimeout bounds the libvirt console readback. Constitution III caps
// external dependencies at 1 second, and go-libvirt's generated calls take no
// context — the dialer's timeout only covers connect, which happens once at
// startup — so the caller has to impose this itself (SPEC-021 FR-007).
const consoleReadTimeout = time.Second

// Console readback failures, each mapping to a distinct API response so the
// caller never has to guess why a console is unavailable (SPEC-021 FR-006).
var (
	// ErrConsoleUnavailable: this deployment cannot serve consoles at all
	// (stub mode). Maps to 501.
	ErrConsoleUnavailable = errors.New("console unavailable")
	// ErrConsoleNotReady: the domain exists but libvirt has not resolved its
	// autoport yet, i.e. it is not running. Maps to 409.
	ErrConsoleNotReady = errors.New("console not ready")
	// ErrNoGraphicalConsole: the domain has no VNC graphics device. Maps to 409.
	ErrNoGraphicalConsole = errors.New("no graphical console")
	// ErrConsoleParse: libvirt returned XML we could not read. Maps to 500.
	ErrConsoleParse = errors.New("console xml parse failed")
	// ErrConsoleNotLoopback: the guest console is listening off-host, so the
	// token would not be the only way in. Refuse rather than proxy to it.
	ErrConsoleNotLoopback = errors.New("console not bound to loopback")
)

// domainGraphics is a deliberately narrow view of the domain XML — only the
// graphics elements. Unmarshalling the whole domain would couple this to every
// unrelated schema change libvirt makes.
type domainGraphics struct {
	Devices struct {
		Graphics []struct {
			Type     string `xml:"type,attr"`
			Port     int    `xml:"port,attr"`
			AutoPort string `xml:"autoport,attr"`
			Listen   string `xml:"listen,attr"`
			ListenEl []struct {
				Type    string `xml:"type,attr"`
				Address string `xml:"address,attr"`
			} `xml:"listen"`
		} `xml:"graphics"`
	} `xml:"devices"`
}

// GetConsoleEndpoint returns the host and port libvirt actually assigned to the
// instance's VNC console.
//
// It is read fresh on every call and never cached: libvirt allocates the port
// when the domain starts, so it can differ after any stop/start, and a cached
// value is how the pre-SPEC-021 console handed out endpoints that pointed
// nowhere — or worse, at whatever instance later took that port.
func (m *VMManager) GetConsoleEndpoint(ctx context.Context, vmUUID string) (string, int, error) {
	if m.mode == "stub" || m.conn == nil {
		return "", 0, fmt.Errorf("%w: nova is running in stub mode, there is no guest to display", ErrConsoleUnavailable)
	}

	ctx, cancel := context.WithTimeout(ctx, consoleReadTimeout)
	defer cancel()

	type result struct {
		host string
		port int
		err  error
	}
	// go-libvirt's calls are blocking and context-free, so the deadline is
	// enforced by racing them. The goroutine may outlive this function if
	// libvirt is wedged; the buffered channel keeps it from leaking on send.
	ch := make(chan result, 1)
	go func() {
		host, port, err := m.fetchDomainConsoleEndpoint(vmUUID)
		ch <- result{host: host, port: port, err: err}
	}()

	select {
	case <-ctx.Done():
		return "", 0, fmt.Errorf("console readback timed out after %s: %w", consoleReadTimeout, ctx.Err())
	case r := <-ch:
		return r.host, r.port, r.err
	}
}

// fetchDomainConsoleEndpoint is the blocking libvirt half of GetConsoleEndpoint,
// split out so that the deadline machinery above reads as deadline machinery and this
// reads as libvirt work. It is called only from that goroutine, and it is the only
// part of the readback that needs a live connection — everything above it is
// unit-testable, everything here is not.
func (m *VMManager) fetchDomainConsoleEndpoint(vmUUID string) (string, int, error) {
	uuidBytes, err := parseUUID(vmUUID)
	if err != nil {
		return "", 0, fmt.Errorf("invalid UUID: %w", err)
	}

	domain, err := m.conn.DomainLookupByUUID(uuidBytes)
	if err != nil {
		// Two very different failures arrive through the same call, and they
		// deserve different answers. Both were observed on the host:
		//
		//   * libvirt says NO_DOMAIN — the instance exists in O3K's database
		//     but has no domain (created without an image, so it reached
		//     ERROR). That is "not running": the caller's problem, 409.
		//   * anything else, typically a broken connection because libvirtd is
		//     down. The guest may well still be running; we simply cannot ask.
		//     That is our problem, 500.
		//
		// Mapping both to 409 would tell an operator whose libvirtd had died
		// that their instance is not running, which is both wrong and
		// misdirecting.
		if libvirt.IsNotFound(err) {
			return "", 0, fmt.Errorf("%w: no libvirt domain for this instance", ErrConsoleNotReady)
		}
		return "", 0, fmt.Errorf("looking up domain (is libvirtd running?): %w", err)
	}

	// Flags 0 = the RUNNING definition, where autoport has been resolved to a
	// real port. DomainXMLInactive would return the persistent definition with
	// port="-1" and quietly reintroduce the bug this feature exists to fix.
	desc, err := m.conn.DomainGetXMLDesc(domain, 0)
	if err != nil {
		return "", 0, fmt.Errorf("failed to read domain XML: %w", err)
	}

	return parseConsoleEndpoint(desc)
}

// parseConsoleEndpoint extracts the VNC listen address and resolved port from a
// running domain's XML.
func parseConsoleEndpoint(desc string) (string, int, error) {
	var d domainGraphics
	if err := xml.Unmarshal([]byte(desc), &d); err != nil {
		return "", 0, fmt.Errorf("%w: %v", ErrConsoleParse, err)
	}

	for _, g := range d.Devices.Graphics {
		if g.Type != "vnc" {
			continue
		}
		// port="-1" means autoport has not been resolved, which in practice means
		// the domain is defined but not running.
		if g.Port <= 0 {
			return "", 0, fmt.Errorf("%w: libvirt has not assigned a console port (instance not running?)", ErrConsoleNotReady)
		}

		host := g.Listen
		for _, l := range g.ListenEl {
			if l.Type == "address" && l.Address != "" {
				host = l.Address
			}
		}
		if host == "" {
			host = "127.0.0.1"
		}
		if !isLoopback(host) {
			// Refusing is the safe direction: a guest listening off-host can be
			// reached without a token, so proxying to it would imply a protection
			// that is not there (FR-017).
			return "", 0, fmt.Errorf("%w: guest console listens on %s, expected loopback", ErrConsoleNotLoopback, host)
		}
		return host, g.Port, nil
	}

	return "", 0, fmt.Errorf("%w: domain has no vnc graphics device", ErrNoGraphicalConsole)
}

// isLoopback reports whether the guest console is bound host-locally, which is
// what makes the token-validating proxy the only route in.
func isLoopback(host string) bool {
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}
