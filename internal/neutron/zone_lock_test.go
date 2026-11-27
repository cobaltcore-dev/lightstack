package neutron

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// SPEC-033 R32, R51; constitution Article III: a network's connection-tracking zone is the lowest free one,
// read and taken under one lock. Two requests never compute the same zone and race for it, and a statement
// that fails is not run again — Article III: "Lightstack itself MUST NOT retry a failed statement … prevented
// by taking the write lock …, not by retrying". Every statement that gives a network its zone — a network's
// create, the auto-allocated topology's, an older network's first port — runs once, in a transaction that
// took the zone lock first.

// zoneLog records the statements that give a network its zone (those holding nextZone).
type zoneLog struct {
	mu                 sync.Mutex
	attempts, unlocked int
	fail               int // how many to fail with a unique violation, as if another request took the zone
}

// record counts a zone statement and reports whether it is to fail.
func (l *zoneLog) record(locked bool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.attempts++
	if !locked {
		l.unlocked++
	}
	if l.fail > 0 {
		l.fail--
		return true
	}
	return false
}

func (l *zoneLog) counts() (attempts, unlocked int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.attempts, l.unlocked
}

var errZoneTaken = &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint \"networks_ct_zone\""}

type zoneDB struct {
	database.DB
	log *zoneLog
}

func (d zoneDB) Exec(ctx context.Context, query string, args ...any) (database.Result, error) {
	if strings.Contains(query, nextZone) && d.log.record(false) {
		return nil, errZoneTaken
	}
	return d.DB.Exec(ctx, query, args...)
}

func (d zoneDB) Begin(ctx context.Context) (database.Tx, error) {
	tx, err := d.DB.Begin(ctx)
	return &zoneTx{Tx: tx, log: d.log}, err
}

type zoneTx struct {
	database.Tx
	log    *zoneLog
	locked bool
}

func (t *zoneTx) Exec(ctx context.Context, query string, args ...any) (database.Result, error) {
	if strings.Contains(query, "pg_advisory_xact_lock") && len(args) == 1 && args[0] == ctZoneLock {
		t.locked = true
	}
	if strings.Contains(query, nextZone) && t.log.record(t.locked) {
		return nil, errZoneTaken
	}
	return t.Tx.Exec(ctx, query, args...)
}

func TestZoneIsTakenUnderItsLockAndNotRetried(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}
	// expect checks the zone statements run since the log was made: want of them, none outside the lock.
	expect := func(t *testing.T, log *zoneLog, want int) {
		t.Helper()
		attempts, unlocked := log.counts()
		if attempts != want || unlocked != 0 {
			t.Errorf("%d zone statement(s), %d outside the zone lock; want %d, all under it", attempts, unlocked, want)
		}
	}

	t.Run("a network's create", func(t *testing.T) {
		r, svc := newNeutronTestRouterWithService()
		log := &zoneLog{}
		svc.pool = zoneDB{testPool, log}
		name := "spec033-zone-lock-" + uuid.NewString()[:6]
		t.Cleanup(func() { mustExec(t, "DELETE FROM networks WHERE name = $1", name) })
		if code, body := doReqBody(r, http.MethodPost, "/v2.0/networks", `{"network":{"name":"`+name+`"}}`, hdr); code != http.StatusCreated {
			t.Fatalf("create: code=%d body=%s; want 201", code, body)
		}
		expect(t, log, 1)

		log.fail = 1
		if code, body := doReqBody(r, http.MethodPost, "/v2.0/networks", `{"network":{"name":"`+name+`"}}`, hdr); code != http.StatusInternalServerError {
			t.Fatalf("create whose zone statement fails: code=%d body=%s; want 500, not a retry", code, body)
		}
		expect(t, log, 2)
	})

	t.Run("the auto-allocated topology's create", func(t *testing.T) {
		r, svc := newNeutronTestRouterWithService()
		svc.dhcp, svc.brManager = newFakeDHCP(), newFakeBridges()
		log := &zoneLog{fail: 1}
		svc.pool = zoneDB{testPool, log}
		cleanupAutoAllocated(t)
		if code, body := doReqBody(r, http.MethodPost, "/v2.0/auto-allocated-topology/"+seededProjectID, "", hdr); code != http.StatusInternalServerError {
			t.Fatalf("create whose zone statement fails: code=%d body=%s; want 500, not a retry", code, body)
		}
		expect(t, log, 1)

		postAutoAllocated(t, r, hdr, http.StatusCreated)
		expect(t, log, 2)
	})

	t.Run("an older network's zone", func(t *testing.T) {
		_, svc := newNeutronTestRouterWithService()
		network := seedNetwork(t, "spec033-zone-lock-old-"+uuid.NewString()[:6]) // no zone, as before migration 084
		subnet := seedSubnet(t, network, "spec033-zone-lock-old", "10.86.0.0/24", nil, 4, false)
		port, _ := seedPortWithIP(t, network, subnet)
		mustExec(t, "UPDATE ports SET binding_host_id = $1 WHERE id = $2", svc.nodes.Name, port)
		log := &zoneLog{fail: 1}
		svc.pool = zoneDB{testPool, log}
		if err := svc.assignMissingZones(ctx); err == nil {
			t.Fatal("a zone statement that fails: the zones were assigned; want the error, not a retry")
		}
		expect(t, log, 1)

		if err := svc.assignMissingZones(ctx); err != nil {
			t.Fatalf("assign the zones: %v", err)
		}
		expect(t, log, 2)
		var zone *int
		if err := testPool.QueryRow(ctx, "SELECT ct_zone FROM networks WHERE id = $1", network).Scan(&zone); err != nil || zone == nil {
			t.Errorf("the network's zone: %v, %v; want one", zone, err)
		}
	})
}
