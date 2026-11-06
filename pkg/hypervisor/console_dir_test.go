package hypervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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

// PrepareConsoleSocketDir reads QEMU's owner from libvirt and records the outcome:
// a prepared directory lets starts through, a failure keeps them refused and says why.
func TestPrepareConsoleSocketDir(t *testing.T) {
	label := fmt.Sprintf("<baselabel type='kvm'>+%d:+%d</baselabel>", os.Getuid(), os.Getgid())

	t.Run("prepared", func(t *testing.T) {
		m := newConnectedManagerWith(t, &fakeLibvirtd{caps: capsWith(apparmorModel + dacModel(label))})
		dir := filepath.Join(t.TempDir(), "console-sockets")
		if err := m.PrepareConsoleSocketDir(t.Context(), dir); err != nil {
			t.Fatalf("PrepareConsoleSocketDir: %v", err)
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
		m := newConnectedManagerWith(t, &fakeLibvirtd{caps: capsWith(apparmorModel + dacModel(qemuLabel))})
		dir := filepath.Join(t.TempDir(), "console-sockets")
		if err := m.PrepareConsoleSocketDir(t.Context(), dir); err != nil {
			t.Fatalf("PrepareConsoleSocketDir: %v", err)
		}
		if gotUID, gotGID, _ := statOwnerMode(t, dir); gotUID != uid || gotGID != gid {
			t.Errorf("owner = %d:%d; want %d:%d", gotUID, gotGID, uid, gid)
		}
	})

	t.Run("no owner reported", func(t *testing.T) {
		m := newConnectedManagerWith(t, &fakeLibvirtd{caps: capsWith(apparmorModel)})
		m.prepared.Store(false) // never prepared
		dir := filepath.Join(t.TempDir(), "console-sockets")
		if err := m.PrepareConsoleSocketDir(t.Context(), dir); err == nil {
			t.Fatal("PrepareConsoleSocketDir succeeded without a DAC label to read the owner from")
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
	for _, tc := range []struct {
		name  string
		start func(m *VMManager) error
	}{
		{"create", func(m *VMManager) error { _, err := m.CreateVM(t.Context(), "<domain/>"); return err }},
		{"start", func(m *VMManager) error { return m.StartVM(t.Context(), id) }},
		{"hard reboot", func(m *VMManager) error { return m.RebootVM(t.Context(), id, true) }},
		{"resume", func(m *VMManager) error { return m.ResumeVM(t.Context(), id) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeLibvirtd{active: true} // no capabilities: the on-demand attempt fails too
			m := newConnectedManagerWith(t, f)
			m.prepared.Store(false) // never prepared
			dir := filepath.Join(t.TempDir(), "console-sockets")
			m.consoleDirPath.Store(&dir)

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
	serveFakeOnSocket(t, filepath.Join(dir, "virtqemud-sock"),
		&fakeLibvirtd{caps: dacFor(uid, gid), capsDelay: 1500 * time.Millisecond})
	consoleDir := filepath.Join(t.TempDir(), "console-sockets")

	m, err := StartVMManager("qemu:///system", "real", consoleDir)
	if err != nil {
		t.Fatalf("StartVMManager: %v", err)
	}
	t.Cleanup(m.Close)
	if m.ConsoleDirPrepared() {
		t.Fatal("prepared before libvirt answered")
	}
	waitForWithin(t, "the preparation to succeed", 10*time.Second, m.ConsoleDirPrepared)

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
}

// SPEC-031 FR-013, FR-016 and the v1.5.0 exception: a guest start that finds an attempt in
// flight takes that attempt's result — one bounded wait — instead of waiting and then making
// an attempt of its own.
func TestStartJoinsInFlightAttempt(t *testing.T) {
	dir := socketDir(t)
	useRunDir(t, dir)
	f := &fakeLibvirtd{caps: dacFor(os.Getuid(), os.Getgid()), capsDelay: 3 * time.Second}
	serveFakeOnSocket(t, filepath.Join(dir, "virtqemud-sock"), f)

	m, err := StartVMManager("qemu:///system", "real", filepath.Join(t.TempDir(), "console-sockets"))
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

// SPEC-031 FR-015, FR-017: the first success is never withdrawn — not by a lost connection,
// not by a redo that fails — and a redo that succeeds picks up a changed QEMU user.
func TestPreparedSurvivesDisconnect(t *testing.T) {
	d := &pipeDialer{fake: &fakeLibvirtd{caps: dacFor(os.Getuid(), os.Getgid())}}
	m := &VMManager{mode: "real", stubVMs: make(map[string]*stubVM)}
	dir := filepath.Join(t.TempDir(), "console-sockets")
	m.consoleDirPath.Store(&dir)
	if err := m.connect(d, time.Millisecond, 5*time.Millisecond); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(m.Close)
	waitForWithin(t, "the preparation", 10*time.Second, m.ConsoleDirPrepared)

	d.drop()
	if !m.ConsoleDirPrepared() {
		t.Fatal("a lost connection withdrew the preparation")
	}
	waitForWithin(t, "the reconnect", 10*time.Second, func() bool { return d.dialCount() >= 2 })
	if !m.ConsoleDirPrepared() {
		t.Fatal("prepared lost across the reconnect")
	}
}

func TestRedoAfterReconnect(t *testing.T) {
	uid, gid := otherOwner(t)
	f := &fakeLibvirtd{caps: dacFor(os.Getuid(), os.Getgid())}
	d := &pipeDialer{fake: f}
	m := &VMManager{mode: "real", stubVMs: make(map[string]*stubVM)}
	dir := filepath.Join(t.TempDir(), "console-sockets")
	m.consoleDirPath.Store(&dir)
	if err := m.connect(d, time.Millisecond, 5*time.Millisecond); err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(m.Close)
	waitForWithin(t, "the preparation", 10*time.Second, m.ConsoleDirPrepared)

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

	// A redo that fails changes nothing in effect.
	before := f.capsCalls()
	f.setCaps(capsWith(apparmorModel)) // no DAC label: the redo cannot succeed
	d.drop()
	waitForWithin(t, "a failed redo", 10*time.Second, func() bool { return f.capsCalls() > before })
	if !m.ConsoleDirPrepared() {
		t.Error("a failed redo withdrew the preparation")
	}
	if o := m.qemuOwner.Load(); o == nil || o.uid != uid || o.gid != gid {
		t.Errorf("a failed redo replaced QEMU's user: %+v", o)
	}
}
