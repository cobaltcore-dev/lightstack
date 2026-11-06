package hypervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/pkg/paths"
)

// capsWith is a capabilities document carrying the given secmodels, shaped like the
// one libvirt reports (docs/formatcaps.rst): an AppArmor model next to the DAC one.
func capsWith(secmodels string) string {
	return "<capabilities><host><uuid>7b55704c-29f4-11b2-a85c-9dc6ff50623f</uuid>" +
		secmodels + "</host><guest><os_type>hvm</os_type></guest></capabilities>"
}

const apparmorModel = "<secmodel><model>apparmor</model><doi>0</doi></secmodel>"

func dacModel(labels string) string {
	return "<secmodel><model>dac</model><doi>0</doi>" + labels + "</secmodel>"
}

// QEMU's own uid:gid comes from the DAC security model's base label, which libvirt
// formats as "+uid:+gid". The kvm label is what a KVM domain runs as; the qemu label
// is the fallback when no kvm one is reported (SPEC-021 FR-017a).
func TestQEMUDACOwner(t *testing.T) {
	for _, tc := range []struct {
		name     string
		caps     string
		uid, gid int
		wantErr  bool
	}{
		{"libvirt's documented example", capsWith(apparmorModel + dacModel(
			"<baselabel type='kvm'>+77:+77</baselabel><baselabel type='qemu'>+77:+77</baselabel>")), 77, 77, false},
		{"kvm wins over qemu", capsWith(dacModel(
			"<baselabel type='qemu'>+1:+2</baselabel><baselabel type='kvm'>+64055:+108</baselabel>")), 64055, 108, false},
		{"qemu when no kvm label", capsWith(dacModel("<baselabel type='qemu'>+64055:+108</baselabel>")), 64055, 108, false},
		{"root", capsWith(dacModel("<baselabel type='kvm'>+0:+0</baselabel>")), 0, 0, false},
		{"no dac model", capsWith(apparmorModel), 0, 0, true},
		{"dac model without a label", capsWith(dacModel("")), 0, 0, true},
		{"not numeric", capsWith(dacModel("<baselabel type='kvm'>+libvirt-qemu:+kvm</baselabel>")), 0, 0, true},
		{"no separator", capsWith(dacModel("<baselabel type='kvm'>+64055</baselabel>")), 0, 0, true},
		{"not xml", "nope", 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uid, gid, err := qemuDACOwner(tc.caps)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("qemuDACOwner = %d:%d; want an error", uid, gid)
				}
				return
			}
			if err != nil {
				t.Fatalf("qemuDACOwner: %v", err)
			}
			if uid != tc.uid || gid != tc.gid {
				t.Errorf("owner = %d:%d; want %d:%d", uid, gid, tc.uid, tc.gid)
			}
		})
	}
}

// statOwnerMode reports a path's owner and permission bits.
func statOwnerMode(t *testing.T, path string) (uid, gid int, mode os.FileMode) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	return int(st.Uid), int(st.Gid), fi.Mode().Perm()
}

// The socket directory is QEMU's and closed to everyone else (0750). A missing parent is
// created traversable (0755), because QEMU must still reach its disks under the storage
// root. An existing directory with a wider mode is corrected, and one with another owner
// becomes QEMU's.
func TestEnsureOwnedDir(t *testing.T) {
	uid, gid := os.Getuid(), os.Getgid() // chown to oneself needs no privilege

	t.Run("missing, with a missing parent", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "state")
		dir := filepath.Join(root, "console-sockets")
		if err := ensureOwnedDir(dir, uid, gid); err != nil {
			t.Fatalf("ensureOwnedDir: %v", err)
		}
		if gotUID, gotGID, mode := statOwnerMode(t, dir); gotUID != uid || gotGID != gid || mode != 0o750 {
			t.Errorf("dir = %d:%d %o; want %d:%d 750", gotUID, gotGID, mode, uid, gid)
		}
		if _, _, mode := statOwnerMode(t, root); mode != 0o755 {
			t.Errorf("parent mode = %o; want 755 so QEMU can traverse it", mode)
		}
	})

	t.Run("existing and too wide", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "console-sockets")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := ensureOwnedDir(dir, uid, gid); err != nil {
			t.Fatalf("ensureOwnedDir: %v", err)
		}
		if _, _, mode := statOwnerMode(t, dir); mode != 0o750 {
			t.Errorf("mode = %o; want 750", mode)
		}
	})

	// The subtests above chown to this process's own owner, which a new directory already
	// has; this one asks for an owner it does not have, so the chown is seen.
	t.Run("existing and someone else's", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "console-sockets")
		if err := os.Mkdir(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		qemuUID, qemuGID := otherOwner(t)
		if err := ensureOwnedDir(dir, qemuUID, qemuGID); err != nil {
			t.Fatalf("ensureOwnedDir: %v", err)
		}
		if gotUID, gotGID, _ := statOwnerMode(t, dir); gotUID != qemuUID || gotGID != qemuGID {
			t.Errorf("owner = %d:%d; want %d:%d", gotUID, gotGID, qemuUID, qemuGID)
		}
	})
}

// The preparation reads QEMU's owner from libvirt and records the outcome: a prepared
// directory lets starts through, a failure keeps them refused and says why.
func TestPrepareConsoleSocketDir(t *testing.T) {
	label := fmt.Sprintf("<baselabel type='kvm'>+%d:+%d</baselabel>", os.Getuid(), os.Getgid())

	t.Run("prepared", func(t *testing.T) {
		useTempBase(t)
		m := newConnectedManagerWith(t, &fakeLibvirtd{caps: capsWith(apparmorModel + dacModel(label))})
		dir := paths.ConsoleSocketDir()
		if err := m.prepareShared(t.Context()); err != nil {
			t.Fatalf("prepareShared: %v", err)
		}
		if err := m.ensurePrepared(t.Context()); err != nil {
			t.Errorf("ensurePrepared after a successful prepare: %v", err)
		}
		if _, _, mode := statOwnerMode(t, dir); mode != 0o750 {
			t.Errorf("mode = %o; want 750", mode)
		}
	})

	// The label above names this process's own owner, which a new directory already has;
	// this one names an owner it does not have, so the chown to it is seen.
	t.Run("prepared for someone else", func(t *testing.T) {
		uid, gid := otherOwner(t)
		qemuLabel := fmt.Sprintf("<baselabel type='kvm'>+%d:+%d</baselabel>", uid, gid)
		useTempBase(t)
		m := newConnectedManagerWith(t, &fakeLibvirtd{caps: capsWith(apparmorModel + dacModel(qemuLabel))})
		dir := paths.ConsoleSocketDir()
		if err := m.prepareShared(t.Context()); err != nil {
			t.Fatalf("prepareShared: %v", err)
		}
		if gotUID, gotGID, _ := statOwnerMode(t, dir); gotUID != uid || gotGID != gid {
			t.Errorf("owner = %d:%d; want %d:%d", gotUID, gotGID, uid, gid)
		}
	})

	t.Run("no owner reported", func(t *testing.T) {
		useTempBase(t)
		m := newConnectedManagerWith(t, &fakeLibvirtd{caps: capsWith(apparmorModel)})
		setPrepared(m, false) // never prepared
		dir := paths.ConsoleSocketDir()
		if err := m.prepareShared(t.Context()); err == nil {
			t.Fatal("prepareShared succeeded without a DAC label to read the owner from")
		}
		err := m.ensurePrepared(t.Context())
		if !errors.Is(err, ErrConsoleSocketDirNotReady) {
			t.Fatalf("ensurePrepared = %v; want ErrConsoleSocketDirNotReady", err)
		}
		if !strings.Contains(err.Error(), dir) {
			t.Errorf("error %q does not name the directory", err)
		}
	})
}

// Until the socket directory is QEMU's, every path that starts a guest is refused before
// libvirt is asked anything that would fail inside it: no domain is defined or started,
// and a hard reboot never powers a guest off that it then could not start (FR-017a).
func TestStartsRefusedUntilConsoleDirReady(t *testing.T) {
	const id = "11111111-2222-3333-4444-555555555555"
	for _, tc := range guestStarts(id) {
		t.Run(tc.name, func(t *testing.T) {
			useTempBase(t)
			f := &fakeLibvirtd{active: true} // no capabilities: the on-demand attempt fails too
			m := newConnectedManagerWith(t, f)
			setPrepared(m, false) // never prepared
			dir := paths.ConsoleSocketDir()

			err := tc.start(m)
			if !errors.Is(err, ErrConsoleSocketDirNotReady) {
				t.Fatalf("err = %v; want ErrConsoleSocketDirNotReady", err)
			}
			if !strings.Contains(err.Error(), dir) || !strings.Contains(err.Error(), "capabilities") {
				t.Errorf("err %q; want it to name the directory and the cause (FR-016)", err)
			}
			for _, p := range f.domainCalls() {
				if p == procDomainCreate || p == procDomainDefineXML || p == procDomainDestroy {
					t.Errorf("libvirt was asked procedure %d before the refusal", p)
				}
			}
		})
	}
}

// isReady adapts the readiness probe to waitFor: true once Ready answers nil.
func isReady(m *VMManager) func() bool {
	return func() bool { return m.Ready(context.Background()) == nil }
}

// dacFor is a capabilities document naming uid:gid as the user QEMU runs KVM guests as.
func dacFor(uid, gid int) string {
	return capsWith(apparmorModel + dacModel(fmt.Sprintf("<baselabel type='kvm'>+%d:+%d</baselabel>", uid, gid)))
}

// countLines counts the log lines holding every one of parts.
func countLines(log string, parts ...string) int {
	n := 0
	for _, line := range strings.Split(log, "\n") {
		all := line != ""
		for _, p := range parts {
			all = all && strings.Contains(line, p)
		}
		if all {
			n++
		}
	}
	return n
}

// SPEC-031 FR-013, FR-017a, FR-018: a first capabilities read held past the 1-second bound —
// a socket-activated daemon probing QEMU on its first call — no longer disables guest starts
// until a restart. The preparation is retried until it succeeds, and the log says when.
func TestPreparationRetriesUntilLibvirtAnswers(t *testing.T) {
	dir := socketDir(t)
	useRunDir(t, dir)
	logs := captureLog(t)
	uid, gid := otherOwner(t)
	const id = "11111111-2222-3333-4444-555555555555"
	domainUUID, err := parseUUID(id)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeLibvirtd{caps: dacFor(uid, gid), capsDelay: 1500 * time.Millisecond, uuid: domainUUID, stopped: true}
	serveFakeOnSocket(t, filepath.Join(dir, "virtqemud-sock"), f)
	useTempBase(t)
	consoleDir := paths.ConsoleSocketDir()

	m, err := StartVMManager("qemu:///system", "real")
	if err != nil {
		t.Fatalf("StartVMManager: %v", err)
	}
	t.Cleanup(m.Close)
	if err := m.Ready(t.Context()); !errors.Is(err, ErrConsoleSocketDirNotReady) {
		t.Fatalf("Ready before libvirt answered: %v; want ErrConsoleSocketDirNotReady", err)
	}
	waitForWithin(t, "the preparation to succeed", 10*time.Second, isReady(m))

	if gotUID, gotGID, _ := statOwnerMode(t, consoleDir); gotUID != uid || gotGID != gid {
		t.Errorf("directory owner %d:%d; want %d:%d", gotUID, gotGID, uid, gid)
	}
	out := logs.String()
	if n := countLines(out, `"level":"error"`, "console socket directory"); n != 1 {
		t.Errorf("%d ERROR lines about the console socket directory; want exactly one, for the first failure:\n%s", n, out)
	}
	if countLines(out, `"level":"info"`, "console socket directory ready", consoleDir, fmt.Sprintf("%d:%d", uid, gid)) != 1 {
		t.Errorf("no INFO line naming the directory and QEMU's user on success:\n%s", out)
	}

	// The next guest start goes through, with no restart, into a directory QEMU owns (FR-018).
	if err := m.StartVM(context.Background(), id); err != nil {
		t.Fatalf("the next guest start: %v", err)
	}
	if gotUID, gotGID, _ := statOwnerMode(t, InstanceDir(id)); gotUID != uid || gotGID != gid {
		t.Errorf("instance directory owner %d:%d; want QEMU's %d:%d", gotUID, gotGID, uid, gid)
	}
	if !slices.Contains(f.domainCalls(), procDomainCreate) {
		t.Errorf("the guest was never started: calls %v", f.domainCalls())
	}
}

// SPEC-031 FR-013, FR-016 and the v1.5.0 exception: a guest start that finds an attempt in
// flight takes that attempt's result — one bounded wait — instead of waiting and then making
// an attempt of its own.
func TestStartJoinsInFlightAttempt(t *testing.T) {
	dir := socketDir(t)
	useRunDir(t, dir)
	f := &fakeLibvirtd{caps: dacFor(os.Getuid(), os.Getgid()), capsDelay: 3 * time.Second}
	serveFakeOnSocket(t, filepath.Join(dir, "virtqemud-sock"), f)
	useTempBase(t)

	m, err := StartVMManager("qemu:///system", "real")
	if err != nil {
		t.Fatalf("StartVMManager: %v", err)
	}
	t.Cleanup(m.Close)
	waitForWithin(t, "the loop's first attempt to be in flight", 10*time.Second, func() bool { return f.capsCalls() == 1 })

	start := time.Now()
	err = m.ensurePrepared(context.Background())
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("prepared while the only capabilities read was still held")
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("the start waited %s; want at most one 1-second bound", elapsed)
	}
	if n := f.capsCalls(); n != 1 {
		t.Errorf("%d capabilities reads; want the start to share the one in flight", n)
	}
}

// A capabilities read libvirt never answers, on a connection it keeps open, runs on past its
// bound until libvirt answers or the connection drops. The attempts made meanwhile must not send
// reads of their own: a daemon that hangs would otherwise collect one more pending call, and one
// more goroutine, with every retry for as long as it hangs. The read is released with the
// connection, and the next connection reads again.
func TestUnansweredCapabilitiesReadIsNotRepeated(t *testing.T) {
	useTempBase(t)
	logs := captureLog(t)
	f := &fakeLibvirtd{caps: dacFor(os.Getuid(), os.Getgid()), capsSilent: true}
	d := &pipeDialer{fake: f}
	m := &VMManager{mode: "real", prepares: true, stubVMs: make(map[string]*stubVM)}
	if err := m.connect(d, time.Millisecond, 5*time.Millisecond); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(m.Close)

	waitForWithin(t, "three failed attempts", 10*time.Second, func() bool {
		return countLines(logs.String(), "console socket directory") >= 3
	})
	if n := f.capsCalls(); n != 1 {
		t.Errorf("%d capabilities reads across three attempts; want only the one libvirt has not answered", n)
	}

	before := f.capsCalls()
	d.drop()
	waitForWithin(t, "a read on the next connection", 10*time.Second, func() bool { return f.capsCalls() > before })
}

// SPEC-031 FR-015, FR-017, FR-018a: the first success is never withdrawn — not by a lost
// connection, not by a redo that fails — so the readiness probe is ready after it and stays ready
// through a later loss of libvirt; and a redo that succeeds picks up a changed QEMU user.
func TestPreparedSurvivesDisconnect(t *testing.T) {
	useTempBase(t)
	d := &pipeDialer{fake: &fakeLibvirtd{caps: dacFor(os.Getuid(), os.Getgid())}}
	m := &VMManager{mode: "real", prepares: true, stubVMs: make(map[string]*stubVM)}
	if err := m.connect(d, time.Millisecond, 5*time.Millisecond); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(m.Close)
	waitForWithin(t, "the preparation", 10*time.Second, isReady(m))

	d.drop()
	if err := m.Ready(t.Context()); err != nil {
		t.Fatalf("a lost connection withdrew the preparation: %v", err)
	}
	waitForWithin(t, "the reconnect", 10*time.Second, func() bool { return d.dialCount() >= 2 })
	if err := m.Ready(t.Context()); err != nil {
		t.Fatalf("prepared lost across the reconnect: %v", err)
	}
}

func TestRedoAfterReconnect(t *testing.T) {
	uid, gid := otherOwner(t)
	f := &fakeLibvirtd{caps: dacFor(os.Getuid(), os.Getgid())}
	useTempBase(t)
	d := &pipeDialer{fake: f}
	m := &VMManager{mode: "real", prepares: true, stubVMs: make(map[string]*stubVM)}
	dir := paths.ConsoleSocketDir()
	if err := m.connect(d, time.Millisecond, 5*time.Millisecond); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(m.Close)
	waitForWithin(t, "the preparation", 10*time.Second, isReady(m))

	// libvirt comes back running QEMU as another user: the redo picks it up.
	f.setCaps(dacFor(uid, gid))
	d.drop()
	waitForWithin(t, "the redo to replace QEMU's user", 10*time.Second, func() bool {
		o := m.qemuOwner.Load()
		return o != nil && o.uid == uid && o.gid == gid
	})
	waitForWithin(t, "the directory to follow", 10*time.Second, func() bool {
		gotUID, gotGID, _ := statOwnerMode(t, dir)
		return gotUID == uid && gotGID == gid
	})

	// A redo that fails leaves readiness and QEMU's recorded user alone, but guest starts on
	// the new connection are refused until one succeeds: the user it would have read is unknown.
	before := f.capsCalls()
	f.setCaps(capsWith(apparmorModel)) // no DAC label: the redo cannot succeed
	d.drop()
	waitForWithin(t, "a failed redo", 10*time.Second, func() bool { return f.capsCalls() > before })
	if err := m.Ready(t.Context()); err != nil {
		t.Errorf("a failed redo withdrew the preparation: %v", err)
	}
	if o := m.qemuOwner.Load(); o == nil || o.uid != uid || o.gid != gid {
		t.Errorf("a failed redo replaced QEMU's user: %+v", o)
	}
	if err := m.ensurePrepared(t.Context()); !errors.Is(err, ErrConsoleSocketDirNotReady) {
		t.Errorf("a guest start after a failed redo: %v; want ErrConsoleSocketDirNotReady", err)
	}
}

// SPEC-031 FR-015, FR-016: a reconnect publishes its connection before the redo has read QEMU's
// user over it. A guest start in that window used to go ahead on the earlier success and make
// its instance directory the previous user's, so QEMU, running as the new one, could not write
// the guest's console.log. It now waits for the redo — joining it, within its 1-second bound —
// and makes the directory the new user's. Readiness stays ready throughout (FR-017).
func TestGuestStartWaitsForTheRedoAfterReconnect(t *testing.T) {
	uid, gid := otherOwner(t)
	f := &fakeLibvirtd{caps: dacFor(os.Getuid(), os.Getgid())}
	useTempBase(t)
	d := &pipeDialer{fake: f}
	m := &VMManager{mode: "real", prepares: true, stubVMs: make(map[string]*stubVM)}
	if err := m.connect(d, time.Millisecond, 5*time.Millisecond); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(m.Close)
	waitForWithin(t, "the preparation", 10*time.Second, isReady(m))

	// libvirt comes back running QEMU as another user, and answers the redo's read late.
	f.setCaps(dacFor(uid, gid))
	release := f.holdCaps(t)
	before := f.capsCalls()
	d.drop()
	waitForWithin(t, "the redo's capabilities read", 10*time.Second, func() bool { return f.capsCalls() > before })

	const id = "11111111-2222-3333-4444-555555555555"
	started := make(chan error, 1)
	go func() { // a guest start's two steps before QEMU launches (FR-013, FR-014)
		err := m.ensurePrepared(context.Background())
		if err == nil {
			err = m.PrepareInstanceDir(id)
		}
		started <- err
	}()
	select {
	case err := <-started:
		t.Fatalf("a guest start went ahead before the redo had read QEMU's user (err %v)", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := m.Ready(t.Context()); err != nil {
		t.Errorf("readiness withdrawn during the redo: %v", err)
	}

	release()
	if err := <-started; err != nil {
		t.Fatalf("the guest start once the redo succeeded: %v", err)
	}
	if gotUID, gotGID, _ := statOwnerMode(t, InstanceDir(id)); gotUID != uid || gotGID != gid {
		t.Errorf("instance directory owner %d:%d; want QEMU's new user %d:%d", gotUID, gotGID, uid, gid)
	}
}

// SPEC-031 FR-017a: the one ERROR line belongs to the preparation's first real failure. A guest
// start while libvirt is unreachable fails its attempt for want of a connection, which the
// connection loop reports itself. It must not use the line up, or the cause that keeps guests
// from starting once libvirt answers — here, no DAC label — would be logged at DEBUG only.
func TestPreparationErrorLineNotSpentOnMissingConnection(t *testing.T) {
	dir := socketDir(t)
	useRunDir(t, dir)
	useTempBase(t)
	logs := captureLog(t)

	m, err := StartVMManager("qemu:///system", "real")
	if err != nil {
		t.Fatalf("StartVMManager: %v", err)
	}
	t.Cleanup(m.Close)
	if err := m.ensurePrepared(context.Background()); !errors.Is(err, ErrConsoleSocketDirNotReady) {
		t.Fatalf("a start before libvirt is reachable: %v; want ErrConsoleSocketDirNotReady", err)
	}

	f := &fakeLibvirtd{caps: capsWith(apparmorModel)} // no DAC label: the preparation cannot succeed
	serveFakeOnSocket(t, filepath.Join(dir, "virtqemud-sock"), f)
	waitForWithin(t, "an ERROR line naming the missing DAC label", 10*time.Second, func() bool {
		return countLines(logs.String(), `"level":"error"`, "DAC base label") == 1
	})
	if n := countLines(logs.String(), `"level":"error"`, "not connected to libvirt"); n != 0 {
		t.Errorf("%d ERROR line(s) for the missing connection; the connection loop reports that:\n%s", n, logs.String())
	}
}

// A preparation whose directory cannot be made QEMU's fails and leaves guests refused: the state
// root is a regular file, or a regular file sits where the directory belongs. The second used to
// pass — the existing "directory" was taken as made, and the file was chowned instead.
func TestPreparationFailsWhenTheDirectoryCannotBeMade(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block func(t *testing.T) // puts a regular file in the way
	}{
		{"the state root is a file", func(t *testing.T) {
			if err := os.Remove(paths.Base()); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(paths.Base(), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{"a file where the directory belongs", func(t *testing.T) {
			if err := os.WriteFile(paths.ConsoleSocketDir(), nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useTempBase(t)
			tc.block(t)
			m := newConnectedManagerWith(t, &fakeLibvirtd{caps: dacFor(os.Getuid(), os.Getgid())})
			setPrepared(m, false) // never prepared

			if err := m.prepareShared(t.Context()); err == nil {
				t.Fatal("prepared a directory that could not be made")
			}
			if err := m.Ready(t.Context()); !errors.Is(err, ErrConsoleSocketDirNotReady) {
				t.Errorf("Ready = %v; want ErrConsoleSocketDirNotReady", err)
			}
		})
	}
}
