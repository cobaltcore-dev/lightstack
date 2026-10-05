// Package console owns the lifecycle of noVNC console tokens (SPEC-021 / F1).
//
// A console token is the only thing standing between a browser and a guest's
// framebuffer, so this package is deliberately self-contained and independently
// testable (Article I), and exposed to operators through `lightstack console-tokens`
// (Article II). internal/nova holds thin handlers that call in here.
//
// # Two halves, one source of truth
//
// The database table console_auth_tokens is the STORE OF RECORD. A directory of
// one-line files (<state_path>/console-tokens/<token> containing
// "<token>: unix_socket:<path>") is a PROJECTION of it, because that is the only
// interface websockify's TokenFile plugin offers. The path is the guest's VNC
// socket, the instance's own in <state_path>/console-sockets.
//
// The projection cannot carry an expiry, an instance or a project, which is why
// the table exists. It also means websockify performs NO expiry check of its
// own: a token dies when its file is removed, not when its expires_at passes.
// Reaping is therefore the enforcement mechanism, not housekeeping — see
// Reconciler.
//
// Every deletion removes the file BEFORE the row. The dangerous direction is a
// file outliving its record: that is a live console Lightstack no longer knows
// about, and nothing but this package can take it back.
package console

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
)

// ErrTokenNotFound is returned for a token that is unknown, expired, or whose
// instance is gone. The three are deliberately indistinguishable to callers.
var ErrTokenNotFound = errors.New("console token not found")

// Record is a resolved console token.
type Record struct {
	Token      string
	InstanceID string
	ProjectID  string
	Socket     string
	DomainID   int32 // the libvirt domain ID of the run it was issued for (FR-014a)
	CreatedAt  time.Time
	ExpiresAt  time.Time
}

// Store issues, resolves and revokes console tokens, keeping the projection
// directory in step with the table.
type Store struct {
	pool *pgxpool.Pool
	dir  string
	ttl  time.Duration
}

// NewStore returns a Store keeping its records in pool and writing its projection
// into dir with the given token lifetime.
func NewStore(pool *pgxpool.Pool, dir string, ttl time.Duration) *Store {
	return &Store{pool: pool, dir: dir, ttl: ttl}
}

// Dir is the projection directory the console proxy reads.
func (s *Store) Dir() string { return s.dir }

// EnsureDir creates the projection directory. On the chart the initContainer
// already has; anywhere else nothing would, and every Issue would fail closed.
//
// 0755 although an entry's name is its token: MkdirAll gives missing parents the
// same mode, and qemu — not root — must traverse the storage root to reach its
// disks. 0700 would protect nothing anyway: root on the node can open a guest's
// console socket without any token.
func (s *Store) EnsureDir() error {
	return os.MkdirAll(s.dir, 0o755)
}

// TTL is the configured token lifetime.
func (s *Store) TTL() time.Duration { return s.ttl }

// newToken returns a 32-hex-character token. Hex is not incidental: the
// projection parser splits on ": ", so a token containing that sequence would
// corrupt the file it is written into.
func newToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating console token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Issue records a console token for an instance and projects it for the proxy.
//
// It fails closed: if the projection cannot be written, the row is rolled back
// and no token is returned. A token that exists only in the database can be
// neither used nor revoked through the directory the proxy actually reads, so
// handing one out would be worse than refusing.
func (s *Store) Issue(ctx context.Context, instanceID, projectID, socket string, domainID int32) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}

	// expires_at is computed by the DATABASE, not here. Resolve and RevokeExpired
	// both compare it against the database's now(), and the PostgreSQL Lightstack uses is
	// external — routinely a different host with a different clock. Writing this
	// deadline locally would let skew silently change every token's real lifetime,
	// and the unsafe direction is a fast app clock: tokens outlive their configured
	// TTL with nothing anywhere reporting it. A console link is a credential; one
	// clock owns its expiry. RETURNING it back means the log below states what was
	// actually stored.
	var expiresAt time.Time
	if err := s.pool.QueryRow(ctx,
		`INSERT INTO console_auth_tokens (token, instance_id, project_id, socket, domain_id, expires_at)
		 VALUES ($1, $2, $3, $4, $5, now() + make_interval(secs => $6))
		 RETURNING expires_at`,
		token, instanceID, projectID, socket, domainID, s.ttl.Seconds(),
	).Scan(&expiresAt); err != nil {
		return "", fmt.Errorf("recording console token: %w", err)
	}

	if err := s.writeProjection(token, socket); err != nil {
		// Roll back rather than leave a row the proxy can never honour.
		if _, delErr := s.pool.Exec(ctx, "DELETE FROM console_auth_tokens WHERE token = $1", token); delErr != nil {
			log.Error().Err(delErr).Str("instance_id", instanceID).
				Msg("failed to roll back console token after projection write failed")
		}
		return "", fmt.Errorf("projecting console token: %w", err)
	}

	// Opportunistic reap: cheap, indexed, and it keeps a busy instance's expired
	// entries from lingering between reconciler ticks.
	if err := s.revokeExpiredForInstance(ctx, instanceID); err != nil {
		log.Warn().Err(err).Str("instance_id", instanceID).Msg("opportunistic console-token reap failed")
	}

	log.Info().
		Str("instance_id", instanceID).
		Str("project_id", projectID).
		Str("socket", socket).
		Time("expires_at", expiresAt).
		Msg("console token issued")

	return token, nil
}

// Resolve looks up a live token. Expired tokens are reported as not found.
func (s *Store) Resolve(ctx context.Context, token string) (*Record, error) {
	var r Record
	err := s.pool.QueryRow(ctx,
		`SELECT token, instance_id, project_id, socket, domain_id, created_at, expires_at
		   FROM console_auth_tokens
		  WHERE token = $1 AND expires_at > now()`,
		token,
	).Scan(&r.Token, &r.InstanceID, &r.ProjectID, &r.Socket, &r.DomainID, &r.CreatedAt, &r.ExpiresAt)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrTokenNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("resolving console token: %w", err)
	}
	return &r, nil
}

// RevokeByInstance removes every token of an instance.
//
// Called on every operation that can move or free a console endpoint — stop,
// start, reboot, suspend, resume, shelve, delete, forceDelete — so a link never
// outlives the run of the guest it was issued for. The socket path is the
// instance's own, so a link can never reach another guest; this keeps the links
// honest, and RevokeStale catches the restarts that never go through the API.
//
// Not serialized with Issue: a token whose row lands before this SELECT but whose
// file is written after the unlink survives as a file with no row. It names the
// same guest's socket, never another's, and SweepOrphans removes it on the next
// sweep, so the window is one reconcile interval for that guest alone.
//
// A token issued wholly after the revoke, from a readback taken before the action,
// survives it as well. RevokeStale removes it within one interval once the guest
// stops or starts a new run; a soft reboot keeps the run, so such a token lives to
// its TTL. Either way it reaches the same guest, never another.
func (s *Store) RevokeByInstance(ctx context.Context, instanceID string) error {
	n, err := s.revokeWhere(ctx, "instance_id = $1", instanceID)
	if err == nil && n > 0 {
		log.Info().Str("instance_id", instanceID).Int("revoked", n).
			Msg("console tokens revoked")
	}
	return err
}

// RevokeExpired reaps expired tokens and returns how many were removed.
//
// This is what actually enforces expiry (FR-010): the proxy resolves whatever it
// finds on disk and never checks a clock.
func (s *Store) RevokeExpired(ctx context.Context) (int, error) {
	return s.revokeWhere(ctx, "expires_at <= now()")
}

// CurrentSocket reports the console socket an instance's guest serves right now and
// the libvirt domain ID of its current run: "" when it serves none (not running, no
// console, a TCP listener), an error when that cannot be determined.
type CurrentSocket func(ctx context.Context, instanceID string) (socket string, domainID int32, err error)

// RevokeStale revokes every live token whose guest no longer serves the socket the
// token names, or serves it from another run, and reports how many went. The socket
// path is fixed per instance, so a restart shows only as a new domain ID (FR-014a).
//
// Lifecycle revocation only sees the API. A guest that powers itself off, crashes
// or restarts outside Lightstack moves its console without a call to revoke on, so
// this compares each token with what libvirt reports now — the server-side form of
// upstream Nova's validate_console_port, which websockify cannot do at connect time.
//
// An error keeps that instance's tokens for the next sweep. A socket path is never
// another guest's, so a stale token reaches at most its own guest; revoking every
// console whenever libvirt is briefly unreachable would be the worse failure.
func (s *Store) RevokeStale(ctx context.Context, current CurrentSocket) (int, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT token, instance_id, socket, domain_id FROM console_auth_tokens WHERE expires_at > now()")
	if err != nil {
		return 0, fmt.Errorf("listing console tokens: %w", err)
	}
	type target struct {
		token, socket string
		domainID      int32
	}
	byInstance := make(map[string][]target)
	for rows.Next() {
		var t target
		var instanceID string
		if err := rows.Scan(&t.token, &instanceID, &t.socket, &t.domainID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scanning console token: %w", err)
		}
		byInstance[instanceID] = append(byInstance[instanceID], t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterating console tokens: %w", err)
	}

	var stale []string
	for instanceID, targets := range byInstance {
		socket, domainID, err := current(ctx, instanceID)
		if err != nil {
			log.Warn().Err(err).Str("instance_id", instanceID).
				Msg("console: cannot read the guest's console socket; keeping its tokens until the next sweep")
			continue
		}
		for _, t := range targets {
			if t.socket != socket || t.domainID != domainID {
				stale = append(stale, t.token)
			}
		}
	}
	if len(stale) == 0 {
		return 0, nil
	}

	n, err := s.revokeWhere(ctx, "token = ANY($1)", stale)
	if n > 0 {
		log.Info().Int("revoked", n).Msg("console tokens revoked: their guest no longer serves that console")
	}
	return n, err
}

// revokeExpiredForInstance is the opportunistic variant run at issue time.
func (s *Store) revokeExpiredForInstance(ctx context.Context, instanceID string) error {
	_, err := s.revokeWhere(ctx, "instance_id = $1 AND expires_at <= now()", instanceID)
	return err
}

// revokeWhere unlinks the projection of every token matching cond, then deletes the
// rows, and reports how many rows went.
//
// One routine rather than one per predicate: the ordering below is the only thing
// standing between a revoked token and a console that still resolves, so it exists
// once and every caller inherits it. cond is a compile-time literal at every call site, never
// caller input.
func (s *Store) revokeWhere(ctx context.Context, cond string, args ...any) (int, error) {
	rows, err := s.pool.Query(ctx,
		"SELECT token FROM console_auth_tokens WHERE "+cond, args...)
	if err != nil {
		return 0, fmt.Errorf("listing console tokens: %w", err)
	}
	var tokens []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scanning console token: %w", err)
		}
		tokens = append(tokens, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("iterating console tokens: %w", err)
	}

	// Files first, per token: the FILE is what makes a token resolvable — websockify
	// reads the projection directory and never queries this table — so "row gone,
	// file still there" is a live console Lightstack no longer knows about, while "file
	// gone, row still there" is merely a stale row. Unlink, then delete.
	//
	// One failure must not block the rest: a single unremovable file used to abort
	// the whole call before the DELETE, so no row was deleted on any retry and the
	// reaper could never make progress. Collect instead, and delete only what was
	// actually unlinked.
	unlinked := make([]string, 0, len(tokens))
	var failed int
	var firstErr error
	for _, t := range tokens {
		if err := s.removeProjection(t); err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
			log.Error().Err(err).Str("token", redact(t)).
				Msg("console token projection could not be removed; its row is kept so a later revoke retries it")
			continue
		}
		unlinked = append(unlinked, t)
	}

	// Scoped to the tokens whose files are gone, NOT to cond again. Re-evaluating
	// cond here would delete rows this call never unlinked — a token that expired
	// between the SELECT and the DELETE, or one issued concurrently — leaving the
	// file behind as a resolvable orphan.
	var revoked int
	if len(unlinked) > 0 {
		tag, err := s.pool.Exec(ctx,
			"DELETE FROM console_auth_tokens WHERE token = ANY($1)", unlinked)
		if err != nil {
			return 0, fmt.Errorf("deleting console tokens: %w", err)
		}
		revoked = int(tag.RowsAffected())
	}

	if failed > 0 {
		return revoked, fmt.Errorf("revoking console token projections: %d of %d failed, first: %w",
			failed, len(tokens), firstErr)
	}
	return revoked, nil
}

// SweepOrphans removes projection files with no surviving row.
//
// Rows disappear behind our back when an instance is deleted (the FK cascades),
// which would otherwise leave the file — and therefore a resolvable token —
// behind. Also catches anything an unclean shutdown stranded.
//
// The order of the two reads is load-bearing: DIRECTORY FIRST, then the table. A
// token issued after the ReadDir is not in entries, so it is left alone; one issued
// between the two reads is in live, so its file is kept. Querying first would open a
// window in which a freshly issued token's projection is deleted — which would make
// this sweep a source of broken consoles rather than a cleaner of stale ones.
func (s *Store) SweepOrphans(ctx context.Context) error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // nothing projected yet
		}
		return fmt.Errorf("reading console-token directory: %w", err)
	}

	live, err := s.liveTokens(ctx)
	if err != nil {
		return err
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		token := e.Name()
		if _, ok := live[token]; ok {
			continue
		}
		if err := s.removeProjection(token); err != nil {
			return fmt.Errorf("removing orphaned console token projection: %w", err)
		}
		log.Info().Str("token", redact(token)).Msg("removed orphaned console-token projection")
	}
	return nil
}

// liveTokens returns the set of every token still recorded, expired or not.
//
// Expiry is deliberately not filtered here: SweepOrphans is about rows that no longer
// exist, and RevokeExpired owns the expired ones. Treating an expired-but-present row
// as absent would have the two sweeps race over the same file for no gain.
func (s *Store) liveTokens(ctx context.Context) (map[string]struct{}, error) {
	rows, err := s.pool.Query(ctx, "SELECT token FROM console_auth_tokens")
	if err != nil {
		return nil, fmt.Errorf("listing console tokens: %w", err)
	}
	defer rows.Close()

	live := make(map[string]struct{})
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("scanning console token: %w", err)
		}
		live[t] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating console tokens: %w", err)
	}
	return live, nil
}

// List returns every live token, newest first. Backs `lightstack console-tokens list`.
func (s *Store) List(ctx context.Context) ([]Record, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT token, instance_id, project_id, socket, domain_id, created_at, expires_at
		   FROM console_auth_tokens
		  WHERE expires_at > now()
		  ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("listing console tokens: %w", err)
	}
	defer rows.Close()

	var out []Record
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.Token, &r.InstanceID, &r.ProjectID, &r.Socket, &r.DomainID, &r.CreatedAt, &r.ExpiresAt); err != nil {
			return nil, fmt.Errorf("scanning console token: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating console tokens: %w", err)
	}
	return out, nil
}

// writeProjection creates the one-line file websockify's TokenFile plugin reads.
// "unix_socket" as the host half is what makes it connect to a socket path
// (websocketproxy.validate_connection) instead of a TCP endpoint.
func (s *Store) writeProjection(token, socket string) error {
	if err := validTokenFilename(token); err != nil {
		return err
	}
	if err := validSocketPath(socket); err != nil {
		return err
	}
	// 0600: the file names a reachable guest console. The proxy must be able to
	// read it, which is a deployment concern (matching UID, or a shared group) —
	// deliberately not solved by loosening the mode here.
	line := fmt.Sprintf("%s: unix_socket:%s\n", token, socket)
	return os.WriteFile(filepath.Join(s.dir, token), []byte(line), 0o600)
}

// maxSocketPath is the longest path a sockaddr_un holds on Linux: 108 bytes
// including the terminating NUL.
const maxSocketPath = 107

// validSocketPath keeps the target usable by websockify. Its parser splits the line
// on ": " and then on the LAST ":", so either in the path would corrupt the target,
// and a path the kernel cannot put in a sockaddr_un would fail only when a browser
// connects. The paths guests are given (<state_path>/console-sockets/<uuid>.sock, with
// state_path at most 50 bytes) trip none of these; this exists so that if that ever
// changes, issuance fails instead of the console.
func validSocketPath(p string) error {
	switch {
	case p == "" || !filepath.IsAbs(p) || filepath.Clean(p) != p:
		return fmt.Errorf("console socket %q is not a clean absolute path", p)
	case strings.ContainsAny(p, ": \t\n\r#"):
		return fmt.Errorf("console socket %q contains characters unsafe for the projection", p)
	case len(p) > maxSocketPath:
		return fmt.Errorf("console socket path is %d bytes; a unix socket path holds at most %d", len(p), maxSocketPath)
	}
	return nil
}

// removeProjection deletes a token's file. Absent is success: a reconciler sweep
// and a lifecycle revoke can race for the same entry.
func (s *Store) removeProjection(token string) error {
	if err := validTokenFilename(token); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(s.dir, token)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// validTokenFilename keeps the token usable as a filename. Tokens are generated
// hex so this cannot trigger in practice; it exists so that if that ever changes,
// the failure is an error rather than a write outside the directory.
func validTokenFilename(token string) error {
	if token == "" || token == "." || token == ".." {
		return fmt.Errorf("invalid console token name %q", token)
	}
	if strings.ContainsAny(token, `/\`) || strings.ContainsAny(token, ": \t\n#") {
		return fmt.Errorf("console token %q contains characters unsafe for the projection", redact(token))
	}
	return nil
}

// redact keeps token values out of logs in reusable form (FR-028) while leaving
// enough to correlate entries.
func redact(token string) string {
	if len(token) <= 8 {
		return "…"
	}
	return token[:8] + "…"
}
