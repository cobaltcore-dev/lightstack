package hypervisor

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cobaltcore-dev/lightstack/pkg/paths"
	"github.com/rs/zerolog/log"
)

// ErrConsoleSocketDirNotReady refuses a guest start while the console socket directory
// is not QEMU's. Each guest's domain names its VNC socket there (SPEC-021 FR-017), and
// libvirt prepares only its own private directories, never one a domain names, so QEMU
// must create the socket itself. With the wrong owner every start would fail inside
// libvirt, so starts are refused before libvirt is asked (FR-017a).
var ErrConsoleSocketDirNotReady = errors.New("console socket directory not ready")

// dacOwner is the uid:gid libvirt runs QEMU as (its DAC base label). Cached on the
// VMManager so per-instance directories can be chowned to it without re-reading libvirt
// capabilities on every guest create.
type dacOwner struct {
	uid, gid int
}

// PrepareConsoleSocketDir makes dir the guests' console socket directory: owned by the
// uid:gid libvirt runs QEMU as, which it reports in its capabilities, with mode 0750. QEMU can
// then create and remove its socket there, the kvm group can read it, and nobody else gets in.
//
// A caller arriving while an attempt runs takes that attempt's result. The first success is
// never withdrawn: a later attempt that fails changes nothing in effect, and one that succeeds
// replaces QEMU's user (SPEC-031 FR-015). Guest starts are refused until one has succeeded.
func (m *VMManager) PrepareConsoleSocketDir(ctx context.Context, dir string) error {
	m.consoleDirPath.Store(&dir)
	return m.prepareShared(ctx, dir)
}

// prepareShared runs one preparation attempt, or joins the one in flight (single-flight).
func (m *VMManager) prepareShared(ctx context.Context, dir string) error {
	_, err, _ := m.prepGroup.Do(dir, func() (any, error) {
		return nil, m.prepareConsoleSocketDir(ctx, dir)
	})
	return err
}

// prepareConsoleSocketDir is one attempt, logged: the first failure at ERROR with its cause,
// later ones at DEBUG, the first success at INFO naming the directory and QEMU's user, a redo
// at DEBUG unless QEMU's user changed (SPEC-031 FR-017a).
func (m *VMManager) prepareConsoleSocketDir(ctx context.Context, dir string) error {
	uid, gid, err := m.attemptPrepare(ctx, dir)
	if err != nil {
		if !m.prepared.Load() && m.prepFailureLogged.CompareAndSwap(false, true) {
			log.Error().Err(err).Str("dir", dir).
				Msg("console socket directory not ready; guest starts are refused until it is")
		} else {
			log.Debug().Err(err).Str("dir", dir).Msg("console socket directory preparation failed")
		}
		return err
	}
	owner := fmt.Sprintf("%d:%d", uid, gid)
	prev := m.qemuOwner.Swap(&dacOwner{uid: uid, gid: gid})
	switch wasPrepared := m.prepared.Swap(true); {
	case !wasPrepared:
		log.Info().Str("dir", dir).Str("qemu_user", owner).Msg("console socket directory ready; guests can start")
	case prev == nil || prev.uid != uid || prev.gid != gid:
		log.Info().Str("dir", dir).Str("qemu_user", owner).Msg("console socket directory prepared again for QEMU's new user")
	default:
		log.Debug().Str("dir", dir).Str("qemu_user", owner).Msg("console socket directory checked again after a reconnect")
	}
	return nil
}

// attemptPrepare reads QEMU's user from libvirt's capabilities, under the 1-second bound, and
// makes dir that user's. It changes no state: the caller records a success.
func (m *VMManager) attemptPrepare(ctx context.Context, dir string) (uid, gid int, err error) {
	conn := m.conn.Load()
	if conn == nil {
		return 0, 0, errors.New("not connected to libvirt")
	}
	caps, err := withDeadline(ctx, conn.ConnectGetCapabilities)
	if err != nil {
		return 0, 0, fmt.Errorf("reading libvirt capabilities: %w", err)
	}
	if uid, gid, err = qemuDACOwner(caps); err != nil {
		return 0, 0, err
	}
	if err := ensureOwnedDir(dir, uid, gid); err != nil {
		return 0, 0, err
	}
	return uid, gid, nil
}

// ensurePrepared lets a guest start go ahead once the console socket directory is QEMU's. If
// it is not yet, the start makes one attempt — or takes the result of the one in flight —
// bounded at 1 second, and is refused with ErrConsoleSocketDirNotReady naming the directory
// and the cause when that fails (SPEC-031 FR-013, FR-016).
func (m *VMManager) ensurePrepared(ctx context.Context) error {
	if m.prepared.Load() {
		return nil
	}
	dir := m.consoleSocketDir()
	if err := m.prepareShared(ctx, dir); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrConsoleSocketDirNotReady, dir, err)
	}
	return nil
}

// consoleSocketDir is the directory this manager prepares: the one it was given, else the
// state path's.
func (m *VMManager) consoleSocketDir() string {
	if p := m.consoleDirPath.Load(); p != nil && *p != "" {
		return *p
	}
	return paths.ConsoleSocketDir()
}

// ConsoleDirPrepared reports whether the console socket directory has been prepared at least
// once — what real-mode readiness waits for (SPEC-031 FR-017). Once true, it stays true.
func (m *VMManager) ConsoleDirPrepared() bool { return m.prepared.Load() }

// capabilitiesSecModels is the slice of libvirt's capabilities document that names the
// security models and the labels guests run under (docs/formatcaps.rst).
type capabilitiesSecModels struct {
	Host struct {
		SecModels []struct {
			Model      string `xml:"model"`
			BaseLabels []struct {
				Type  string `xml:"type,attr"`
				Value string `xml:",chardata"`
			} `xml:"baselabel"`
		} `xml:"secmodel"`
	} `xml:"host"`
}

// qemuDACOwner returns the uid:gid libvirt runs QEMU as: the DAC security model's base
// label for kvm domains, else the one for qemu domains, formatted "+uid:+gid".
func qemuDACOwner(capsXML string) (uid, gid int, err error) {
	var caps capabilitiesSecModels
	if err := xml.Unmarshal([]byte(capsXML), &caps); err != nil {
		return 0, 0, fmt.Errorf("parsing libvirt capabilities: %w", err)
	}
	labels := map[string]string{}
	for _, sm := range caps.Host.SecModels {
		if sm.Model != "dac" {
			continue
		}
		for _, l := range sm.BaseLabels {
			labels[l.Type] = strings.TrimSpace(l.Value)
		}
	}
	label, ok := labels["kvm"]
	if !ok {
		label, ok = labels["qemu"]
	}
	if !ok {
		return 0, 0, errors.New("libvirt reports no DAC base label for kvm or qemu domains, so QEMU's user is unknown")
	}
	u, g, found := strings.Cut(label, ":")
	if !found {
		return 0, 0, fmt.Errorf("DAC base label %q is not uid:gid", label)
	}
	if uid, err = strconv.Atoi(strings.TrimPrefix(u, "+")); err != nil {
		return 0, 0, fmt.Errorf("DAC base label %q: uid: %w", label, err)
	}
	if gid, err = strconv.Atoi(strings.TrimPrefix(g, "+")); err != nil {
		return 0, 0, fmt.Errorf("DAC base label %q: gid: %w", label, err)
	}
	return uid, gid, nil
}

// ensureOwnedDir makes dir exist, owned by uid:gid, with mode 0750. Missing parents are
// created 0755, not 0750: QEMU must still traverse the storage root to reach its disks.
// The mode is set explicitly because MkdirAll leaves an existing directory's alone.
func ensureOwnedDir(dir string, uid, gid int) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(dir), err)
	}
	if err := os.Mkdir(dir, 0o750); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	if err := os.Chown(dir, uid, gid); err != nil {
		return fmt.Errorf("making %s QEMU's (%d:%d): %w", dir, uid, gid, err)
	}
	if err := os.Chmod(dir, 0o750); err != nil {
		return fmt.Errorf("setting the mode of %s: %w", dir, err)
	}
	return nil
}

// withDeadline runs a libvirt call under the console readback's 1s bound. go-libvirt's
// generated calls take no context, so the bound is imposed by racing them; the buffered
// channel lets a call that outlives the deadline finish without leaking on send
// (Constitution III).
//
// The goroutine may outlive the call if libvirt is wedged, but it ends with the wedge:
// go-libvirt closes every pending call's channel when the socket drops, and a recovered
// libvirtd answers them. Their number is bounded too: console issuance times out as
// well, so no new tokens appear, and the tokens the sweep checks expire within the TTL.
func withDeadline[T any](ctx context.Context, call func() (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(ctx, consoleReadTimeout)
	defer cancel()

	type result struct {
		v   T
		err error
	}
	ch := make(chan result, 1)
	go func() {
		v, err := call()
		ch <- result{v, err}
	}()

	select {
	case <-ctx.Done():
		var zero T
		return zero, fmt.Errorf("libvirt did not answer within %s: %w", consoleReadTimeout, ctx.Err())
	case r := <-ch:
		return r.v, r.err
	}
}
