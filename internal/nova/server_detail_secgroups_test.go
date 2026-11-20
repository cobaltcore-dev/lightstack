package nova

import (
	"net/http"
	"slices"
	"sort"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/internal/neutron"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

func hasName(names []string, want string) bool {
	return slices.Contains(names, want)
}

// SPEC-033 (FR-014, FR-015, FR-016; contracts/server-report.md): a server's security_groups are the
// groups its ports carry, one entry per port and group, and the key is absent when there are none.
// The report is read from port_security_groups, the list the filter is built from; nothing reads
// server_security_groups any more (research R1, R10).

// reportOf returns a server's security_groups from show, and whether the key is present at all.
func reportOf(t *testing.T, r *gin.Engine, projectID, serverID string) ([]string, bool) {
	t.Helper()
	code, body := doReq(r, http.MethodGet, "/v2.1/servers/"+serverID, "", testAuthHdr(projectID, seededUserID))
	if code != http.StatusOK {
		t.Fatalf("show %s: code=%d body=%s", serverID, code, body)
	}
	raw, present := serverBody(t, body)["security_groups"]
	if !present {
		return nil, false
	}
	names := sgNamesFromResponse(t, raw)
	sort.Strings(names)
	return names, true
}

// detailReports returns every listed server's security_groups from detail, keyed by id. A server
// without the key maps to nil.
func detailReports(t *testing.T, r *gin.Engine, projectID string) map[string][]string {
	t.Helper()
	code, body := doReq(r, http.MethodGet, "/v2.1/servers/detail", "", testAuthHdr(projectID, seededUserID))
	if code != http.StatusOK {
		t.Fatalf("detail: code=%d body=%s", code, body)
	}
	out := map[string][]string{}
	for _, s := range serversBody(t, body) {
		id, _ := s["id"].(string)
		raw, present := s["security_groups"]
		if !present {
			out[id] = nil
			continue
		}
		names := sgNamesFromResponse(t, raw)
		sort.Strings(names)
		out[id] = names
	}
	return out
}

func wantNames(names ...string) []string {
	out := append([]string(nil), names...)
	sort.Strings(out)
	return out
}

// spec033Server creates a server in projectID, with a network for its ports.
func spec033Server(t *testing.T, r *gin.Engine, projectID string) (serverID, networkID string) {
	t.Helper()
	networkID, _ = createTestNetwork(t, projectID)
	serverID = createTestServer(t, r, projectID,
		`{"server":{"name":"spec033-report-`+uuid.NewString()[:8]+`","flavorRef":"m1.small"}}`)
	return serverID, networkID
}

func groupName(t *testing.T, id string) string {
	t.Helper()
	var name string
	if err := testPool.QueryRow(t.Context(), "SELECT name FROM security_groups WHERE id = $1", id).Scan(&name); err != nil {
		t.Fatalf("read group name: %v", err)
	}
	return name
}

func TestServerReport_OnePortOneGroup(t *testing.T) {
	requireDB(t)
	r, _, network := newNovaWithNetwork(t)
	web := createTestGroup(t, seededProjectID, "spec033-web-"+uuid.NewString()[:6])
	server, netID := spec033Server(t, r, seededProjectID)
	allocateTestPort(t, network, netID, seededProjectID, server, []string{web})

	names, present := reportOf(t, r, seededProjectID, server)
	if !present || !slices.Equal(names, wantNames(groupName(t, web))) {
		t.Fatalf("security_groups=%v (present=%v); want only the port's group %q", names, present, groupName(t, web))
	}
}

func TestServerReport_OnePortTwoGroups(t *testing.T) {
	requireDB(t)
	r, _, network := newNovaWithNetwork(t)
	def, err := neutron.EnsureDefaultSecurityGroup(t.Context(), testPool, seededProjectID)
	if err != nil {
		t.Fatal(err)
	}
	web := createTestGroup(t, seededProjectID, "spec033-web-"+uuid.NewString()[:6])
	server, netID := spec033Server(t, r, seededProjectID)
	allocateTestPort(t, network, netID, seededProjectID, server, []string{def, web})

	names, _ := reportOf(t, r, seededProjectID, server)
	if !slices.Equal(names, wantNames("default", groupName(t, web))) {
		t.Fatalf("security_groups=%v; want default and %s", names, groupName(t, web))
	}
}

func TestServerReport_TwoPortsEachInDefault(t *testing.T) {
	requireDB(t)
	r, _, network := newNovaWithNetwork(t)
	server, netID := spec033Server(t, r, seededProjectID)
	allocateTestPort(t, network, netID, seededProjectID, server, nil)
	allocateTestPort(t, network, netID, seededProjectID, server, nil)

	names, _ := reportOf(t, r, seededProjectID, server)
	if !slices.Equal(names, []string{"default", "default"}) {
		t.Fatalf("security_groups=%v; want default twice, one entry per port and group", names)
	}
}

func TestServerReport_NoPortOmitsTheKey(t *testing.T) {
	requireDB(t)
	r, _, _ := newNovaWithNetwork(t)
	server, _ := spec033Server(t, r, seededProjectID)

	if names, present := reportOf(t, r, seededProjectID, server); present {
		t.Fatalf("show: security_groups=%v; want the key absent for a server with no port", names)
	}
	if names, listed := detailReports(t, r, seededProjectID)[server]; !listed || names != nil {
		t.Fatalf("detail: security_groups=%v (listed=%v); want the server listed without the key", names, listed)
	}
}

func TestServerReport_PortWithoutGroupOmitsTheKey(t *testing.T) {
	requireDB(t)
	r, _, network := newNovaWithNetwork(t)
	server, netID := spec033Server(t, r, seededProjectID)
	port := allocateTestPort(t, network, netID, seededProjectID, server, nil)
	if err := database.WithTx(t.Context(), testPool, func(tx database.Tx) error {
		return neutron.SetPortSecurityGroups(t.Context(), tx, port, nil)
	}); err != nil {
		t.Fatal(err)
	}

	if names, present := reportOf(t, r, seededProjectID, server); present {
		t.Fatalf("security_groups=%v; want the key absent when no port carries a group", names)
	}
}

// FR-015 and User Story 5's first scenario: releases up to v0.6.1 gave a server's ports no group and
// recorded none for the server. Such a server reports no group, which is what its ports carry.
func TestServerReport_EarlierReleaseReportsNoGroup(t *testing.T) {
	requireDB(t)
	r, _, network := newNovaWithNetwork(t)
	server, netID := spec033Server(t, r, seededProjectID)
	port := allocateTestPort(t, network, netID, seededProjectID, server, nil)
	if _, err := testPool.Exec(t.Context(), "DELETE FROM port_security_groups WHERE port_id = $1", port); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), "DELETE FROM server_security_groups WHERE server_id = $1", server); err != nil {
		t.Fatal(err)
	}

	if names, present := reportOf(t, r, seededProjectID, server); present {
		t.Fatalf("security_groups=%v; want the key absent, never an invented default", names)
	}
}

// A group with an empty name is reported by its id, upstream's fallback.
func TestServerReport_EmptyNameIsReportedByID(t *testing.T) {
	requireDB(t)
	r, _, network := newNovaWithNetwork(t)
	nameless := createTestGroup(t, seededProjectID, "")
	server, netID := spec033Server(t, r, seededProjectID)
	allocateTestPort(t, network, netID, seededProjectID, server, []string{nameless})

	names, _ := reportOf(t, r, seededProjectID, server)
	if !slices.Equal(names, []string{nameless}) {
		t.Fatalf("security_groups=%v; want the nameless group by its id %s", names, nameless)
	}
}

// A deleted group is not reported. A group a port carries can no longer be deleted (FR-008c), so it is
// taken off the port first, then deleted; the report lists what the port still carries. This replaces
// the old OrphanDropped test, whose orphan lived in server_security_groups, which nothing reads now.
func TestServerReport_DeletedGroupIsNotReported(t *testing.T) {
	requireDB(t)
	r, _, network := newNovaWithNetwork(t)
	web := createTestGroup(t, seededProjectID, "spec033-web-"+uuid.NewString()[:6])
	gone := createTestGroup(t, seededProjectID, "spec033-gone-"+uuid.NewString()[:6])
	server, netID := spec033Server(t, r, seededProjectID)
	port := allocateTestPort(t, network, netID, seededProjectID, server, []string{web, gone})
	if err := database.WithTx(t.Context(), testPool, func(tx database.Tx) error {
		_, err := neutron.RemovePortSecurityGroup(t.Context(), tx, port, gone)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(t.Context(), "DELETE FROM security_groups WHERE id = $1", gone); err != nil {
		t.Fatal(err)
	}

	names, _ := reportOf(t, r, seededProjectID, server)
	if !slices.Equal(names, wantNames(groupName(t, web))) {
		t.Fatalf("security_groups=%v; want only %s, not the deleted group", names, groupName(t, web))
	}
}

func TestServerReport_DetailListsEachServersOwnPorts(t *testing.T) {
	requireDB(t)
	r, _, network := newNovaWithNetwork(t)
	web := createTestGroup(t, seededProjectID, "spec033-web-"+uuid.NewString()[:6])
	db := createTestGroup(t, seededProjectID, "spec033-db-"+uuid.NewString()[:6])

	a, netA := spec033Server(t, r, seededProjectID)
	allocateTestPort(t, network, netA, seededProjectID, a, []string{web})
	b, netB := spec033Server(t, r, seededProjectID)
	allocateTestPort(t, network, netB, seededProjectID, b, []string{web, db})
	allocateTestPort(t, network, netB, seededProjectID, b, []string{db})
	c, _ := spec033Server(t, r, seededProjectID)

	got := detailReports(t, r, seededProjectID)
	if !slices.Equal(got[a], wantNames(groupName(t, web))) {
		t.Errorf("server a: %v; want [%s]", got[a], groupName(t, web))
	}
	if want := wantNames(groupName(t, web), groupName(t, db), groupName(t, db)); !slices.Equal(got[b], want) {
		t.Errorf("server b: %v; want %v", got[b], want)
	}
	if names, listed := got[c]; !listed || names != nil {
		t.Errorf("server c: %v (listed=%v); want listed without security_groups", names, listed)
	}
}

// The create path stays as it was (locks, FR-011, FR-012): a server created with a group gets it on
// its port, one created with none gets default. The port is allocated here because the test router
// has no boot.
func TestServerDetail_SecurityGroups_RoundTrip(t *testing.T) {
	requireDB(t)
	cleanupReadback(t)
	defer cleanupReadback(t)

	r, _, network := newNovaWithNetwork(t)
	netID, _ := createTestNetwork(t, seededProjectID)
	code, body := createServerAs(t, r, seededProjectID, seededUserID,
		withImage(t, seededProjectID, `{"server":{"name":"readback-sg1","flavorRef":"m1.small","security_groups":[{"name":"default"}]}}`))
	if code != http.StatusAccepted {
		t.Fatalf("create: code=%d body=%s; want 202", code, body)
	}
	id, _ := serverBody(t, body)["id"].(string)
	allocateTestPort(t, network, netID, seededProjectID, id, nil)

	if names, _ := reportOf(t, r, seededProjectID, id); !hasName(names, "default") {
		t.Errorf("show security_groups=%v; want to contain default", names)
	}
	if names := detailReports(t, r, seededProjectID)[id]; !hasName(names, "default") {
		t.Errorf("detail security_groups=%v; want default", names)
	}
}

// FR-005a: unknown non-default SG rejected with 400, no instance created.
func TestCreateServer_UnknownSecurityGroup_400(t *testing.T) {
	requireDB(t)
	cleanupReadback(t)
	defer cleanupReadback(t)

	r := newNovaTestRouter()
	code, body := createServerAs(t, r, seededProjectID, seededUserID,
		withImage(t, seededProjectID, `{"server":{"name":"readback-sgbad","flavorRef":"m1.small","security_groups":[{"name":"readback-nope"}]}}`))
	if code != http.StatusBadRequest {
		t.Fatalf("create with unknown SG: code=%d body=%s; want 400", code, body)
	}
	if n := instanceCount(t, "readback-sgbad", seededProjectID); n != 0 {
		t.Errorf("instance created despite 400: count=%d", n)
	}
}

// FR-006a: explicit ["default"] succeeds in a project with no seeded default SG —
// the default is ensured (auto-created) on demand.
func TestCreateServer_EnsureDefault_FreshProject(t *testing.T) {
	requireDB(t)
	cleanupReadback(t)
	defer cleanupReadback(t)
	createTestProject(t, freshProjectID, "readback-proj")
	if securityGroupExists(t, "default", freshProjectID) {
		t.Fatalf("precondition failed: fresh project already has a default SG")
	}

	r, _, network := newNovaWithNetwork(t)
	code, body := createServerAs(t, r, freshProjectID, seededUserID,
		withImage(t, freshProjectID, `{"server":{"name":"readback-fresh","flavorRef":"m1.small","security_groups":[{"name":"default"}]}}`))
	if code != http.StatusAccepted {
		t.Fatalf("create in fresh project: code=%d body=%s; want 202 (ensure default)", code, body)
	}
	if !securityGroupExists(t, "default", freshProjectID) {
		t.Errorf("default SG was not ensured for the fresh project")
	}
	id, _ := serverBody(t, body)["id"].(string)
	netID, _ := createTestNetwork(t, freshProjectID)
	allocateTestPort(t, network, netID, freshProjectID, id, nil)
	if names, _ := reportOf(t, r, freshProjectID, id); !slices.Equal(names, []string{"default"}) {
		t.Errorf("show security_groups=%v; want [default]", names)
	}
}

// FR-006 / FR-006a on the implicit path: a create with NO security_groups in a fresh
// project must ensure the project's default group and persist the association, so
// the port allocated at boot can attach it and the read-back is a real association
// rather than a synthesized one.
func TestCreateServer_ImplicitDefault_FreshProject(t *testing.T) {
	requireDB(t)
	cleanupReadback(t)
	defer cleanupReadback(t)
	createTestProject(t, freshProjectID, "readback-proj")
	if securityGroupExists(t, "default", freshProjectID) {
		t.Fatalf("precondition failed: fresh project already has a default SG")
	}

	r, _, network := newNovaWithNetwork(t)
	code, body := createServerAs(t, r, freshProjectID, seededUserID,
		withImage(t, freshProjectID, `{"server":{"name":"readback-implicit","flavorRef":"m1.small"}}`))
	if code != http.StatusAccepted {
		t.Fatalf("implicit create in fresh project: code=%d body=%s; want 202", code, body)
	}
	if !securityGroupExists(t, "default", freshProjectID) {
		t.Errorf("default SG was not ensured on the implicit path")
	}
	id, _ := serverBody(t, body)["id"].(string)
	netID, _ := createTestNetwork(t, freshProjectID)
	allocateTestPort(t, network, netID, freshProjectID, id, nil)
	if names, _ := reportOf(t, r, freshProjectID, id); !slices.Equal(names, []string{"default"}) {
		t.Errorf("show security_groups=%v; want [default], read from the port", names)
	}
}

// Upstream parity for degenerate items: {"name":""} fails schema validation (400,
// no instance); {} carries no name and is ignored, so the server gets `default`.
func TestCreateServer_EmptySecurityGroupName_400(t *testing.T) {
	requireDB(t)
	cleanupReadback(t)
	defer cleanupReadback(t)

	r := newNovaTestRouter()
	code, body := createServerAs(t, r, seededProjectID, seededUserID,
		withImage(t, seededProjectID, `{"server":{"name":"readback-sgempty","flavorRef":"m1.small","security_groups":[{"name":""}]}}`))
	if code != http.StatusBadRequest {
		t.Fatalf("create with empty SG name: code=%d body=%s; want 400", code, body)
	}
	if n := instanceCount(t, "readback-sgempty", seededProjectID); n != 0 {
		t.Errorf("instance created despite 400: count=%d", n)
	}
}

func TestCreateServer_NamelessSecurityGroupItem_Ignored(t *testing.T) {
	requireDB(t)
	cleanupReadback(t)
	defer cleanupReadback(t)

	r, _, network := newNovaWithNetwork(t)
	code, body := createServerAs(t, r, seededProjectID, seededUserID,
		withImage(t, seededProjectID, `{"server":{"name":"readback-sgnameless","flavorRef":"m1.small","security_groups":[{}]}}`))
	if code != http.StatusAccepted {
		t.Fatalf("create with nameless SG item: code=%d body=%s; want 202", code, body)
	}
	id, _ := serverBody(t, body)["id"].(string)
	netID, _ := createTestNetwork(t, seededProjectID)
	allocateTestPort(t, network, netID, seededProjectID, id, nil)
	if names, _ := reportOf(t, r, seededProjectID, id); !slices.Equal(names, []string{"default"}) {
		t.Errorf("show security_groups=%v; want [default]", names)
	}
}

// neutron.EnsureDefaultSecurityGroup seeds rules only for a group it created; an
// existing `default` — even one with no rules — is left exactly as found.
func TestEnsureDefaultSecurityGroup_SeedsOnlyWhenCreated(t *testing.T) {
	requireDB(t)
	cleanupReadback(t)
	defer cleanupReadback(t)
	ctx := t.Context()

	createTestProject(t, freshProjectID, "readback-proj")
	id, err := neutron.EnsureDefaultSecurityGroup(ctx, testPool, freshProjectID)
	if err != nil {
		t.Fatalf("ensure (fresh): %v", err)
	}
	if n := ruleCountForSG(t, id); n != 4 {
		t.Errorf("fresh default: rules=%d; want 4 seeded (SPEC-033 FR-043)", n)
	}

	// Second call on the now-existing group: still 4, and the same id.
	id2, err := neutron.EnsureDefaultSecurityGroup(ctx, testPool, freshProjectID)
	if err != nil || id2 != id {
		t.Fatalf("ensure (again): id=%s err=%v; want same id %s", id2, err, id)
	}
	if n := ruleCountForSG(t, id); n != 4 {
		t.Errorf("re-ensure: rules=%d; want 4 (unchanged)", n)
	}

	// Operator empties the group: ensure must NOT restore the rules.
	if _, err := testPool.Exec(ctx, "DELETE FROM security_group_rules WHERE security_group_id = $1", id); err != nil {
		t.Fatal(err)
	}
	if _, err := neutron.EnsureDefaultSecurityGroup(ctx, testPool, freshProjectID); err != nil {
		t.Fatalf("ensure (emptied): %v", err)
	}
	if n := ruleCountForSG(t, id); n != 0 {
		t.Errorf("emptied default was re-seeded: rules=%d; want 0", n)
	}
}

// The seeded rules of a lazily-created `default` group open nothing to every address. Upstream's
// default security group admits inbound traffic only from its own members, and an ingress rule
// carrying neither remote_ip_prefix nor remote_group_id is enforced as 0.0.0.0/0 — so seeding one here
// would open that port to the world for every project that ever boots a server (cloud-storage#528).
// Since SPEC-033 FR-043 the two ingress rules name the group itself as their remote.
func TestEnsureDefaultSecurityGroup_SeedsNoIngressOpenToEveryAddress(t *testing.T) {
	requireDB(t)
	cleanupReadback(t)
	defer cleanupReadback(t)
	ctx := t.Context()

	createTestProject(t, freshProjectID, "readback-proj")
	id, err := neutron.EnsureDefaultSecurityGroup(ctx, testPool, freshProjectID)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	want := []string{"egress/IPv4//0-0", "egress/IPv6//0-0", "ingress/IPv4//0-0", "ingress/IPv6//0-0"}
	if got := sgRuleShapes(t, id); !slices.Equal(got, want) {
		t.Errorf("fresh default rules=%v; want %v (egress allow-all, ingress from members)", got, want)
	}
	var open int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM security_group_rules
		WHERE security_group_id = $1 AND direction = 'ingress'
		  AND (remote_group_id IS NULL OR remote_group_id <> $1 OR remote_ip_prefix IS NOT NULL)`, id).Scan(&open); err != nil {
		t.Fatal(err)
	}
	if open != 0 {
		t.Errorf("fresh default has %d ingress rule(s) admitting more than its own members; want none", open)
	}
}

// The seeded project's `default` group (migration 002) opens nothing to every address, for the
// same reason. Since migration 084 it admits its own members, as upstream's default does
// (SPEC-033 FR-043): two ingress rules whose remote group is `default` itself, and no other
// ingress rule. Needs a database migrated from scratch: a database that ran the earlier 002
// keeps the ingress rule it inserted, since migrations are not re-applied.
func TestSeededDefaultSecurityGroup_NoIngressOpenToEveryAddress(t *testing.T) {
	requireDB(t)
	ctx := t.Context()

	const seededDefaultSGID = "00000000-0000-0000-0000-000000000030"
	rows, err := testPool.Query(ctx,
		`SELECT ethertype, COALESCE(remote_group_id::text, ''), COALESCE(remote_ip_prefix, '')
		   FROM security_group_rules WHERE security_group_id = $1 AND direction = 'ingress'
		  ORDER BY ethertype`, seededDefaultSGID)
	if err != nil {
		t.Fatalf("read ingress rules of the seeded default group: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var ethertype, remoteGroup, prefix string
		if err := rows.Scan(&ethertype, &remoteGroup, &prefix); err != nil {
			t.Fatal(err)
		}
		if remoteGroup != seededDefaultSGID || prefix != "" {
			t.Errorf("seeded default has an ingress rule %s from group %q, prefix %q; want only its own members (cloud-storage#528)",
				ethertype, remoteGroup, prefix)
		}
		got = append(got, ethertype)
	}
	if want := []string{"IPv4", "IPv6"}; !slices.Equal(got, want) {
		t.Errorf("seeded default's ingress rules = %v; want the two member rules %v (FR-043)", got, want)
	}
}
