package neutron

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// Branch review 2026-08-28, SRC-1 — the Update* handlers the SQL refactor rewrote.
//
// `refactor(sql)` converted twenty Update* handlers from hand-counted placeholders to
// common.SQLUpdate. Fifteen of the twenty had no test at all — eight of them in this package.
//
// WHAT THESE TESTS CATCH, AND WHAT THEY DO NOT. Each test sends SEVERAL fields in one request, every
// one carrying a value distinguishable from the others, then reads each column back separately. That
// catches a value assigned to the wrong column, a field the handler silently ignores, and a SET wider
// than the request — none of which raises an error anywhere.
//
// It does NOT catch a misnumbered placeholder, and no test here could, because SQLUpdate makes that
// class unrepresentable: a placeholder is obtained only by appending the value it reads, and the
// rendered fragment carries its own number ("name = $1"), so reordering the assignments changes
// nothing. Verified by mutation — reversing the SET list leaves every one of these tests green. The
// refactor closed that bug by construction rather than by testing, which is the stronger outcome and
// worth stating plainly so nobody reads these tests as guarding it.
//
// Where a handler assigns only one caller-supplied column there is nothing to swap it with except the
// WHERE argument, so those tests assert instead that the addressed row changed and its siblings did
// not — the shape a wrong id predicate produces.

// mustExec runs a fixture statement and fails the test on error.
func mustExec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("fixture %q: %v", sql, err)
	}
}

// cleanupRows removes a fixture row when the test ends.
func cleanupRows(t *testing.T, table, id string) {
	t.Helper()
	t.Cleanup(func() {
		testPool.Exec(context.Background(), //nolint:errcheck // best-effort cleanup
			fmt.Sprintf("DELETE FROM %s WHERE id = $1", table), id)
	})
}

func TestUpdatePortAssignsEachFieldToItsOwnColumn(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()

	id, beforeDevice, afterDevice := uuid.NewString(), uuid.NewString(), uuid.NewString()
	netID := uuid.NewString()
	cleanupRows(t, "ports", id)
	cleanupRows(t, "networks", netID)
	// The port needs its network: UpdatePort writes, then delegates the response to GetPort, which
	// INNER JOINs networks. A port with no network_id is updated and then answered 404.
	mustExec(t, `INSERT INTO networks (id, name, project_id) VALUES ($1, 'src1-port-net', $2)`,
		netID, seededProjectID)
	// mac_address, status and fixed_ips are set because GetPort scans them into plain strings; a NULL
	// in any is a 500. Not reachable through the API — CreatePort writes all three — so this is a
	// fixture requirement, not something this test asserts.
	mustExec(t, `INSERT INTO ports
		(id, name, network_id, project_id, admin_state_up, device_id, device_owner, mac_address, status, fixed_ips)
		VALUES ($1, 'before-name', $2, $3, false, $4, 'before-owner', 'fa:16:3e:00:00:01', 'ACTIVE', '[]')`,
		id, netID, seededProjectID, beforeDevice)

	code, body := doReqBody(r, http.MethodPut, "/v2.0/ports/"+id,
		fmt.Sprintf(`{"port":{"name":"after-name","admin_state_up":true,"device_id":%q,"device_owner":"after-owner"}}`,
			afterDevice),
		neutronAuthHdr(seededProjectID, false))
	if code != http.StatusOK {
		t.Fatalf("PUT port: code=%d body=%s; want 200", code, body)
	}

	var name, deviceID, deviceOwner string
	var adminStateUp bool
	if err := testPool.QueryRow(context.Background(),
		"SELECT name, admin_state_up, device_id, device_owner FROM ports WHERE id = $1",
		id).Scan(&name, &adminStateUp, &deviceID, &deviceOwner); err != nil {
		t.Fatalf("read back port: %v", err)
	}
	for _, c := range []struct{ col, got, want string }{
		{"name", name, "after-name"},
		{"device_id", deviceID, afterDevice},
		{"device_owner", deviceOwner, "after-owner"},
	} {
		if c.got != c.want {
			t.Errorf("ports.%s = %q, want %q — a column holding another field's value is a "+
				"wrong-column assignment", c.col, c.got, c.want)
		}
	}
	if !adminStateUp {
		t.Errorf("ports.admin_state_up = false, want true")
	}
}

func TestUpdateQoSPolicyAssignsEachFieldToItsOwnColumn(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()

	id := uuid.NewString()
	cleanupRows(t, "qos_policies", id)
	mustExec(t, `INSERT INTO qos_policies (id, project_id, name, description, shared)
		VALUES ($1, $2, 'before-name', 'before-description', false)`, id, seededProjectID)

	code, body := doReqBody(r, http.MethodPut, "/v2.0/qos/policies/"+id,
		`{"policy":{"name":"after-name","description":"after-description","shared":true}}`,
		neutronAuthHdr(seededProjectID, false))
	if code != http.StatusOK {
		t.Fatalf("PUT qos policy: code=%d body=%s; want 200", code, body)
	}

	var name, description string
	var shared bool
	if err := testPool.QueryRow(context.Background(),
		"SELECT name, description, shared FROM qos_policies WHERE id = $1",
		id).Scan(&name, &description, &shared); err != nil {
		t.Fatalf("read back policy: %v", err)
	}
	if name != "after-name" {
		t.Errorf("qos_policies.name = %q, want %q", name, "after-name")
	}
	if description != "after-description" {
		t.Errorf("qos_policies.description = %q, want %q", description, "after-description")
	}
	if !shared {
		t.Errorf("qos_policies.shared = false, want true")
	}
}

// The two integer columns are the pair most exposed to a swap: max_kbps and max_burst_kbps share a
// type, so exchanging them produces no error anywhere — the rule simply enforces the wrong rate. They
// are seeded and updated with values that cannot be confused.
func TestUpdateBandwidthLimitRuleAssignsEachFieldToItsOwnColumn(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()

	policyID, ruleID := uuid.NewString(), uuid.NewString()
	cleanupRows(t, "qos_bandwidth_limit_rules", ruleID)
	cleanupRows(t, "qos_policies", policyID)
	mustExec(t, `INSERT INTO qos_policies (id, project_id, name) VALUES ($1, $2, 'src1-blr-policy')`,
		policyID, seededProjectID)
	mustExec(t, `INSERT INTO qos_bandwidth_limit_rules (id, qos_policy_id, max_kbps, max_burst_kbps, direction)
		VALUES ($1, $2, 1000, 2000, 'egress')`, ruleID, policyID)

	code, body := doReqBody(r, http.MethodPut,
		"/v2.0/qos/policies/"+policyID+"/bandwidth_limit_rules/"+ruleID,
		`{"bandwidth_limit_rule":{"max_kbps":31111,"max_burst_kbps":42222,"direction":"ingress"}}`,
		neutronAuthHdr(seededProjectID, false))
	if code != http.StatusOK {
		t.Fatalf("PUT bandwidth limit rule: code=%d body=%s; want 200", code, body)
	}

	var maxKbps, maxBurst int
	var direction string
	if err := testPool.QueryRow(context.Background(),
		"SELECT max_kbps, max_burst_kbps, direction FROM qos_bandwidth_limit_rules WHERE id = $1",
		ruleID).Scan(&maxKbps, &maxBurst, &direction); err != nil {
		t.Fatalf("read back rule: %v", err)
	}
	if maxKbps != 31111 {
		t.Errorf("max_kbps = %d, want 31111 (42222 would mean it read max_burst_kbps's argument)", maxKbps)
	}
	if maxBurst != 42222 {
		t.Errorf("max_burst_kbps = %d, want 42222 (31111 would mean it read max_kbps's argument)", maxBurst)
	}
	if direction != "ingress" {
		t.Errorf("direction = %q, want %q", direction, "ingress")
	}
}

// UpdateRBACPolicy assigns one caller-supplied column, so there is no second field to swap with. What
// remains swappable is the WHERE argument: a statement that read the id as the value and the value as
// the id would write into the wrong row, or none. Two rows are seeded and both are read back.
func TestUpdateRBACPolicyWritesOnlyTheAddressedRow(t *testing.T) {
	requireDB(t)
	requireSecondProject(t)
	r := newNeutronTestRouter()

	target, sibling := uuid.NewString(), uuid.NewString()
	netID := uuid.NewString()
	cleanupRows(t, "rbac_policies", target)
	cleanupRows(t, "rbac_policies", sibling)
	cleanupRows(t, "networks", netID)
	mustExec(t, `INSERT INTO networks (id, name, project_id) VALUES ($1, 'src1-rbac-net', $2)`,
		netID, seededProjectID)
	for _, id := range []string{target, sibling} {
		mustExec(t, `INSERT INTO rbac_policies (id, project_id, object_type, object_id, target_tenant, action)
			VALUES ($1, $2, 'network', $3, $4, 'access_as_shared')`,
			id, seededProjectID, netID, seededProjectID)
	}

	code, body := doReqBody(r, http.MethodPut, "/v2.0/rbac-policies/"+target,
		fmt.Sprintf(`{"rbac_policy":{"target_tenant":%q}}`, otherProjectID),
		neutronAuthHdr(seededProjectID, false))
	if code != http.StatusOK {
		t.Fatalf("PUT rbac policy: code=%d body=%s; want 200", code, body)
	}

	var targetTenant, siblingTenant string
	if err := testPool.QueryRow(context.Background(),
		"SELECT target_tenant::text FROM rbac_policies WHERE id = $1", target).Scan(&targetTenant); err != nil {
		t.Fatalf("read back target: %v", err)
	}
	if err := testPool.QueryRow(context.Background(),
		"SELECT target_tenant::text FROM rbac_policies WHERE id = $1", sibling).Scan(&siblingTenant); err != nil {
		t.Fatalf("read back sibling: %v", err)
	}
	if targetTenant != otherProjectID {
		t.Errorf("addressed policy target_tenant = %q, want %q", targetTenant, otherProjectID)
	}
	if siblingTenant != seededProjectID {
		t.Errorf("sibling policy target_tenant = %q, want it untouched (%q) — a swapped WHERE argument "+
			"writes the wrong row", siblingTenant, seededProjectID)
	}
}

// Five columns, three of them integers that share a type — the widest swap surface of the eight.
func TestUpdateSubnetPoolAssignsEachFieldToItsOwnColumn(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()

	id := uuid.NewString()
	cleanupRows(t, "subnet_pools", id)
	mustExec(t, `INSERT INTO subnet_pools (id, project_id, name, prefixes, min_prefixlen, max_prefixlen, default_prefixlen)
		VALUES ($1, $2, 'before-name', $3, 8, 32, 24)`, id, seededProjectID, []string{"10.0.0.0/8"})

	code, body := doReqBody(r, http.MethodPut, "/v2.0/subnetpools/"+id,
		`{"subnetpool":{"name":"after-name","prefixes":["192.0.2.0/24"],"min_prefixlen":11,"max_prefixlen":29,"default_prefixlen":27}}`,
		neutronAuthHdr(seededProjectID, false))
	if code != http.StatusOK {
		t.Fatalf("PUT subnet pool: code=%d body=%s; want 200", code, body)
	}

	var name string
	var prefixes []string
	var minLen, maxLen, defLen int
	if err := testPool.QueryRow(context.Background(),
		`SELECT name, prefixes, min_prefixlen, max_prefixlen, default_prefixlen
		 FROM subnet_pools WHERE id = $1`,
		id).Scan(&name, &prefixes, &minLen, &maxLen, &defLen); err != nil {
		t.Fatalf("read back subnet pool: %v", err)
	}
	if name != "after-name" {
		t.Errorf("subnet_pools.name = %q, want %q", name, "after-name")
	}
	if len(prefixes) != 1 || prefixes[0] != "192.0.2.0/24" {
		t.Errorf("subnet_pools.prefixes = %v, want [192.0.2.0/24]", prefixes)
	}
	// Distinct values on purpose: 11/29/27 cannot be permuted into one another silently.
	for _, c := range []struct {
		col  string
		got  int
		want int
	}{{"min_prefixlen", minLen, 11}, {"max_prefixlen", maxLen, 29}, {"default_prefixlen", defLen, 27}} {
		if c.got != c.want {
			t.Errorf("subnet_pools.%s = %d, want %d — these three share a type, so a swap between "+
				"them raises no error", c.col, c.got, c.want)
		}
	}
}

func TestUpdateTrunkAssignsEachFieldToItsOwnColumn(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()

	portID, trunkID := uuid.NewString(), uuid.NewString()
	cleanupRows(t, "trunks", trunkID)
	cleanupRows(t, "ports", portID)
	mustExec(t, `INSERT INTO ports (id, name, project_id) VALUES ($1, 'src1-trunk-port', $2)`,
		portID, seededProjectID)
	mustExec(t, `INSERT INTO trunks (id, project_id, port_id, name, description, admin_state_up)
		VALUES ($1, $2, $3, 'before-name', 'before-description', false)`, trunkID, seededProjectID, portID)

	code, body := doReqBody(r, http.MethodPut, "/v2.0/trunks/"+trunkID,
		`{"trunk":{"name":"after-name","description":"after-description","admin_state_up":true}}`,
		neutronAuthHdr(seededProjectID, false))
	if code != http.StatusOK {
		t.Fatalf("PUT trunk: code=%d body=%s; want 200", code, body)
	}

	var name, description string
	var adminStateUp bool
	if err := testPool.QueryRow(context.Background(),
		"SELECT name, description, admin_state_up FROM trunks WHERE id = $1",
		trunkID).Scan(&name, &description, &adminStateUp); err != nil {
		t.Fatalf("read back trunk: %v", err)
	}
	if name != "after-name" {
		t.Errorf("trunks.name = %q, want %q", name, "after-name")
	}
	if description != "after-description" {
		t.Errorf("trunks.description = %q, want %q", description, "after-description")
	}
	if !adminStateUp {
		t.Errorf("trunks.admin_state_up = false, want true")
	}
}

// The disassociate branch is the one worth pinning: it assigns four columns in a single statement —
// three set to NULL and one to a literal — which is the densest argument list of any handler here.
func TestUpdateFloatingIPDisassociateAssignsEachColumn(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()

	fipID, portID, netID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	cleanupRows(t, "floating_ips", fipID)
	cleanupRows(t, "ports", portID)
	cleanupRows(t, "networks", netID)
	mustExec(t, `INSERT INTO networks (id, name, project_id, external) VALUES ($1, 'src1-fip-ext', $2, true)`,
		netID, seededProjectID)
	mustExec(t, `INSERT INTO ports (id, name, project_id) VALUES ($1, 'src1-fip-port', $2)`,
		portID, seededProjectID)
	// floating_network_id is set because GetFloatingIP scans it into a plain string and a NULL there
	// is an unconditional 500 — the same nullable-column shape B6 fixed for subnets.gateway_ip. It is
	// not reachable through the API (CreateFloatingIP always writes one), so it is a fixture
	// requirement here rather than a defect this test asserts.
	mustExec(t, `INSERT INTO floating_ips
		(id, project_id, floating_network_id, floating_ip_address, port_id, fixed_ip_address, status, description)
		VALUES ($1, $2, $3, '192.0.2.55', $4, '10.0.0.5', 'ACTIVE', 'before-description')`,
		fipID, seededProjectID, netID, portID)

	code, body := doReqBody(r, http.MethodPut, "/v2.0/floatingips/"+fipID,
		`{"floatingip":{"port_id":null,"description":"after-description"}}`,
		neutronAuthHdr(seededProjectID, false))
	if code != http.StatusOK {
		t.Fatalf("PUT floating ip: code=%d body=%s; want 200", code, body)
	}

	var storedPort, fixedIP, routerID, description *string
	var status string
	if err := testPool.QueryRow(context.Background(),
		`SELECT port_id::text, fixed_ip_address, router_id::text, status, description
		 FROM floating_ips WHERE id = $1`,
		fipID).Scan(&storedPort, &fixedIP, &routerID, &status, &description); err != nil {
		t.Fatalf("read back floating ip: %v", err)
	}
	for _, c := range []struct {
		col string
		got *string
	}{{"port_id", storedPort}, {"fixed_ip_address", fixedIP}, {"router_id", routerID}} {
		if c.got != nil {
			t.Errorf("floating_ips.%s = %q after disassociation, want NULL", c.col, *c.got)
		}
	}
	if status != "DOWN" {
		t.Errorf("floating_ips.status = %q, want DOWN", status)
	}
	if description == nil || *description != "after-description" {
		t.Errorf("floating_ips.description = %v, want %q — the description argument sits after four "+
			"others in this statement", description, "after-description")
	}
}

func TestUpdatePortForwardingAssignsEachFieldToItsOwnColumn(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()

	fipID, portID, pfID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	pfNetID := uuid.NewString()
	cleanupRows(t, "port_forwardings", pfID)
	cleanupRows(t, "floating_ips", fipID)
	cleanupRows(t, "ports", portID)
	cleanupRows(t, "networks", pfNetID)
	mustExec(t, `INSERT INTO ports (id, name, project_id) VALUES ($1, 'src1-pf-port', $2)`,
		portID, seededProjectID)
	mustExec(t, `INSERT INTO networks (id, name, project_id, external) VALUES ($1, 'src1-pf-ext', $2, true)`,
		pfNetID, seededProjectID)
	mustExec(t, `INSERT INTO floating_ips (id, project_id, floating_network_id, floating_ip_address)
		VALUES ($1, $2, $3, '192.0.2.66')`, fipID, seededProjectID, pfNetID)
	mustExec(t, `INSERT INTO port_forwardings
		(id, project_id, floatingip_id, internal_port_id, internal_ip_address, external_port, internal_port, protocol, description)
		VALUES ($1, $2, $3, $4, '10.0.0.9', 8080, 80, 'tcp', 'before-description')`,
		pfID, seededProjectID, fipID, portID)

	code, body := doReqBody(r, http.MethodPut,
		"/v2.0/floatingips/"+fipID+"/port_forwardings/"+pfID,
		`{"port_forwarding":{"internal_ip_address":"10.0.0.77","internal_port":8443,"description":"after-description"}}`,
		neutronAuthHdr(seededProjectID, false))
	if code != http.StatusOK {
		t.Fatalf("PUT port forwarding: code=%d body=%s; want 200", code, body)
	}

	var internalIP, description string
	var internalPort, externalPort int
	if err := testPool.QueryRow(context.Background(),
		`SELECT internal_ip_address, internal_port, external_port, description
		 FROM port_forwardings WHERE id = $1`,
		pfID).Scan(&internalIP, &internalPort, &externalPort, &description); err != nil {
		t.Fatalf("read back port forwarding: %v", err)
	}
	if internalIP != "10.0.0.77" {
		t.Errorf("internal_ip_address = %q, want %q", internalIP, "10.0.0.77")
	}
	if internalPort != 8443 {
		t.Errorf("internal_port = %d, want 8443", internalPort)
	}
	// Not in the request, so it must be untouched — an over-wide SET would show up here.
	if externalPort != 8080 {
		t.Errorf("external_port = %d, want it unchanged at 8080", externalPort)
	}
	if description != "after-description" {
		t.Errorf("description = %q, want %q", description, "after-description")
	}
}
