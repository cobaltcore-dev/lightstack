package nova

import (
	"context"
	"testing"

	"github.com/gin-gonic/gin"
)

// SPEC-025 US6 (cloud-storage#547 B17): a floating IP associated with a server must
// appear in the server's address map.
//
// getInstanceAddresses built the map from ports.fixed_ips alone and hardcoded
// "OS-EXT-IPS:type": "fixed", so only one kind of address could ever appear. The
// association itself worked — `openstack floating ip list` reported it bound and
// ACTIVE — but the field users, `server list`'s Networks column, the dashboard's
// instance list and the infrastructure-as-code provider all read was missing it. A
// user who associated an address was told it had not taken effect.
//
// Upstream chains fixed and floating addresses into the same per-network list and tags
// each entry with its own kind.

// addrFixture seeds a network, a port on it owned by a project, and returns both ids.
func addrFixture(t *testing.T, projectID, netName, fixedIP string) (networkID, portID string) {
	t.Helper()
	ctx := t.Context()

	if err := testPool.QueryRow(ctx,
		`INSERT INTO networks (name, project_id, status, admin_state_up)
		 VALUES ($1, $2, 'ACTIVE', true) RETURNING id::text`, netName, projectID,
	).Scan(&networkID); err != nil {
		t.Fatalf("seeding network: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM networks WHERE id = $1`, networkID)
	})
	return networkID, ""
}

// attachPort creates a port bound to an instance with one fixed address.
func attachPort(t *testing.T, networkID, projectID, instanceID, fixedIP string) string {
	t.Helper()
	var portID string
	if err := testPool.QueryRow(t.Context(),
		`INSERT INTO ports (network_id, project_id, device_id, mac_address, fixed_ips, status, admin_state_up)
		 VALUES ($1, $2, $3, '52:54:00:12:34:56', $4::jsonb, 'ACTIVE', true)
		 RETURNING id::text`,
		networkID, projectID, instanceID,
		`[{"ip_address":"`+fixedIP+`","subnet_id":null}]`,
	).Scan(&portID); err != nil {
		t.Fatalf("seeding port: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM ports WHERE id = $1`, portID)
	})
	return portID
}

// associateFloatingIP binds a floating address to a port.
func associateFloatingIP(t *testing.T, projectID, networkID, portID, floatingIP, fixedIP string) {
	t.Helper()
	var id string
	if err := testPool.QueryRow(t.Context(),
		`INSERT INTO floating_ips (project_id, floating_network_id, floating_ip_address, fixed_ip_address, port_id, status)
		 VALUES ($1, $2, $3, $4, $5, 'ACTIVE') RETURNING id::text`,
		projectID, networkID, floatingIP, fixedIP, portID,
	).Scan(&id); err != nil {
		t.Fatalf("seeding floating ip: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM floating_ips WHERE id = $1`, id)
	})
}

// entriesFor returns the address entries for a network name.
func entriesFor(t *testing.T, addresses gin.H, netName string) []gin.H {
	t.Helper()
	raw, ok := addresses[netName]
	if !ok {
		t.Fatalf("no addresses for network %q; got %#v", netName, addresses)
	}
	list, ok := raw.([]gin.H)
	if !ok {
		t.Fatalf("addresses for %q are %T, not a list", netName, raw)
	}
	return list
}

// kindsAndAddrs summarises an entry list as kind->address pairs.
func kindsAndAddrs(entries []gin.H) map[string]string {
	out := map[string]string{}
	for _, e := range entries {
		kind, _ := e["OS-EXT-IPS:type"].(string)
		addr, _ := e["addr"].(string)
		out[addr] = kind
	}
	return out
}

// Floating addresses here are TEST-NET-3 (203.0.113.0/24) on purpose. The real-mode
// runbook's external subnet is TEST-NET-1 (192.0.2.0/24) and Lightstack's allocator hands out
// 192.0.2.100 first — so a hardcoded 192.0.2.100 in a test collides with a live
// deployment on floating_ips' unique address constraint, against the shared development
// database. Observed during the SPEC-025 host pass, as
// `duplicate key value violates unique constraint "floating_ips_floating_ip_address_key"`.
//
// Keep the two ranges disjoint: whichever range the runbook allocates from, these tests
// must not claim addresses in it.

// T071 (FR-034, FR-035): both kinds, same list, each tagged from the address.
func TestAddressesIncludeFloatingIPs(t *testing.T) {
	requireDB(t)
	_, _, mgr := newLifecycleService(t)
	svc, _, _ := newLifecycleService(t)

	instanceID, _ := seedLifecycleInstance(t, mgr, "spec025-addr-fip")
	netID, _ := addrFixture(t, seededProjectID, "spec025-addr-net", "10.244.10.77")
	portID := attachPort(t, netID, seededProjectID, instanceID, "10.244.10.77")
	associateFloatingIP(t, seededProjectID, netID, portID, "203.0.113.100", "10.244.10.77")

	addresses, err := svc.getInstanceAddresses(t.Context(), instanceID, seededProjectID)
	if err != nil {
		t.Fatalf("getInstanceAddresses: %v", err)
	}

	entries := entriesFor(t, addresses, "spec025-addr-net")
	got := kindsAndAddrs(entries)

	if len(entries) != 2 {
		t.Errorf("%d address entries; want 2 (the fixed and the floating). got %#v", len(entries), got)
	}
	if got["10.244.10.77"] != "fixed" {
		t.Errorf("fixed address tagged %q; want \"fixed\"", got["10.244.10.77"])
	}
	if kind, ok := got["203.0.113.100"]; !ok {
		t.Error("the associated floating address is absent — `server show` still says the association had no effect (FR-034)")
	} else if kind != "floating" {
		t.Errorf("floating address tagged %q; want \"floating\" — the kind must come from the address, not a constant (FR-035)", kind)
	}
}

// T072 (FR-037, FR-038): unchanged shape without a floating address; an unassociated
// floating address appears nowhere; two on one port both appear.
func TestAddressesFloatingEdgeCases(t *testing.T) {
	requireDB(t)
	_, _, mgr := newLifecycleService(t)
	svc, _, _ := newLifecycleService(t)

	t.Run("no floating address leaves the shape unchanged", func(t *testing.T) {
		instanceID, _ := seedLifecycleInstance(t, mgr, "spec025-addr-nofip")
		netID, _ := addrFixture(t, seededProjectID, "spec025-nofip-net", "10.244.11.5")
		attachPort(t, netID, seededProjectID, instanceID, "10.244.11.5")

		addresses, err := svc.getInstanceAddresses(t.Context(), instanceID, seededProjectID)
		if err != nil {
			t.Fatalf("getInstanceAddresses: %v", err)
		}
		entries := entriesFor(t, addresses, "spec025-nofip-net")
		if len(entries) != 1 {
			t.Errorf("%d entries; want exactly the fixed one (FR-037)", len(entries))
		}
		if kind, _ := entries[0]["OS-EXT-IPS:type"].(string); kind != "fixed" {
			t.Errorf("entry tagged %q; want \"fixed\"", kind)
		}
	})

	t.Run("an unassociated floating address appears on no server", func(t *testing.T) {
		instanceID, _ := seedLifecycleInstance(t, mgr, "spec025-addr-unassoc")
		netID, _ := addrFixture(t, seededProjectID, "spec025-unassoc-net", "10.244.12.5")
		attachPort(t, netID, seededProjectID, instanceID, "10.244.12.5")

		// Allocated, but bound to no port.
		var fipID string
		if err := testPool.QueryRow(t.Context(),
			`INSERT INTO floating_ips (project_id, floating_network_id, floating_ip_address, status)
			 VALUES ($1, $2, '203.0.113.201', 'DOWN') RETURNING id::text`,
			seededProjectID, netID).Scan(&fipID); err != nil {
			t.Fatalf("seeding unassociated fip: %v", err)
		}
		t.Cleanup(func() {
			_, _ = testPool.Exec(context.Background(), `DELETE FROM floating_ips WHERE id = $1`, fipID)
		})

		addresses, err := svc.getInstanceAddresses(t.Context(), instanceID, seededProjectID)
		if err != nil {
			t.Fatalf("getInstanceAddresses: %v", err)
		}
		for addr := range kindsAndAddrs(entriesFor(t, addresses, "spec025-unassoc-net")) {
			if addr == "203.0.113.201" {
				t.Error("an unassociated floating address appeared on a server (FR-038)")
			}
		}
	})

	t.Run("two floating addresses on one port both appear", func(t *testing.T) {
		instanceID, _ := seedLifecycleInstance(t, mgr, "spec025-addr-two")
		netID, _ := addrFixture(t, seededProjectID, "spec025-two-net", "10.244.13.5")
		portID := attachPort(t, netID, seededProjectID, instanceID, "10.244.13.5")
		associateFloatingIP(t, seededProjectID, netID, portID, "203.0.113.211", "10.244.13.5")
		associateFloatingIP(t, seededProjectID, netID, portID, "203.0.113.212", "10.244.13.5")

		addresses, err := svc.getInstanceAddresses(t.Context(), instanceID, seededProjectID)
		if err != nil {
			t.Fatalf("getInstanceAddresses: %v", err)
		}
		got := kindsAndAddrs(entriesFor(t, addresses, "spec025-two-net"))
		for _, want := range []string{"203.0.113.211", "203.0.113.212"} {
			if got[want] != "floating" {
				t.Errorf("floating address %s missing or mistagged (%q)", want, got[want])
			}
		}
	})

	t.Run("a server on several networks only gains the entry on the right one", func(t *testing.T) {
		instanceID, _ := seedLifecycleInstance(t, mgr, "spec025-addr-multi")
		netA, _ := addrFixture(t, seededProjectID, "spec025-multi-a", "10.244.14.5")
		netB, _ := addrFixture(t, seededProjectID, "spec025-multi-b", "10.244.15.5")
		portA := attachPort(t, netA, seededProjectID, instanceID, "10.244.14.5")
		attachPort(t, netB, seededProjectID, instanceID, "10.244.15.5")
		associateFloatingIP(t, seededProjectID, netA, portA, "203.0.113.221", "10.244.14.5")

		addresses, err := svc.getInstanceAddresses(t.Context(), instanceID, seededProjectID)
		if err != nil {
			t.Fatalf("getInstanceAddresses: %v", err)
		}
		if len(entriesFor(t, addresses, "spec025-multi-a")) != 2 {
			t.Error("the network with the floating address should carry two entries")
		}
		if len(entriesFor(t, addresses, "spec025-multi-b")) != 1 {
			t.Error("the other network must be untouched")
		}
	})
}

// T074 (FR-039): an administrator reading another project's server sees its floating
// addresses too.
//
// getInstanceAddresses takes the instance's OWNING project deliberately — its own
// comment explains that passing the caller's project returns an empty map for an
// administrator viewing another tenant's server. The new join has to widen with its
// parent for the same reason, and this is the assertion that catches it if it does not.
func TestAdminSeesAnotherProjectsFloatingAddresses(t *testing.T) {
	requireDB(t)
	_, _, mgr := newLifecycleService(t)
	svc, _, _ := newLifecycleService(t)

	const otherProject = "00000000-0000-0000-0000-0000000000f6"
	if _, err := testPool.Exec(t.Context(),
		`INSERT INTO projects (id, name, description, enabled)
		 VALUES ($1, 'spec025-fip-other', '', true) ON CONFLICT (id) DO NOTHING`, otherProject); err != nil {
		t.Fatalf("seeding the other project: %v", err)
	}

	// A server, port and floating address all owned by the OTHER project.
	var instanceID string
	if err := testPool.QueryRow(t.Context(),
		`INSERT INTO instances (name, project_id, user_id, flavor_id, status, power_state)
		 VALUES ('spec025-other-srv', $1, $2, (SELECT id FROM flavors LIMIT 1), 'ACTIVE', 1)
		 RETURNING id::text`, otherProject, seededUserID).Scan(&instanceID); err != nil {
		t.Fatalf("seeding the other project's instance: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM instances WHERE id = $1`, instanceID)
	})

	netID, _ := addrFixture(t, otherProject, "spec025-other-net", "10.244.20.9")
	portID := attachPort(t, netID, otherProject, instanceID, "10.244.20.9")
	associateFloatingIP(t, otherProject, netID, portID, "203.0.113.231", "10.244.20.9")

	// The OWNING project is what the caller must pass — that is the whole point of the
	// parameter, and what makes the administrative read work.
	addresses, err := svc.getInstanceAddresses(t.Context(), instanceID, otherProject)
	if err != nil {
		t.Fatalf("getInstanceAddresses: %v", err)
	}
	got := kindsAndAddrs(entriesFor(t, addresses, "spec025-other-net"))
	if got["203.0.113.231"] != "floating" {
		t.Errorf("an administrator reading another project's server does not see its floating address (FR-039); got %#v", got)
	}
	_ = mgr
}
