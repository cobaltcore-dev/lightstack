// Package dbtest opens the test database for a package's TestMain, the same way for
// every database-backed package and on every backend.
//
// The backend follows the scheme of LIGHTSTACK_TEST_DB_URL. When the variable is unset
// the database-backed tests skip. When it is set but cannot be opened or migrated, the
// test binary exits: a misconfigured test database must fail the run, never skip it.
//
// On SQLite every package gets a fresh file of its own next to the one the URL names
// (see URL): go test runs packages in parallel processes, and they must not share one.
package dbtest

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database"
)

// Outcome compares the status codes several concurrent requests answered with what a case
// expects — keyed by status class ("2xx") or exact code ("404") — and returns "" when they match,
// or a description of what was answered when they do not.
func Outcome(codes []int, want map[string]int) string {
	got := map[string]int{}
	for _, c := range codes {
		k := strconv.Itoa(c)
		if c >= 200 && c < 300 {
			k = "2xx"
		}
		got[k]++
	}
	if maps.Equal(got, want) {
		return ""
	}
	var parts []string
	for k, n := range got {
		parts = append(parts, fmt.Sprintf("%d×%s", n, k))
	}
	sort.Strings(parts)
	return "answered " + strings.Join(parts, ", ")
}

// EnvURL names the variable that selects the test database.
const EnvURL = "LIGHTSTACK_TEST_DB_URL"

type config struct {
	before []func(database.DB) error
}

// Option adjusts Open.
type Option func(*config)

// Before runs fn after the database is opened and before it is migrated — for repairs a
// previous, interrupted run may have left behind.
func Before(fn func(database.DB) error) Option {
	return func(c *config) { c.before = append(c.before, fn) }
}

// URL returns this package's test database URL, or "" when LIGHTSTACK_TEST_DB_URL is
// unset. On PostgreSQL it is the variable itself. On SQLite it names the package's own
// file: sqlite:///tmp/ls/test.db becomes sqlite:///tmp/ls/test-internal-nova.db for the
// tests of internal/nova.
func URL() string {
	raw := os.Getenv(EnvURL)
	if raw == "" || Backend() != database.SQLite {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Path == "" {
		return raw // database.Open reports what is wrong with it
	}
	ext := filepath.Ext(u.Path)
	u.Path = strings.TrimSuffix(u.Path, ext) + "-" + packageTag() + ext
	return u.String()
}

// packageTag names the package under test by its directory relative to the module
// root, "internal/nova" → "internal-nova". go test runs a package's tests in its
// directory.
func packageTag() string {
	wd, err := os.Getwd()
	if err != nil {
		return "pkg"
	}
	for dir := wd; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			if rel, err := filepath.Rel(dir, wd); err == nil && rel != "." {
				return strings.ReplaceAll(filepath.ToSlash(rel), "/", "-")
			}
			break
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	return filepath.Base(wd)
}

// removeSQLiteFile deletes a previous run's database file with its WAL and shared
// memory files.
func removeSQLiteFile(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(u.Path + suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// Open opens and migrates the test database for a package's TestMain. ok is false when
// LIGHTSTACK_TEST_DB_URL is unset. On SQLite the package's file is created afresh.
func Open(migrationsPath string, opts ...Option) (db database.DB, ok bool) {
	url := URL()
	if url == "" {
		return nil, false
	}
	var c config
	for _, o := range opts {
		o(&c)
	}
	if Backend() == database.SQLite {
		resetSQLiteFile(url)
	}
	db = openOrExit(url)
	for _, fn := range c.before {
		if err := fn(db); err != nil {
			fmt.Fprintf(os.Stderr, "%s: preparing the test database failed: %v\n", EnvURL, err)
			db.Close()
			os.Exit(1)
		}
	}
	migrateOrExit(db, url, migrationsPath)
	return db, true
}

// resetSQLiteFile removes a previous run's SQLite file, exiting the test binary if it cannot: a
// stale file must fail the run, not be reused.
func resetSQLiteFile(url string) {
	if err := removeSQLiteFile(url); err != nil {
		fmt.Fprintf(os.Stderr, "%s: removing the previous test database failed: %v\n", EnvURL, err)
		os.Exit(1)
	}
}

// openOrExit opens the test database, exiting the test binary if it cannot be reached.
func openOrExit(url string) database.DB {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	db, err := database.Open(ctx, url, database.Options{Pool: database.PoolConfig{
		MaxConns:          5,
		MinConns:          1,
		MaxConnLifetime:   time.Hour,
		MaxConnIdleTime:   15 * time.Minute,
		HealthCheckPeriod: time.Minute,
	}})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s is set but connecting to it failed: %v\n", EnvURL, err)
		os.Exit(1)
	}
	return db
}

// migrateOrExit migrates the test database up, closing db and exiting the test binary if it cannot.
func migrateOrExit(db database.DB, url, migrationsPath string) {
	if err := database.MigrateUp(url, migrationsPath); err != nil {
		fmt.Fprintf(os.Stderr, "%s is set but migrations failed: %v\n", EnvURL, err)
		db.Close()
		os.Exit(1)
	}
}

// Close closes db at the end of TestMain. A pool whose Close does not return — a
// handler that leaked rows keeps a connection acquired, and the pool waits for it
// forever — fails the run with a diagnostic instead of hanging it.
func Close(db database.DB) {
	if db == nil {
		return
	}
	// A handler that leaked a Rows keeps a connection checked out. On SQLite, database/sql's
	// Close returns even then (the 30s guard below only trips for pgxpool), so this explicit
	// in-use check is the only leak signal there.
	//
	// A statement cancelled by its context deadline — the dbtest.Unavailable failure tests do
	// exactly that — hands its connection back to the pool asynchronously, so poll briefly for
	// the count to settle rather than reading it the instant the tests finish: a genuine leak
	// never clears, a cancellation still in flight does.
	if s, ok := db.(interface{ InUse() int }); ok {
		n := s.InUse()
		for deadline := time.Now().Add(5 * time.Second); n > 0 && time.Now().Before(deadline); n = s.InUse() {
			time.Sleep(20 * time.Millisecond)
		}
		if n > 0 {
			fmt.Fprintf(os.Stderr, "connection leak: %d connection(s) still in use at close — a handler did not close its rows\n", n)
			os.Exit(1)
		}
	}
	done := make(chan struct{})
	go func() {
		db.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		fmt.Fprintln(os.Stderr, "connection leak: the test database did not close within 30s — a handler did not close its rows")
		os.Exit(1)
	}
}

// Backend reports the backend the test database URL selects, or "" when it is unset or
// invalid.
func Backend() database.Backend {
	b, err := database.ParseURL(os.Getenv(EnvURL))
	if err != nil {
		return ""
	}
	return b
}

// RequirePostgres skips t unless the test database is PostgreSQL — for tests that
// inspect PostgreSQL itself (system catalogs, PL/pgSQL, driver errors).
func RequirePostgres(t testing.TB) {
	t.Helper()
	if Backend() != database.Postgres {
		t.Skip("PostgreSQL-only test: set " + EnvURL + " to a postgres:// URL")
	}
}

// Unavailable makes every statement on table fail until release is called — a stand-in
// for a database that cannot answer — while statements on other tables still succeed.
//
// On PostgreSQL another connection holds the table in ACCESS EXCLUSIVE mode, so a
// statement on it waits out its context's deadline and fails: give the request one. On
// SQLite, where readers never wait for a writer, the table is renamed away and a
// statement on it fails at once with "no such table".
//
// On PostgreSQL every package's test binary shares one database, so these locks are not
// private: another package's statement on the table — including a cascading delete during
// its cleanup — queues behind the ACCESS EXCLUSIVE lock. A caller that locks several
// tables must therefore take them in foreign-key order, parent before child, matching the
// order a cascade touches them; otherwise its locks and a concurrent cascade can deadlock
// across packages, where Postgres cannot see the cycle because one edge lives in Go. A
// 10s lock_timeout bounds the wait so such a mistake fails fast instead of hanging the
// run. On SQLite each package has its own file, so none of this applies.
func Unavailable(t testing.TB, db database.DB, table string) (release func()) {
	t.Helper()
	ctx := context.Background()
	switch db.Backend() {
	case database.SQLite:
		hidden := table + "__unavailable"
		if _, err := db.Exec(ctx, "ALTER TABLE "+table+" RENAME TO "+hidden); err != nil {
			t.Fatalf("hide %s: %v", table, err)
		}
		return func() {
			if _, err := db.Exec(context.Background(), "ALTER TABLE "+hidden+" RENAME TO "+table); err != nil {
				t.Errorf("restore %s: %v", table, err)
			}
		}
	default:
		tx, err := db.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		// Bound the wait: without it a lock-order mistake (a caller taking several tables in
		// an order that crosses a concurrent cascade from another package on the shared
		// database) hangs until go test's 10-minute timeout. With it the LOCK fails in
		// seconds and the message below names the likely cause.
		if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '10s'"); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("set lock_timeout: %v", err)
		}
		if _, err := tx.Exec(ctx, "LOCK TABLE "+table+" IN ACCESS EXCLUSIVE MODE"); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("timed out waiting for the lock on %s, likely a lock-order cycle with another package sharing the Postgres test database: %v", table, err)
		}
		return func() { _ = tx.Rollback(context.Background()) }
	}
}

// TxLockModes are the ways a SQLite transaction can begin, which the concurrency tests
// compare to decide the default (FR-010, FR-039).
var TxLockModes = []string{"deferred", "immediate"}

// OpenWithTxLock opens another handle on this package's test database whose transactions
// begin in mode, closed when the test ends. It skips unless the test database is SQLite:
// PostgreSQL has no such choice.
func OpenWithTxLock(t testing.TB, mode string) database.DB {
	t.Helper()
	if Backend() != database.SQLite {
		t.Skip("transaction modes are SQLite's: set " + EnvURL + " to a sqlite:// URL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := database.Open(ctx, URL(), database.Options{TxLock: mode, MustExist: true})
	if err != nil {
		t.Fatalf("open the test database with %s transactions: %v", mode, err)
	}
	t.Cleanup(db.Close)
	return db
}

// ConcurrencyCase is one transaction of the inventory, exercised by concurrent requests.
// Run returns why the case failed, or "" when it held.
type ConcurrencyCase struct {
	Name string
	Run  func(t *testing.T, db database.DB) string
}

// RunConcurrencyMatrix runs every case once per transaction mode and logs the mode × case
// table the default is decided from (FR-039, research R4). It returns the results by
// mode, then case.
func RunConcurrencyMatrix(t *testing.T, cases []ConcurrencyCase) map[string]map[string]string {
	t.Helper()
	results := map[string]map[string]string{}
	for _, mode := range TxLockModes {
		results[mode] = map[string]string{}
		db := OpenWithTxLock(t, mode)
		for _, c := range cases {
			results[mode][c.Name] = c.Run(t, db)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\n| case | %s |\n|---|%s\n", strings.Join(TxLockModes, " | "), strings.Repeat("---|", len(TxLockModes)))
	for _, c := range cases {
		b.WriteString("| " + c.Name + " |")
		for _, mode := range TxLockModes {
			if r := results[mode][c.Name]; r == "" {
				b.WriteString(" pass |")
			} else {
				b.WriteString(" FAIL: " + r + " |")
			}
		}
		b.WriteString("\n")
	}
	t.Log(b.String())
	return results
}

// Concurrently runs fn(0) … fn(n-1) at the same moment and waits for all of them.
func Concurrently(n int, fn func(i int)) {
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			fn(i)
		}()
	}
	close(start)
	wg.Wait()
}
