package console

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestStore gives a store whose projection lives in a temp dir, so the
// on-disk half is exercised for real without touching /var/lib/lightstack.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	return NewStore(testPool, t.TempDir(), 10*time.Minute)
}

func readProjection(t *testing.T, dir, token string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, token))
	if err != nil {
		t.Fatalf("reading projection for %s: %v", token, err)
	}
	return string(b)
}

// The projection line is a wire format dictated by websockify's parser:
// re.split(r':\s', line) then rsplit(':', 1), and a first half of "unix_socket"
// makes the second a socket path (websocketproxy.validate_connection). Getting it
// wrong means every token silently fails to resolve, so pin it exactly.
func TestProjectionLineFormat(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(testPool, dir, time.Minute)

	if err := s.writeProjection("abc123", testSocket); err != nil {
		t.Fatalf("writeProjection: %v", err)
	}

	got := readProjection(t, dir, "abc123")
	if want := "abc123: unix_socket:" + testSocket + "\n"; got != want {
		t.Errorf("projection = %q; want %q", got, want)
	}
}

// The socket path is written into a line websockify splits on ": " and on its last
// ":", and it ends up in a sockaddr_un. Anything that would break either is refused
// at issue time rather than failing, silently, when a browser connects.
func TestProjectionRejectsUnsafeSocketPaths(t *testing.T) {
	s := NewStore(testPool, t.TempDir(), time.Minute)

	for _, bad := range []string{
		"",
		"vnc.sock",
		"/var/lib/libvirt/qemu/../escape/vnc.sock",
		"/var/lib/libvirt/qemu/a:b/vnc.sock",
		"/var/lib/libvirt/qemu/a b/vnc.sock",
		"/var/lib/libvirt/qemu/a\n/vnc.sock",
		"/" + strings.Repeat("a", 120),
	} {
		if err := s.writeProjection("abc123", bad); err == nil {
			t.Errorf("writeProjection(%q) should have been rejected", bad)
		}
	}
}

// A token containing ": " would corrupt websockify's parse. Tokens are hex, so
// this can only regress via a change to token generation — catch it here.
func TestGeneratedTokensAreParserSafe(t *testing.T) {
	for range 50 {
		tok, err := newToken()
		if err != nil {
			t.Fatalf("newToken: %v", err)
		}
		if strings.ContainsAny(tok, ": \t\n#") {
			t.Fatalf("token %q contains a character that breaks the projection parser", tok)
		}
		if len(tok) < 32 {
			t.Errorf("token %q is shorter than 32 hex chars — too guessable", tok)
		}
	}
}

// Issuance must fail closed: if the projection cannot be written, we must not
// hand out a token, because a token that exists only in the database can be
// neither resolved nor revoked through the directory the proxy reads.
func TestWriteProjectionFailsClosedOnUnwritableDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist")
	s := NewStore(testPool, dir, time.Minute)

	if err := s.writeProjection("abc123", testSocket); err == nil {
		t.Fatal("expected an error writing into a missing directory")
	}
}

// Only the chart's initContainer pre-creates the projection directory. Anywhere
// else — systemd, the deploy scripts — it would be missing, and every Issue would
// then fail closed as above, so real-mode startup creates it.
func TestEnsureDirCreatesProjectionDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "lightstack", "console-tokens")
	s := NewStore(testPool, dir, time.Minute)

	if err := s.EnsureDir(); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("projection directory not created: %v", err)
	}
	if err := s.writeProjection("abc123", testSocket); err != nil {
		t.Errorf("writeProjection after EnsureDir: %v", err)
	}
	// Idempotent: on the chart the initContainer has already created it.
	if err := s.EnsureDir(); err != nil {
		t.Errorf("EnsureDir on an existing directory: %v", err)
	}
}

// Revocation is an unlink. It must be idempotent — a reconciler sweep racing a
// lifecycle revoke will try to remove the same entry twice.
func TestRemoveProjectionIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(testPool, dir, time.Minute)

	if err := s.writeProjection("abc123", testSocket); err != nil {
		t.Fatalf("writeProjection: %v", err)
	}
	if err := s.removeProjection("abc123"); err != nil {
		t.Fatalf("first remove: %v", err)
	}
	if err := s.removeProjection("abc123"); err != nil {
		t.Errorf("second remove should be a no-op, got: %v", err)
	}
}

// The projection filename is the token, so a token containing a path separator
// would let a caller aim the write elsewhere. Tokens are generated, not user
// input, but defence in depth is cheap here.
func TestProjectionRejectsUnsafeTokenNames(t *testing.T) {
	s := NewStore(testPool, t.TempDir(), time.Minute)

	for _, bad := range []string{"../escape", "a/b", "", "."} {
		if err := s.writeProjection(bad, testSocket); err == nil {
			t.Errorf("writeProjection(%q) should have been rejected", bad)
		}
	}
}

func TestIssueAndResolve(t *testing.T) {
	requireDB(t)
	ctx := t.Context()
	s := newTestStore(t)
	inst, proj := seedInstance(t, ctx)

	tok, err := s.Issue(ctx, inst, proj, testSocket, testDomainID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// Both halves must exist: the row is the record, the file is what the proxy
	// actually reads.
	rec, err := s.Resolve(ctx, tok)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if rec.InstanceID != inst || rec.Socket != testSocket || rec.DomainID != testDomainID {
		t.Errorf("resolved %+v; want instance=%s socket=%s domain=%d", rec, inst, testSocket, testDomainID)
	}
	if got := readProjection(t, s.dir, tok); got != tok+": unix_socket:"+testSocket+"\n" {
		t.Errorf("projection = %q", got)
	}
}

// FR-010a: a link must survive repeated use inside its window, or a page
// refresh would break the console.
func TestResolveIsReusableWithinWindow(t *testing.T) {
	requireDB(t)
	ctx := t.Context()
	s := newTestStore(t)
	inst, proj := seedInstance(t, ctx)

	tok, err := s.Issue(ctx, inst, proj, testSocket, testDomainID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	for i := range 3 {
		if _, err := s.Resolve(ctx, tok); err != nil {
			t.Fatalf("Resolve #%d: %v", i+1, err)
		}
	}
}

func TestResolveRejectsExpired(t *testing.T) {
	requireDB(t)
	ctx := t.Context()
	// A negative TTL issues an already-expired token without a sleep.
	s := NewStore(testPool, t.TempDir(), -time.Minute)
	inst, proj := seedInstance(t, ctx)

	tok, err := s.Issue(ctx, inst, proj, testSocket, testDomainID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := s.Resolve(ctx, tok); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("err = %v; want ErrTokenNotFound for an expired token", err)
	}
}

func TestResolveRejectsUnknown(t *testing.T) {
	requireDB(t)
	ctx := t.Context()
	s := newTestStore(t)

	if _, err := s.Resolve(ctx, "definitely-not-a-real-token"); !errors.Is(err, ErrTokenNotFound) {
		t.Fatalf("err = %v; want ErrTokenNotFound", err)
	}
}

// Lifecycle revocation: every token of an instance goes when its console may have
// moved, and the projection goes with it. The socket path is the instance's own, so a
// surviving token could reach only the same guest's next run; this is what ends the
// links an action should end — and a row whose file outlived it would still be a live
// console.
func TestRevokeByInstanceRemovesBothHalves(t *testing.T) {
	requireDB(t)
	ctx := t.Context()
	s := newTestStore(t)
	instA, proj := seedInstance(t, ctx)

	tok, err := s.Issue(ctx, instA, proj, testSocket, testDomainID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// instance A stops -> every token of A is revoked before its port can be
	// handed to another domain.
	if err := s.RevokeByInstance(ctx, instA); err != nil {
		t.Fatalf("RevokeByInstance: %v", err)
	}

	if _, err := s.Resolve(ctx, tok); !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("resolve after revoke: err = %v; want ErrTokenNotFound", err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, tok)); !os.IsNotExist(err) {
		t.Error("projection entry outlived its row — the proxy would still resolve it")
	}
}

func TestRevokeExpiredReapsBothHalves(t *testing.T) {
	requireDB(t)
	ctx := t.Context()
	s := newTestStore(t)
	inst, proj := seedInstance(t, ctx)

	tok, err := s.Issue(ctx, inst, proj, testSocket, testDomainID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	backdate(t, ctx, tok)

	n, err := s.RevokeExpired(ctx)
	if err != nil {
		t.Fatalf("RevokeExpired: %v", err)
	}
	if n < 1 {
		t.Errorf("reaped %d tokens; want at least 1", n)
	}
	if _, err := os.Stat(filepath.Join(s.dir, tok)); !os.IsNotExist(err) {
		t.Error("expired projection entry survived the sweep — expiry is not enforced")
	}
}

// Deleting an instance must take its tokens with it. The FK cascade handles the
// rows; the sweep has to catch the orphaned files.
func TestDeletedInstanceCascadesAndSweeps(t *testing.T) {
	requireDB(t)
	ctx := t.Context()
	s := newTestStore(t)
	inst, proj := seedInstance(t, ctx)

	tok, err := s.Issue(ctx, inst, proj, testSocket, testDomainID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	deleteInstance(t, ctx, inst)

	if _, err := s.Resolve(ctx, tok); !errors.Is(err, ErrTokenNotFound) {
		t.Errorf("resolve after instance delete: err = %v; want ErrTokenNotFound", err)
	}
	if err := s.SweepOrphans(ctx); err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, tok)); !os.IsNotExist(err) {
		t.Error("cascade-deleted token left its projection behind — still resolvable by the proxy")
	}
}

// One unremovable projection must not block the rest, and must not get its row
// deleted while its file is still resolvable.
//
// Before the scoped delete, a single failing removeProjection returned before the
// DELETE, so NO row was deleted on that pass — and because the row survived, every
// later sweep re-selected the same token and failed the same way. The reaper could
// never make progress, and the reaper is what enforces expiry (FR-010).
//
// The poisoned token carries a "/", which validTokenFilename rejects, so its removal
// fails deterministically without needing to break the filesystem. The column is
// plain TEXT, so such a row is insertable even though Issue would never mint one.
func TestRevokeExpiredMakesProgressAroundAnUnremovableProjection(t *testing.T) {
	requireDB(t)
	ctx := t.Context()
	s := newTestStore(t)
	inst, proj := seedInstance(t, ctx)

	good, err := s.Issue(ctx, inst, proj, testSocket, testDomainID)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	backdate(t, ctx, good)

	const poisoned = "bad/token"
	if _, err := testPool.Exec(ctx,
		`INSERT INTO console_auth_tokens (token, instance_id, project_id, socket, domain_id, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		poisoned, inst, proj, testSocket, testDomainID, time.Now().Add(-time.Minute),
	); err != nil {
		t.Fatalf("seeding the poisoned row: %v", err)
	}

	n, err := s.RevokeExpired(ctx)
	if err == nil {
		t.Error("expected an error naming the unremovable projection")
	}

	// Progress: the healthy token is fully revoked despite its neighbour failing.
	if n != 1 {
		t.Errorf("revoked = %d; want 1 (the healthy token)", n)
	}
	if _, statErr := os.Stat(filepath.Join(s.dir, good)); !os.IsNotExist(statErr) {
		t.Error("healthy token's projection survived — one bad file blocked the batch")
	}
	if _, resolveErr := s.Resolve(ctx, good); !errors.Is(resolveErr, ErrTokenNotFound) {
		t.Errorf("healthy token still resolves: %v", resolveErr)
	}

	// Safety: the row we could NOT unlink is kept, so the next sweep retries it.
	// Deleting it here would strand a resolvable file with no record of it.
	var kept int
	if err := testPool.QueryRow(ctx,
		"SELECT count(*) FROM console_auth_tokens WHERE token = $1", poisoned).Scan(&kept); err != nil {
		t.Fatalf("counting the poisoned row: %v", err)
	}
	if kept != 1 {
		t.Error("the un-unlinkable token's row was deleted — its file would outlive its record")
	}
}

// RevokeStale is the sweep that catches every way a console moves without an API
// call: a guest powering itself off, a crash, a restart. The socket path is fixed per
// instance, so a restart shows as a new libvirt domain ID on the same path (FR-014a).
// It revokes on a definitive "different socket", "different run" or "none", and never
// on an error, since a libvirt blip must not take every console down, and a socket
// path is never another guest's.
func TestRevokeStale(t *testing.T) {
	requireDB(t)
	ctx := t.Context()
	s := newTestStore(t)

	issue := func(sock string, domainID int32) (string, string) {
		inst, proj := seedInstance(t, ctx)
		tok, err := s.Issue(ctx, inst, proj, sock, domainID)
		if err != nil {
			t.Fatalf("Issue: %v", err)
		}
		return inst, tok
	}
	const dir = "/var/lib/lightstack/console-sockets/"
	instSame, tokSame := issue(dir+"same.sock", 5)
	instRestarted, tokRestarted := issue(dir+"restarted.sock", 6)
	instMoved, tokMoved := issue("/var/lib/libvirt/qemu/domain-2-instance-moved/vnc.sock", 8)
	instGone, tokGone := issue(dir+"gone.sock", 9)
	instUnknown, tokUnknown := issue(dir+"unknown.sock", 10)

	current := func(_ context.Context, instanceID string) (string, int32, error) {
		switch instanceID {
		case instSame:
			return dir + "same.sock", 5, nil
		case instRestarted:
			return dir + "restarted.sock", 11, nil // same path, a new run (e.g. virsh start)
		case instMoved:
			return dir + "moved.sock", 12, nil // redefined into the console socket directory
		case instGone:
			return "", 0, nil // not running
		case instUnknown:
			return "", 0, errors.New("libvirt unreachable")
		default:
			// Tokens other packages' tests hold in the same database: not ours.
			return "", 0, errors.New("not this test's instance")
		}
	}

	n, err := s.RevokeStale(ctx, current)
	if err != nil {
		t.Fatalf("RevokeStale: %v", err)
	}
	if n != 3 {
		t.Errorf("revoked = %d; want 3 (the restarted, the moved and the stopped guest)", n)
	}
	for _, tok := range []string{tokSame, tokUnknown} {
		if _, err := s.Resolve(ctx, tok); err != nil {
			t.Errorf("token %s revoked; it must survive: %v", redact(tok), err)
		}
	}
	for _, tok := range []string{tokRestarted, tokMoved, tokGone} {
		if _, err := s.Resolve(ctx, tok); !errors.Is(err, ErrTokenNotFound) {
			t.Errorf("token %s still resolves; want ErrTokenNotFound, got %v", redact(tok), err)
		}
		if _, err := os.Stat(filepath.Join(s.dir, tok)); !os.IsNotExist(err) {
			t.Errorf("projection of %s survived its revoke", redact(tok))
		}
	}
}
