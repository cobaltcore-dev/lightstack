package hypervisor

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"path/filepath"
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
	// ErrConsoleNotReady: the domain exists but its console has no socket yet,
	// i.e. it is not running. Maps to 409.
	ErrConsoleNotReady = errors.New("console not ready")
	// ErrNoGraphicalConsole: the domain has no VNC graphics device. Maps to 409.
	ErrNoGraphicalConsole = errors.New("no graphical console")
	// ErrConsoleParse: libvirt returned XML we could not read. Maps to 500.
	ErrConsoleParse = errors.New("console xml parse failed")
	// ErrConsoleNotSocket: the guest console is a TCP listener — a domain defined
	// before consoles moved to UNIX sockets. A port can be handed to another guest
	// while a token still names it, so refuse rather than proxy to it.
	ErrConsoleNotSocket = errors.New("console is not a unix socket")
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
			Socket   string `xml:"socket,attr"`
			ListenEl []struct {
				Type    string `xml:"type,attr"`
				Address string `xml:"address,attr"`
				Socket  string `xml:"socket,attr"`
			} `xml:"listen"`
		} `xml:"graphics"`
	} `xml:"devices"`
}

// GetConsoleSocket returns the UNIX socket libvirt created for the instance's
// running VNC console.
//
// It is read fresh on every call and never cached: libvirt creates the socket in
// the domain's per-start private directory, so the path changes on every start —
// which is exactly what makes a stale token name nothing instead of another guest.
func (m *VMManager) GetConsoleSocket(ctx context.Context, vmUUID string) (string, error) {
	conn := m.conn.Load()
	if m.mode == "stub" || conn == nil {
		return "", fmt.Errorf("%w: nova is running in stub mode, there is no guest to display", ErrConsoleUnavailable)
	}

	ctx, cancel := context.WithTimeout(ctx, consoleReadTimeout)
	defer cancel()

	type result struct {
		socket string
		err    error
	}
	// go-libvirt's calls are blocking and context-free, so the deadline is
	// enforced by racing them. The goroutine may outlive this function if
	// libvirt is wedged; the buffered channel keeps it from leaking on send.
	// It ends with the wedge: go-libvirt closes every pending call's channel when
	// the socket drops, and a recovered libvirtd answers them. Their number is
	// bounded too: issuance times out as well, so no new tokens appear, and the
	// tokens the sweep checks expire within the TTL.
	ch := make(chan result, 1)
	go func() {
		socket, err := fetchDomainConsoleSocket(conn, vmUUID)
		ch <- result{socket: socket, err: err}
	}()

	select {
	case <-ctx.Done():
		return "", fmt.Errorf("console readback timed out after %s: %w", consoleReadTimeout, ctx.Err())
	case r := <-ch:
		return r.socket, r.err
	}
}

// fetchDomainConsoleSocket is the blocking libvirt half of GetConsoleSocket, split
// out so that the deadline machinery above reads as deadline machinery and this
// reads as libvirt work. It is called only from that goroutine, and it is the only
// part of the readback that needs a live connection — everything above it is
// unit-testable, everything here is not.
func fetchDomainConsoleSocket(conn *libvirt.Libvirt, vmUUID string) (string, error) {
	uuidBytes, err := parseUUID(vmUUID)
	if err != nil {
		return "", fmt.Errorf("invalid UUID: %w", err)
	}

	domain, err := conn.DomainLookupByUUID(uuidBytes)
	if err != nil {
		// Two very different failures arrive through the same call, and they
		// deserve different answers. Both were observed on the host:
		//
		//   * libvirt says NO_DOMAIN — the instance exists in Lightstack's database
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
			return "", fmt.Errorf("%w: no libvirt domain for this instance", ErrConsoleNotReady)
		}
		return "", fmt.Errorf("looking up domain (is libvirtd running?): %w", err)
	}

	// Flags 0 = the RUNNING definition, the only one that carries the socket path:
	// DomainXMLInactive returns the persistent definition, whose <listen
	// type='socket'/> has none until libvirt starts the domain.
	desc, err := conn.DomainGetXMLDesc(domain, 0)
	if err != nil {
		return "", fmt.Errorf("failed to read domain XML: %w", err)
	}

	return parseConsoleSocket(desc)
}

// parseConsoleSocket extracts the VNC console's UNIX socket from a running domain's
// XML.
func parseConsoleSocket(desc string) (string, error) {
	var d domainGraphics
	if err := xml.Unmarshal([]byte(desc), &d); err != nil {
		return "", fmt.Errorf("%w: %v", ErrConsoleParse, err)
	}

	for _, g := range d.Devices.Graphics {
		if g.Type != "vnc" {
			continue
		}

		socket := g.Socket
		tcp := g.Listen != "" || g.AutoPort != "" || g.Port != 0
		for _, l := range g.ListenEl {
			switch l.Type {
			case "socket":
				if socket == "" {
					socket = l.Socket
				}
			case "address", "network":
				tcp = true
			}
		}

		switch {
		case socket != "":
			if !filepath.IsAbs(socket) {
				return "", fmt.Errorf("%w: console socket %q is not an absolute path", ErrConsoleParse, socket)
			}
			return socket, nil
		case tcp && g.Port > 0:
			// Refusing is the safe direction: a TCP port outlives the guest that
			// held it, so a token naming it can reach whichever guest libvirt hands
			// it to next (FR-014, FR-017).
			return "", fmt.Errorf("%w: guest console listens on TCP port %d (a domain defined before consoles moved to unix sockets)", ErrConsoleNotSocket, g.Port)
		default:
			// A socket listener before the domain starts, or a legacy TCP one whose
			// autoport is unresolved: either way nothing is serving a console now.
			return "", fmt.Errorf("%w: libvirt has not created a console for this domain (instance not running?)", ErrConsoleNotReady)
		}
	}

	return "", fmt.Errorf("%w: domain has no vnc graphics device", ErrNoGraphicalConsole)
}
