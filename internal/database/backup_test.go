package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// seedVersion is the schema version seedSQLite records, as golang-migrate would.
const seedVersion = 74

// seedSQLite opens a database at path with a table of n rows and returns it open. It records a
// migration version the way golang-migrate does, so check sees a Lightstack database.
func seedSQLite(t *testing.T, path string, n int) DB {
	t.Helper()
	db := openTestSQLite(t, path, Options{})
	ctx := context.Background()
	for _, stmt := range []string{
		"CREATE TABLE schema_migrations (version uint64, dirty bool)",
		fmt.Sprintf("INSERT INTO schema_migrations (version, dirty) VALUES (%d, false)", seedVersion),
		"CREATE TABLE t (id INTEGER PRIMARY KEY, v TEXT NOT NULL)",
	} {
		if _, err := db.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= n; i++ {
		if _, err := db.Exec(ctx, "INSERT INTO t (id, v) VALUES ($1, $2)", i, strings.Repeat("x", 100)); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// keepWriting inserts rows through db until stop is called, then reports how many it wrote
// and the first error — the requests a serving Lightstack would be answering meanwhile.
func keepWriting(db DB, firstID int) (stop func() (int, error)) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	var wrote int
	var werr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		for id := firstID; ; id++ {
			select {
			case <-done:
				return
			default:
			}
			if _, err := db.Exec(context.Background(), "INSERT INTO t (id, v) VALUES ($1, 'w')", id); err != nil {
				werr = err
				return
			}
			wrote++
		}
	}()
	return func() (int, error) {
		close(done)
		wg.Wait()
		return wrote, werr
	}
}

// countRows reads the row count of t from a database file without converting it to WAL or
// leaving side files behind: a backup is inspected exactly as it was written.
func countRows(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM t").Scan(&n); err != nil {
		t.Fatalf("read back %s: %v", path, err)
	}
	return n
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// TestBackupWhileServing pins FR-001 / SC-001: a backup taken while another connection keeps
// writing is one self-contained file holding at least the rows committed before it began, and
// the writer is never interrupted.
func TestBackupWhileServing(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "db", "lightstack.db")
	db := seedSQLite(t, src, 2000)
	stop := keepWriting(db, 1_000_000)

	dest := filepath.Join(dir, "backups", "b.db")
	err := Backup(context.Background(), sqliteURL(src), dest)
	wrote, werr := stop()
	if err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if werr != nil {
		t.Fatalf("the writer failed during the backup after %d rows: %v", wrote, werr)
	}
	for _, side := range []string{dest + "-wal", dest + "-shm", dest + "-journal"} {
		if exists(side) {
			t.Errorf("the backup left %s beside it; a backup is one file", side)
		}
	}
	if n := countRows(t, dest); n < 2000 {
		t.Errorf("backup holds %d rows; want at least the 2000 committed before it", n)
	}
	if _, err := IntegrityCheck(context.Background(), sqliteURL(dest)); err != nil {
		t.Errorf("the backup fails its integrity check: %v", err)
	}
}

// TestBackupRefusesAnExistingTarget pins FR-003: a backup never overwrites, and the refusal
// names the target.
func TestBackupRefusesAnExistingTarget(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "lightstack.db")
	seedSQLite(t, src, 3)
	dest := filepath.Join(dir, "taken.db")
	if err := os.WriteFile(dest, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := Backup(context.Background(), sqliteURL(src), dest)
	if err == nil || !strings.Contains(err.Error(), dest) || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("Backup over an existing file = %v; want a refusal naming %s", err, dest)
	}
	if b, _ := os.ReadFile(dest); string(b) != "keep" {
		t.Errorf("the existing target was modified: %q", b)
	}

	// Backing up onto the live database itself is the same refusal, not a corruption.
	if err := Backup(context.Background(), sqliteURL(src), src); err == nil {
		t.Fatal("Backup onto the live database succeeded")
	}
	if n := countRows(t, src); n != 3 {
		t.Errorf("live database holds %d rows after a refused backup onto it; want 3", n)
	}
}

// TestBackupModes pins FR-003a: a backup is private from the moment it exists — the file 0600
// and a directory it creates 0750 — whatever the umask, because it holds everything the live
// database does.
func TestBackupModes(t *testing.T) {
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })

	dir := t.TempDir()
	src := filepath.Join(dir, "lightstack.db")
	seedSQLite(t, src, 3)
	dest := filepath.Join(dir, "state", "backups", "b.db")
	if err := Backup(context.Background(), sqliteURL(src), dest); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	for path, want := range map[string]os.FileMode{dest: 0o600, filepath.Dir(dest): 0o750} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s mode = %04o; want %04o", path, got, want)
		}
	}
}

// A backup of a database that does not exist creates neither it nor the target: a mistyped URL
// must not leave an empty database or an empty "backup" behind.
func TestBackupOfAMissingDatabase(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "absent", "lightstack.db")
	dest := filepath.Join(dir, "b.db")
	if err := Backup(context.Background(), sqliteURL(src), dest); err == nil {
		t.Fatal("Backup of a missing database succeeded")
	}
	for _, p := range []string{src, dest} {
		if exists(p) {
			t.Errorf("a failed backup left %s behind", p)
		}
	}
}

// TestIntegrityCheck pins FR-005 on real files: an intact database passes; one with damaged
// pages fails, whichever way SQLite reports the damage.
func TestIntegrityCheck(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lightstack.db")
	db := seedSQLite(t, path, 5000)
	db.Close() // the last close checkpoints: the file alone now holds every row

	if v, err := IntegrityCheck(context.Background(), sqliteURL(path)); err != nil || v != seedVersion {
		t.Fatalf("IntegrityCheck on an intact database = %d, %v; want version %d", v, err, seedVersion)
	}

	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte(strings.Repeat("\xa5", 3*4096)), 5*4096); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if _, err := IntegrityCheck(context.Background(), sqliteURL(path)); err == nil {
		t.Fatal("IntegrityCheck passed a database with three overwritten pages")
	}
}

// integrity_check answers with rows, not a status: only a single "ok" is a pass, and every
// other row is a problem to report (research R6). The sqlite3 shell exits 0 either way.
func TestIntegrityResult(t *testing.T) {
	if err := integrityResult([]string{"ok"}); err != nil {
		t.Errorf("[ok] = %v; want a pass", err)
	}
	problems := []string{"row 7 missing from index t_v", "wrong # of entries in index t_v"}
	err := integrityResult(problems)
	if err == nil {
		t.Fatal("problem rows passed")
	}
	for _, p := range problems {
		if !strings.Contains(err.Error(), p) {
			t.Errorf("failure %q does not list %q", err, p)
		}
	}
	if err := integrityResult(nil); err == nil {
		t.Error("no rows at all passed; want a failure")
	}
}

// TestIntegrityCheckWhileServing pins the clarified FR-005: check reads a live database without
// stopping the writer.
func TestIntegrityCheckWhileServing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lightstack.db")
	db := seedSQLite(t, path, 2000)
	stop := keepWriting(db, 1_000_000)
	_, err := IntegrityCheck(context.Background(), sqliteURL(path))
	wrote, werr := stop()
	if err != nil {
		t.Fatalf("IntegrityCheck on a live database: %v", err)
	}
	if werr != nil {
		t.Fatalf("the writer failed during the check after %d rows: %v", wrote, werr)
	}
}

// TestBackupAndCheckRefusePostgres pins FR-006: both are SQLite-only, refuse a PostgreSQL URL by
// naming pg_dump, and touch nothing — no server is needed to be refused.
func TestBackupAndCheckRefusePostgres(t *testing.T) {
	const pg = "postgres://lightstack:secret@127.0.0.1:1/lightstack?sslmode=disable"
	dest := filepath.Join(t.TempDir(), "b.db")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for name, err := range map[string]error{
		"backup": Backup(ctx, pg, dest),
		"check":  checkErr(IntegrityCheck(ctx, pg)),
	} {
		if err == nil || !strings.Contains(err.Error(), "pg_dump") {
			t.Errorf("%s on PostgreSQL = %v; want a refusal naming pg_dump", name, err)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("%s tried to reach the server; it must refuse before connecting", name)
		}
	}
	if exists(dest) {
		t.Error("a refused backup created its target")
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRestoreProcedure pins FR-007 / SC-002, the order the documented restore prescribes. A
// Lightstack that stopped uncleanly leaves its write-ahead log beside the database, holding
// writes made after the backup. Put the backup in place with that log still there and SQLite
// replays it onto the backup; delete the log first and the backup's own data comes back.
func TestRestoreProcedure(t *testing.T) {
	dir := t.TempDir()
	live := filepath.Join(dir, "db", "lightstack.db")
	db := seedSQLite(t, live, 3)
	backup := filepath.Join(dir, "backups", "b.db")
	if err := Backup(context.Background(), sqliteURL(live), backup); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// Writes after the backup, still in the log: copy the log aside while the database is
	// open — what an unclean stop leaves on disk — then close.
	for id := 4; id <= 7; id++ {
		if _, err := db.Exec(context.Background(), "INSERT INTO t (id, v) VALUES ($1, 'after')", id); err != nil {
			t.Fatal(err)
		}
	}
	staleWAL := filepath.Join(dir, "stale-wal")
	copyFile(t, live+"-wal", staleWAL)
	db.Close()

	// Wrong order: the backup goes in, the old log stays.
	copyFile(t, backup, live)
	copyFile(t, staleWAL, live+"-wal")
	_ = os.Remove(live + "-shm")
	n, err := rowsVia(live)
	t.Logf("restore that kept the old log: %d rows, err %v", n, err)
	if err == nil && n == 3 {
		t.Fatal("a restore that kept the old log read back the backup unchanged; the test no longer reproduces the replay")
	}

	// The documented order: delete -wal and -shm, put the backup in place, check, start.
	for _, side := range []string{live + "-wal", live + "-shm"} {
		if err := os.Remove(side); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	copyFile(t, backup, live)
	if _, err := IntegrityCheck(context.Background(), sqliteURL(live)); err != nil {
		t.Fatalf("check after restore: %v", err)
	}
	if n, err := rowsVia(live); err != nil || n != 3 {
		t.Fatalf("restored database holds %d rows (%v); want the backup's 3", n, err)
	}
}

// rowsVia opens path the way the server does and counts t's rows.
func rowsVia(path string) (int, error) {
	db, err := Open(context.Background(), sqliteURL(path), Options{MustExist: true})
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var n int
	err = db.QueryRow(context.Background(), "SELECT count(*) FROM t").Scan(&n)
	return n, err
}

func checkErr(_ uint64, err error) error { return err }

// TestIntegrityCheckRefusesANonLightstackDatabase pins FR-005a, found by the host run: an empty
// file is a valid, empty SQLite database and passes integrity_check, so a failed .recover or a
// broken copy would pass the restore gate and Lightstack would start as a fresh install. check
// also requires the migration version Lightstack's schema records.
func TestIntegrityCheckRefusesANonLightstackDatabase(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.db")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(dir, "foreign.db")
	db := openTestSQLite(t, foreign, Options{})
	if _, err := db.Exec(context.Background(), "CREATE TABLE notes (body TEXT)"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	unversioned := filepath.Join(dir, "unversioned.db")
	db = openTestSQLite(t, unversioned, Options{})
	if _, err := db.Exec(context.Background(), "CREATE TABLE schema_migrations (version uint64, dirty bool)"); err != nil {
		t.Fatal(err)
	}
	db.Close()

	for _, path := range []string{empty, foreign, unversioned} {
		_, err := IntegrityCheck(context.Background(), sqliteURL(path))
		if err == nil || !strings.Contains(err.Error(), "no Lightstack schema") {
			t.Errorf("IntegrityCheck(%s) = %v; want a refusal saying it holds no Lightstack schema", filepath.Base(path), err)
		}
	}
}
