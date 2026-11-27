package neutron

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// SPEC-033 round 2, T136 (FR-008; research R40): a network whose bridge cannot be made on the serving
// node is taken back, compare-and-set: its row goes only while its updated_at holds what the create
// wrote and no subnet, port or router interface references it. A network another request changed or
// built on meanwhile stays.

func TestNetworkCreateUndo(t *testing.T) {
	requireDB(t)
	errBridge := errors.New("RTNETLINK answers: Operation not permitted")
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}
	ctx := context.Background()
	networkNamed := func(t *testing.T, name string) (string, bool) {
		t.Helper()
		var id string
		err := testPool.QueryRow(ctx, "SELECT id::text FROM networks WHERE name = $1", name).Scan(&id)
		if err != nil {
			return "", false
		}
		t.Cleanup(func() {
			mustExec(t, "DELETE FROM subnets WHERE network_id = $1", id)
			mustExec(t, "DELETE FROM networks WHERE id = $1", id)
		})
		return id, true
	}
	create := func(t *testing.T, during func(name string)) string {
		t.Helper()
		r, svc := newNeutronTestRouterWithService()
		bridges := newFakeBridges()
		svc.brManager = bridges
		name := "spec033-undo-" + uuid.NewString()[:8]
		bridges.hook = func(string) error {
			if during != nil {
				during(name)
			}
			return errBridge
		}
		if code, body := doJSONReq(r, http.MethodPost, "/v2.0/networks", `{"network":{"name":"`+name+`"}}`, hdr); code != http.StatusInternalServerError {
			t.Fatalf("create with a failing bridge: code=%d body=%s; want 500", code, body)
		}
		return name
	}

	t.Run("the row goes", func(t *testing.T) {
		name := create(t, nil)
		if id, ok := networkNamed(t, name); ok {
			t.Errorf("network %s is still there; want the create undone", id)
		}
	})

	t.Run("a row another request updated meanwhile stays", func(t *testing.T) {
		name := create(t, func(name string) {
			mustExec(t, "UPDATE networks SET mtu = 1400, updated_at = now() WHERE name = $1", name)
		})
		if _, ok := networkNamed(t, name); !ok {
			t.Error("the network another request updated is gone; want it kept")
		}
	})

	t.Run("a row another request built on stays, with what it built", func(t *testing.T) {
		var subnet string
		name := create(t, func(name string) {
			var id string
			if err := testPool.QueryRow(ctx, "SELECT id::text FROM networks WHERE name = $1", name).Scan(&id); err != nil {
				t.Error(err)
				return
			}
			subnet = uuid.NewString()
			mustExec(t, `INSERT INTO subnets (id, name, network_id, project_id, cidr, ip_version, enable_dhcp, created_at, updated_at)
				VALUES ($1, 'spec033-undo', $2, $3, '10.82.0.0/24', 4, false, now(), now())`, subnet, id, seededProjectID)
		})
		if _, ok := networkNamed(t, name); !ok {
			t.Fatal("the network another request built on is gone; want it kept")
		}
		if !exists(t, "subnets", subnet) {
			t.Error("the subnet another request added is gone")
		}
	})
}
