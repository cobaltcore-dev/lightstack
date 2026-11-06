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
	libvirt "github.com/digitalocean/go-libvirt"
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

// prepareShared makes the console socket directory — the one every domain names its console
// socket in (paths.ConsoleSocketDir) — the guests': owned by the uid:gid libvirt runs QEMU as,
// which it reports in its capabilities, with mode 0750. QEMU can then create and remove its
// socket there, the kvm group can read it, and nobody else gets in.
//
// It runs one attempt, or joins the one in flight (single-flight). The first success is never
// withdrawn from readiness, and one that succeeds replaces QEMU's user (SPEC-031 FR-015). Guest
// starts are refused until one has succeeded over the current connection.
func (m *VMManager) prepareShared(ctx context.Context) error {
	_, err, _ := m.prepGroup.Do("console-socket-dir", func() (any, error) {
		return nil, m.prepareConsoleSocketDir(ctx, paths.ConsoleSocketDir())
	})
	return err
}

// prepareConsoleSocketDir is one attempt, logged (SPEC-031 FR-017a): the first failure at ERROR
// with its cause, later ones at DEBUG, the first success at INFO naming the directory and QEMU's
// user, a redo at DEBUG unless QEMU's user changed.
//
// A success records the connection it read QEMU's user over after the user itself, so a guest
// start that finds that connection current also finds its user.
func (m *VMManager) prepareConsoleSocketDir(ctx context.Context, dir string) error {
	conn := m.conn.Load()
	uid, gid, err := m.attemptPrepare(ctx, conn, dir)
	if err != nil {
		m.logPrepFailure(dir, err)
		return err
	}
	prev := m.qemuOwner.Swap(&dacOwner{uid: uid, gid: gid})
	m.preparedFor.Store(conn)
	first := !m.prepared.Swap(true)
	logPrepSuccess(dir, fmt.Sprintf("%d:%d", uid, gid), first, prev == nil || prev.uid != uid || prev.gid != gid)
	return nil
}

// logPrepFailure logs the first failure before any success at ERROR, every other at DEBUG. An
// attempt that failed for want of a connection is not that failure: the connection loop reports
// the outage itself, and spending the ERROR line on it would leave the cause that keeps guests
// from starting once libvirt answers at DEBUG.
func (m *VMManager) logPrepFailure(dir string, err error) {
	if !errors.Is(err, errNotConnected) && !m.prepared.Load() && m.prepFailureLogged.CompareAndSwap(false, true) {
		log.Error().Err(err).Str("dir", dir).
			Msg("console socket directory not ready; guest starts are refused until it is")
		return
	}
	log.Debug().Err(err).Str("dir", dir).Msg("console socket directory preparation failed")
}

// logPrepSuccess logs the first success and a change of QEMU's user at INFO, a plain redo at
// DEBUG.
func logPrepSuccess(dir, owner string, first, ownerChanged bool) {
	switch {
	case first:
		log.Info().Str("dir", dir).Str("qemu_user", owner).Msg("console socket directory ready; guests can start")
	case ownerChanged:
		log.Info().Str("dir", dir).Str("qemu_user", owner).Msg("console socket directory prepared again for QEMU's new user")
	default:
		log.Debug().Str("dir", dir).Str("qemu_user", owner).Msg("console socket directory checked again after a reconnect")
	}
}

// errCapsReadPending fails a preparation attempt while an earlier attempt's capabilities read
// is still unanswered.
var errCapsReadPending = errors.New("libvirt has not answered the previous capabilities read")

// attemptPrepare reads QEMU's user from libvirt's capabilities over conn, under the 1-second
// bound, and makes dir that user's. It records no outcome: the caller records a success.
//
// One read at a time. A read that misses its bound runs on until libvirt answers or the
// connection drops (withDeadline), so an attempt made meanwhile fails at once instead of sending
// another: a daemon that hangs holds one pending call, not one per retry.
func (m *VMManager) attemptPrepare(ctx context.Context, conn *libvirt.Libvirt, dir string) (uid, gid int, err error) {
	if conn == nil {
		return 0, 0, errNotConnected
	}
	if !m.capsReading.CompareAndSwap(false, true) {
		return 0, 0, errCapsReadPending
	}
	caps, err := withDeadline(ctx, func() (string, error) {
		defer m.capsReading.Store(false)
		return conn.ConnectGetCapabilities()
	})
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

// ensurePrepared lets a guest start go ahead once the console socket directory has been made
// QEMU's over the current connection. If it has not — never yet, or not since a reconnect — the
// start makes one attempt, or takes the result of the one in flight, bounded at 1 second, and is
// refused with ErrConsoleSocketDirNotReady naming the directory and the cause when that fails
// (SPEC-031 FR-013, FR-015, FR-016). An earlier success over another connection does not let
// it through: libvirt may have come back running QEMU as another user, and the directories
// would still be the previous one's.
func (m *VMManager) ensurePrepared(ctx context.Context) error {
	if conn := m.conn.Load(); conn != nil && m.preparedFor.Load() == conn {
		return nil
	}
	if err := m.prepareShared(ctx); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrConsoleSocketDirNotReady, paths.ConsoleSocketDir(), err)
	}
	return nil
}

// Ready is real-mode readiness's hypervisor probe (SPEC-031 FR-017): ErrConsoleSocketDirNotReady
// until the console socket directory has been prepared once, nil from then on — through later
// libvirt outages too, since the first success is never withdrawn. It reads recorded state and
// makes no libvirt call.
func (m *VMManager) Ready(context.Context) error {
	if !m.prepared.Load() {
		return ErrConsoleSocketDirNotReady
	}
	return nil
}

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
// The mode is set explicitly because MkdirAll leaves an existing directory's alone. A file
// that is not a directory, where dir belongs, is refused (MkdirAll fails on it) rather than
// chowned in the directory's place.
func ensureOwnedDir(dir string, uid, gid int) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(dir), err)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
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
// libvirtd answers them. Their number is bounded too: the console socket directory's
// preparation keeps one read pending at most (attemptPrepare), console issuance times out
// as well, so no new tokens appear, and the tokens the sweep checks expire within the TTL.
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
