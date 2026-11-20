package neutron

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/internal/database/dbtest"
	"github.com/google/uuid"
)

// SPEC-033 T003: the package-level helpers every membership path writes through (research R2).
// They run on the caller's transaction, so a compute action and a port update share one write path.

// sgTestPort inserts a port on a fresh network in the seeded project, with updated_at in the past so
// a write that moves it is visible. The network's cleanup removes the port.
func sgTestPort(t *testing.T) string {
	t.Helper()
	netID := seedNetwork(t, "spec033-sg-net-"+uuid.NewString()[:8])
	id := uuid.NewString()
	if _, err := testPool.Exec(context.Background(), `
		INSERT INTO ports (id, name, network_id, project_id, mac_address, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		id, "spec033-sg-port", netID, seededProjectID, "fa:16:3e:00:33:01",
		time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC),
	); err != nil {
		t.Fatalf("seed port: %v", err)
	}
	return id
}

// sgTestGroup inserts a security group named name in projectID, removed again by cleanup.
func sgTestGroup(t *testing.T, projectID, name string) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := testPool.Exec(context.Background(),
		"INSERT INTO security_groups (id, name, project_id, description) VALUES ($1, $2, $3, '')",
		id, name, projectID,
	); err != nil {
		t.Fatalf("seed security group %q: %v", name, err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), "DELETE FROM security_groups WHERE id = $1", id) //nolint:errcheck // best-effort cleanup
	})
	return id
}

func sgTestAttach(t *testing.T, portID string, groupIDs ...string) {
	t.Helper()
	for _, g := range groupIDs {
		if _, err := testPool.Exec(context.Background(),
			"INSERT INTO port_security_groups (port_id, security_group_id) VALUES ($1, $2)", portID, g,
		); err != nil {
			t.Fatalf("attach %s to %s: %v", g, portID, err)
		}
	}
}

func sgTestUpdatedAt(t *testing.T, portID string) time.Time {
	t.Helper()
	var at time.Time
	if err := testPool.QueryRow(context.Background(),
		"SELECT updated_at FROM ports WHERE id = $1", portID).Scan(&at); err != nil {
		t.Fatalf("read updated_at: %v", err)
	}
	return at
}

func sgTestGroupsOf(t *testing.T, portID string) []string {
	t.Helper()
	ids, err := PortSecurityGroupIDs(context.Background(), testPool, portID)
	if err != nil {
		t.Fatalf("PortSecurityGroupIDs: %v", err)
	}
	sort.Strings(ids)
	return ids
}

func sorted(ids ...string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

func equalIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var spec033Epoch = time.Date(2000, 1, 2, 0, 0, 0, 0, time.UTC)

func TestPortSecurityGroupHelpers(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	t.Run("PortSecurityGroupIDs lists the port's groups, and none as an empty list", func(t *testing.T) {
		port := sgTestPort(t)
		if ids := sgTestGroupsOf(t, port); len(ids) != 0 {
			t.Fatalf("a port with no group: got %v, want none", ids)
		}
		web := sgTestGroup(t, seededProjectID, "spec033-web-"+uuid.NewString()[:6])
		db := sgTestGroup(t, seededProjectID, "spec033-db-"+uuid.NewString()[:6])
		sgTestAttach(t, port, web, db)
		if got, want := sgTestGroupsOf(t, port), sorted(web, db); !equalIDs(got, want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("AddPortSecurityGroup inserts the row and moves updated_at", func(t *testing.T) {
		port := sgTestPort(t)
		web := sgTestGroup(t, seededProjectID, "spec033-web-"+uuid.NewString()[:6])
		if err := database.WithTx(ctx, testPool, func(tx database.Tx) error {
			return AddPortSecurityGroup(ctx, tx, port, web)
		}); err != nil {
			t.Fatalf("AddPortSecurityGroup: %v", err)
		}
		if got := sgTestGroupsOf(t, port); !equalIDs(got, []string{web}) {
			t.Fatalf("groups: got %v, want [%s]", got, web)
		}
		if at := sgTestUpdatedAt(t, port); !at.After(spec033Epoch) {
			t.Fatalf("updated_at did not move: %v", at)
		}
	})

	t.Run("RemovePortSecurityGroup deletes the row, moves updated_at and reports it", func(t *testing.T) {
		port := sgTestPort(t)
		web := sgTestGroup(t, seededProjectID, "spec033-web-"+uuid.NewString()[:6])
		db := sgTestGroup(t, seededProjectID, "spec033-db-"+uuid.NewString()[:6])
		sgTestAttach(t, port, web)

		var removed bool
		if err := database.WithTx(ctx, testPool, func(tx database.Tx) error {
			var err error
			removed, err = RemovePortSecurityGroup(ctx, tx, port, web)
			return err
		}); err != nil {
			t.Fatalf("RemovePortSecurityGroup: %v", err)
		}
		if !removed {
			t.Fatal("removing a group the port carried reported false")
		}
		if got := sgTestGroupsOf(t, port); len(got) != 0 {
			t.Fatalf("groups after remove: %v, want none", got)
		}
		if at := sgTestUpdatedAt(t, port); !at.After(spec033Epoch) {
			t.Fatalf("updated_at did not move: %v", at)
		}

		// A group the port does not carry: false, and no error.
		if err := database.WithTx(ctx, testPool, func(tx database.Tx) error {
			var err error
			removed, err = RemovePortSecurityGroup(ctx, tx, port, db)
			return err
		}); err != nil {
			t.Fatalf("RemovePortSecurityGroup of a group not carried: %v", err)
		}
		if removed {
			t.Fatal("removing a group the port did not carry reported true")
		}
	})

	t.Run("SetPortSecurityGroups replaces the list, the empty list included", func(t *testing.T) {
		port := sgTestPort(t)
		web := sgTestGroup(t, seededProjectID, "spec033-web-"+uuid.NewString()[:6])
		db := sgTestGroup(t, seededProjectID, "spec033-db-"+uuid.NewString()[:6])
		app := sgTestGroup(t, seededProjectID, "spec033-app-"+uuid.NewString()[:6])
		sgTestAttach(t, port, web, db)

		if err := database.WithTx(ctx, testPool, func(tx database.Tx) error {
			return SetPortSecurityGroups(ctx, tx, port, []string{db, app})
		}); err != nil {
			t.Fatalf("SetPortSecurityGroups: %v", err)
		}
		if got, want := sgTestGroupsOf(t, port), sorted(db, app); !equalIDs(got, want) {
			t.Fatalf("after set: got %v, want %v", got, want)
		}
		if at := sgTestUpdatedAt(t, port); !at.After(spec033Epoch) {
			t.Fatalf("updated_at did not move: %v", at)
		}

		if err := database.WithTx(ctx, testPool, func(tx database.Tx) error {
			return SetPortSecurityGroups(ctx, tx, port, nil)
		}); err != nil {
			t.Fatalf("SetPortSecurityGroups(empty): %v", err)
		}
		if got := sgTestGroupsOf(t, port); len(got) != 0 {
			t.Fatalf("after the empty set: %v, want none", got)
		}
	})

	t.Run("a group that no longer exists is the not-found error, not a foreign-key one", func(t *testing.T) {
		port := sgTestPort(t)
		gone := uuid.NewString() // as if deleted after it was resolved
		err := database.WithTx(ctx, testPool, func(tx database.Tx) error {
			return AddPortSecurityGroup(ctx, tx, port, gone)
		})
		if !errors.Is(err, ErrSecurityGroupNotFound) {
			t.Fatalf("AddPortSecurityGroup with a missing group: got %v, want ErrSecurityGroupNotFound", err)
		}
		err = database.WithTx(ctx, testPool, func(tx database.Tx) error {
			return SetPortSecurityGroups(ctx, tx, port, []string{gone})
		})
		if !errors.Is(err, ErrSecurityGroupNotFound) {
			t.Fatalf("SetPortSecurityGroups with a missing group: got %v, want ErrSecurityGroupNotFound", err)
		}
	})

	t.Run("a rolled-back transaction leaves the rows unchanged", func(t *testing.T) {
		port := sgTestPort(t)
		web := sgTestGroup(t, seededProjectID, "spec033-web-"+uuid.NewString()[:6])
		db := sgTestGroup(t, seededProjectID, "spec033-db-"+uuid.NewString()[:6])
		sgTestAttach(t, port, web)

		errRollback := errors.New("roll back")
		err := database.WithTx(ctx, testPool, func(tx database.Tx) error {
			if err := AddPortSecurityGroup(ctx, tx, port, db); err != nil {
				return err
			}
			if _, err := RemovePortSecurityGroup(ctx, tx, port, web); err != nil {
				return err
			}
			return errRollback
		})
		if !errors.Is(err, errRollback) {
			t.Fatalf("WithTx: got %v, want the rollback error", err)
		}
		if got := sgTestGroupsOf(t, port); !equalIDs(got, []string{web}) {
			t.Fatalf("after rollback: got %v, want [%s]", got, web)
		}
	})
}

func TestResolveSecurityGroup(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	web := sgTestGroup(t, seededProjectID, "spec033-resolve-"+uuid.NewString()[:6])
	var webName string
	if err := testPool.QueryRow(ctx, "SELECT name FROM security_groups WHERE id = $1", web).Scan(&webName); err != nil {
		t.Fatalf("read name: %v", err)
	}

	t.Run("by name and by id", func(t *testing.T) {
		for _, ref := range []string{webName, web} {
			got, err := ResolveSecurityGroup(ctx, testPool, ref, seededProjectID, false)
			if err != nil || got != web {
				t.Fatalf("resolve %q: got %q, %v; want %q", ref, got, err, web)
			}
		}
	})

	t.Run("an id match wins over a group named after that id", func(t *testing.T) {
		impostor := sgTestGroup(t, seededProjectID, web) // named after web's id
		got, err := ResolveSecurityGroup(ctx, testPool, web, seededProjectID, false)
		if err != nil || got != web {
			t.Fatalf("resolve %q: got %q, %v; want the group with that id (%q), not %q", web, got, err, web, impostor)
		}
	})

	other := createTestProject(t, "spec033-other")
	foreignName := "spec033-foreign-" + uuid.NewString()[:6]
	foreign := sgTestGroup(t, other, foreignName)

	t.Run("a member: another project's group, by name or by id, is not found", func(t *testing.T) {
		for _, ref := range []string{foreignName, foreign} {
			if _, err := ResolveSecurityGroup(ctx, testPool, ref, seededProjectID, false); !errors.Is(err, ErrSecurityGroupNotFound) {
				t.Fatalf("resolve %q from the seeded project: got %v, want ErrSecurityGroupNotFound", ref, err)
			}
		}
	})

	// SPEC-033 round 2, T105 (FR-004, FR-018; research R52): an administrator's id names any project's
	// group, as upstream's; a name still resolves only in the project given (register entry 5).
	t.Run("an administrator: another project's group by id resolves, by name it does not", func(t *testing.T) {
		if got, err := ResolveSecurityGroup(ctx, testPool, foreign, seededProjectID, true); err != nil || got != foreign {
			t.Fatalf("resolve another project's id as an administrator: got %q, %v; want %q", got, err, foreign)
		}
		if _, err := ResolveSecurityGroup(ctx, testPool, foreignName, seededProjectID, true); !errors.Is(err, ErrSecurityGroupNotFound) {
			t.Fatalf("resolve another project's name as an administrator: got %v, want ErrSecurityGroupNotFound", err)
		}
	})

	t.Run("a malformed id is not found, not a query error", func(t *testing.T) {
		if _, err := ResolveSecurityGroup(ctx, testPool, "not-a-uuid", seededProjectID, true); !errors.Is(err, ErrSecurityGroupNotFound) {
			t.Fatalf("resolve a malformed id: got %v, want ErrSecurityGroupNotFound", err)
		}
	})
}

// sgTestInstance inserts an instance in the seeded project, removed again by cleanup.
func sgTestInstance(t *testing.T) string {
	t.Helper()
	id := uuid.NewString()
	if _, err := testPool.Exec(context.Background(),
		"INSERT INTO instances (id, name, project_id) VALUES ($1, $2, $3)", id, "spec033-alloc-"+id[:8], seededProjectID,
	); err != nil {
		t.Fatalf("seed instance: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(context.Background(), "DELETE FROM instances WHERE id = $1", id) //nolint:errcheck // best-effort cleanup
	})
	return id
}

// SPEC-033 T025 (FR-011, FR-012; research R11): a boot's first port carries the groups the create
// resolved, default when none; each later port of the same boot copies the first port's current
// groups, so a change accepted between the two reaches the later one.
func TestAllocatePortForInstanceGroups(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	svc := NewService(testPool, "stub", nil)
	netID := seedNetwork(t, "spec033-alloc-"+uuid.NewString()[:8])
	seedSubnet(t, netID, "spec033-alloc-subnet", "10.34.0.0/24", nil, 4, true)
	defaultID, err := EnsureDefaultSecurityGroup(ctx, testPool, seededProjectID)
	if err != nil {
		t.Fatal(err)
	}

	alloc := func(instance string, groups []string, copyFrom string) (string, error) {
		info, err := svc.AllocatePortForInstance(ctx, netID, seededProjectID, instance, groups, copyFrom, false)
		if err != nil {
			return "", err
		}
		return info.(*PortInfo).ID, nil
	}
	mustAlloc := func(t *testing.T, instance string, groups []string, copyFrom string) string {
		t.Helper()
		id, err := alloc(instance, groups, copyFrom)
		if err != nil {
			t.Fatalf("AllocatePortForInstance: %v", err)
		}
		return id
	}

	t.Run("the first port carries the given groups, and default when none", func(t *testing.T) {
		instance := sgTestInstance(t)
		web := sgTestGroup(t, seededProjectID, "spec033-web-"+uuid.NewString()[:6])
		if got := sgTestGroupsOf(t, mustAlloc(t, instance, []string{web}, "")); !equalIDs(got, []string{web}) {
			t.Errorf("with web: %v; want [%s]", got, web)
		}
		if got := sgTestGroupsOf(t, mustAlloc(t, sgTestInstance(t), nil, "")); !equalIDs(got, []string{defaultID}) {
			t.Errorf("with none: %v; want default [%s]", got, defaultID)
		}
	})

	t.Run("a later port copies the first port's current groups", func(t *testing.T) {
		instance := sgTestInstance(t)
		web := sgTestGroup(t, seededProjectID, "spec033-web-"+uuid.NewString()[:6])
		db := sgTestGroup(t, seededProjectID, "spec033-db-"+uuid.NewString()[:6])
		first := mustAlloc(t, instance, []string{web}, "")
		if err := database.WithTx(ctx, testPool, func(tx database.Tx) error {
			return AddPortSecurityGroup(ctx, tx, first, db) // a change accepted between the two ports
		}); err != nil {
			t.Fatal(err)
		}
		later := mustAlloc(t, instance, nil, first)
		if got, want := sgTestGroupsOf(t, later), sorted(web, db); !equalIDs(got, want) {
			t.Errorf("the later port: %v; want the first port's %v", got, want)
		}

		emptied := mustAlloc(t, sgTestInstance(t), []string{web}, "")
		if err := database.WithTx(ctx, testPool, func(tx database.Tx) error {
			return SetPortSecurityGroups(ctx, tx, emptied, nil)
		}); err != nil {
			t.Fatal(err)
		}
		if got := sgTestGroupsOf(t, mustAlloc(t, sgTestInstance(t), nil, emptied)); len(got) != 0 {
			t.Errorf("copying from a port with no group: %v; want none, not default", got)
		}
	})

	t.Run("the copy waits for a change that holds the instance", func(t *testing.T) {
		dbtest.RequirePostgres(t) // SQLite's immediate transactions serialise the two anyway
		instance := sgTestInstance(t)
		web := sgTestGroup(t, seededProjectID, "spec033-web-"+uuid.NewString()[:6])
		db := sgTestGroup(t, seededProjectID, "spec033-db-"+uuid.NewString()[:6])
		first := mustAlloc(t, instance, []string{web}, "")

		tx, err := testPool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck // a no-op after the commit
		var one int
		if err := tx.QueryRow(ctx, "SELECT 1 FROM instances WHERE id = $1 FOR UPDATE", instance).Scan(&one); err != nil {
			t.Fatal(err)
		}
		if err := AddPortSecurityGroup(ctx, tx, first, db); err != nil {
			t.Fatal(err)
		}

		type result struct {
			port string
			err  error
		}
		done := make(chan result, 1)
		go func() {
			port, err := alloc(instance, nil, first)
			done <- result{port, err}
		}()
		select {
		case r := <-done:
			t.Fatalf("the allocation finished while the change held the instance (%+v)", r)
		case <-time.After(300 * time.Millisecond):
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		r := <-done
		if r.err != nil {
			t.Fatalf("AllocatePortForInstance: %v", r.err)
		}
		if got, want := sgTestGroupsOf(t, r.port), sorted(web, db); !equalIDs(got, want) {
			t.Errorf("the later port: %v; want %v, with the change that held the instance", got, want)
		}
	})
}
