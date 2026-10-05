package hypervisor

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/cobaltcore-dev/lightstack/pkg/paths"
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
	// ErrConsoleLegacySocket: the console socket is outside the console socket
	// directory — a domain defined before consoles moved there, whose socket sits in
	// libvirt's per-domain directory where the proxy cannot reach it. Maps to 500.
	ErrConsoleLegacySocket = errors.New("console socket outside the console socket directory")
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

// GetConsoleSocket returns the UNIX socket of the instance's running VNC console and
// the libvirt domain ID of the run it belongs to.
//
// Both are read fresh on every call and never cached. The socket path is fixed per
// instance (<state_path>/console-sockets/<uuid>.sock, SPEC-021 FR-017), so it is never
// another guest's; libvirt assigns a new domain ID on every start, which is how a token
// that outlived a restart is recognised (FR-014a). A socket anywhere else belongs to a
// domain defined before the move and is refused with ErrConsoleLegacySocket.
func (m *VMManager) GetConsoleSocket(ctx context.Context, vmUUID string) (string, int32, error) {
	if m.mode == "stub" {
		return "", 0, fmt.Errorf("%w: nova is running in stub mode, there is no guest to display", ErrConsoleUnavailable)
	}
	conn := m.conn.Load()
	if conn == nil {
		return "", 0, errors.New("not connected to libvirt")
	}

	console, err := withDeadline(ctx, func() (domainConsole, error) { return fetchDomainConsole(conn, vmUUID) })
	if err != nil {
		return "", 0, err
	}
	if err := checkConsoleSocketDir(console.socket); err != nil {
		return "", 0, err
	}
	return console.socket, console.domainID, nil
}

// checkConsoleSocketDir refuses a socket outside the console socket directory, the only
// one the proxy mounts (FR-017, FR-023c).
func checkConsoleSocketDir(socket string) error {
	if filepath.Dir(filepath.Clean(socket)) != paths.ConsoleSocketDir() {
		return fmt.Errorf("%w: %s is not in %s (a domain defined before console sockets moved there; see docs/UPGRADING.md)",
			ErrConsoleLegacySocket, socket, paths.ConsoleSocketDir())
	}
	return nil
}

// domainConsole is what one readback learns about a guest's console.
type domainConsole struct {
	socket   string
	domainID int32
}

// fetchDomainConsole is the blocking libvirt half of GetConsoleSocket, split out so
// that the deadline machinery reads as deadline machinery and this reads as libvirt
// work. It is the only part of the readback that needs a live connection.
func fetchDomainConsole(conn *libvirt.Libvirt, vmUUID string) (domainConsole, error) {
	uuidBytes, err := parseUUID(vmUUID)
	if err != nil {
		return domainConsole{}, fmt.Errorf("invalid UUID: %w", err)
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
			return domainConsole{}, fmt.Errorf("%w: no libvirt domain for this instance", ErrConsoleNotReady)
		}
		return domainConsole{}, fmt.Errorf("looking up domain (is libvirtd running?): %w", err)
	}

	// The definition cannot tell a stopped guest from a running one: the socket path is
	// fixed per instance, so even the persistent definition names it, and that is what
	// libvirt returns for a domain that is not running. libvirt reports such a domain
	// with ID -1, and no QEMU serves its console.
	if domain.ID <= 0 {
		return domainConsole{}, fmt.Errorf("%w: the domain is not running", ErrConsoleNotReady)
	}

	// Flags 0 = the running definition, the one libvirt hands QEMU.
	desc, err := conn.DomainGetXMLDesc(domain, 0)
	if err != nil {
		return domainConsole{}, fmt.Errorf("failed to read domain XML: %w", err)
	}

	socket, err := parseConsoleSocket(desc)
	if err != nil {
		return domainConsole{}, err
	}
	return domainConsole{socket: socket, domainID: domain.ID}, nil
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
			// A socket listener with no path (defined before the sockets moved, and not
			// started), or a legacy TCP one whose autoport is unresolved: either way
			// nothing is serving a console now.
			return "", fmt.Errorf("%w: libvirt has not created a console for this domain (instance not running?)", ErrConsoleNotReady)
		}
	}

	return "", fmt.Errorf("%w: domain has no vnc graphics device", ErrNoGraphicalConsole)
}
