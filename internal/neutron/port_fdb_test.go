package neutron

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// SPEC-033 round 2, T137 (FR-045; research R40, R51): with VXLAN on, a port's forwarding row is written
// in the port's own transaction, so a port never exists without it; a failing write answers 500 and
// leaves no port. A network created before migration 084 has no connection-tracking zone: it gets the
// lowest free one the first time one of its ports is converged.

func TestPortFDBInThePortsTransaction(t *testing.T) {
	requireDB(t)
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}

	setup := func(t *testing.T) (*Service, *fakeFDB, string, func(body string) (int, string)) {
		t.Helper()
		r, svc := newNeutronTestRouterWithService()
		fdb := &fakeFDB{}
		svc.fdb = fdb
		network := seedNetwork(t, "spec033-fdb-"+uuid.NewString()[:6])
		seedSubnet(t, network, "spec033-fdb", "10.83.0.0/24", nil, 4, false)
		return svc, fdb, network, func(body string) (int, string) {
			return doJSONReq(r, http.MethodPost, "/v2.0/ports", body, hdr)
		}
	}

	t.Run("a port create writes its row in its transaction", func(t *testing.T) {
		_, fdb, network, post := setup(t)
		code, body := post(`{"port":{"network_id":"` + network + `"}}`)
		if code != http.StatusCreated {
			t.Fatalf("create: code=%d body=%s", code, body)
		}
		port := createdID(t, body, "port")
		if len(fdb.ports) != 1 || fdb.ports[0] != port || !fdb.inTx[0] {
			t.Errorf("rows %v (in a transaction %v); want the port's, in its transaction", fdb.ports, fdb.inTx)
		}
	})

	t.Run("a failing row answers 500 and leaves no port", func(t *testing.T) {
		_, fdb, network, post := setup(t)
		fdb.err = errors.New("insert into vxlan_fdb_entries failed")
		if code, body := post(`{"port":{"network_id":"` + network + `","name":"spec033-fdb-fail"}}`); code != http.StatusInternalServerError {
			t.Fatalf("create: code=%d body=%s; want 500", code, body)
		}
		var n int
		if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM ports WHERE network_id = $1 AND name = 'spec033-fdb-fail'", network).Scan(&n); err != nil || n != 0 {
			t.Errorf("%d port(s) (%v); want none", n, err)
		}
	})

	t.Run("a server's port the same", func(t *testing.T) {
		svc, fdb, network, _ := setup(t)
		fdb.err = errors.New("insert into vxlan_fdb_entries failed")
		instance := uuid.NewString()
		if _, err := svc.AllocatePortForInstance(context.Background(), InstancePort{NetworkID: network, ProjectID: seededProjectID, InstanceID: instance, SecurityGroupIDs: nil, CopyGroupsFrom: "", Admin: false}); err == nil {
			t.Fatal("a failing row: the port was allocated")
		}
		var n int
		if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM ports WHERE device_id = $1", instance).Scan(&n); err != nil || n != 0 {
			t.Errorf("%d port(s) (%v); want none", n, err)
		}
	})
}

func TestOlderNetworkGetsItsZoneWithItsFirstPort(t *testing.T) {
	requireDB(t)
	_, svc := newNeutronTestRouterWithService()
	fw := newFakeFirewall()
	svc.firewall, svc.brManager = fw, newFakeBridges()
	network := seedNetwork(t, "spec033-zone-old-"+uuid.NewString()[:6]) // no zone, as before migration 084
	subnet := seedSubnet(t, network, "spec033-zone-old", "10.84.0.0/24", nil, 4, false)
	port, _ := seedPortWithIP(t, network, subnet)

	if err := svc.BindPort(context.Background(), port, svc.nodes.Name); err != nil {
		t.Fatalf("bind: %v", err)
	}
	var zone *int
	if err := testPool.QueryRow(context.Background(), "SELECT ct_zone FROM networks WHERE id = $1", network).Scan(&zone); err != nil || zone == nil {
		t.Fatalf("the network's zone after its port's first convergence: %v, %v; want one", zone, err)
	}
	var below int
	if err := testPool.QueryRow(context.Background(), "SELECT count(*) FROM networks WHERE ct_zone >= 4097 AND ct_zone < $1", *zone).Scan(&below); err != nil {
		t.Fatal(err)
	}
	if *zone < 4097 || below != *zone-4097 {
		t.Errorf("zone %d with %d taken below it; want the lowest free from 4097", *zone, below)
	}
	if p, ok := fw.port(port); !ok || p.Zone != *zone {
		t.Errorf("the port's firewall zone %d (held %v); want %d", p.Zone, ok, *zone)
	}
}
