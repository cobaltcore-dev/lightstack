package nova

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/neutron"
	"github.com/google/uuid"
)

// SPEC-033 round 2, T075 (FR-051; research R47): no port carries stateful and stateless groups at
// once. The check is per port, against that port's own groups, as upstream's: addSecurityGroup
// answers 409 when it would make any of the server's ports mix them (upstream Nova's
// SecurityGroupConnectionStateConflict), an interface attach is never refused for it, and a boot whose
// groups mix both kinds fails its port allocation and ends ERROR with a fault.

func statelessGroup(t *testing.T, projectID, name string) string {
	t.Helper()
	id := createTestGroup(t, projectID, name)
	if _, err := testPool.Exec(t.Context(), "UPDATE security_groups SET stateful = $1 WHERE id = $2", false, id); err != nil {
		t.Fatalf("make %s stateless: %v", name, err)
	}
	return id
}

func TestAddSecurityGroup_StatefulnessConflict(t *testing.T) {
	requireDB(t)

	t.Run("a stateless group on a server whose ports are stateful", func(t *testing.T) {
		r, rec := newSGActionRig(t)
		tenant := tenantProject(t)
		hdr := tenantHdr(tenant)
		web := createTestGroup(t, tenant, "spec033-sf-"+uuid.NewString()[:6])
		sl := statelessGroup(t, tenant, "spec033-sl-"+uuid.NewString()[:6])
		server, ports := serverWithPorts(t, r, rec, tenant, 2)
		setGroups(t, ports[0], web)
		setGroups(t, ports[1], web)

		code, body := sgAction(t, r, hdr, server, "addSecurityGroup", groupName(t, sl))
		wantStatus(t, "add a stateless group", code, body, http.StatusConflict)
		for _, p := range ports {
			if got := groupsOf(t, p); !slices.Equal(got, []string{web}) {
				t.Errorf("port %s after the refusal: %v; want [web] unchanged", p, got)
			}
		}
		if calls := rec.recorded(); len(calls) != 0 {
			t.Errorf("RefreshPortFilters calls %v; want none for a refused change", calls)
		}
	})

	t.Run("ports that already differ, each staying single-kind", func(t *testing.T) {
		r, rec := newSGActionRig(t)
		tenant := tenantProject(t)
		hdr := tenantHdr(tenant)
		sl1 := statelessGroup(t, tenant, "spec033-sl1-"+uuid.NewString()[:6])
		sl2 := statelessGroup(t, tenant, "spec033-sl2-"+uuid.NewString()[:6])
		server, ports := serverWithPorts(t, r, rec, tenant, 2)
		setGroups(t, ports[0], sl1)
		setGroups(t, ports[1]) // none

		code, body := sgAction(t, r, hdr, server, "addSecurityGroup", groupName(t, sl2))
		wantStatus(t, "add a second stateless group", code, body, http.StatusAccepted)
		if got := groupsOf(t, ports[0]); !slices.Equal(got, ids(sl1, sl2)) {
			t.Errorf("first port: %v; want both stateless groups", got)
		}
		if got := groupsOf(t, ports[1]); !slices.Equal(got, []string{sl2}) {
			t.Errorf("second port: %v; want the stateless group", got)
		}
	})
}

// An interface attach is never refused for statefulness: a port attached by id keeps its own groups,
// whatever the server's other ports carry (FR-013).
func TestInterfaceAttach_OtherKindKeepsItsGroups(t *testing.T) {
	requireDB(t)
	r, rec := newSGActionRig(t)
	tenant := tenantProject(t)
	hdr := tenantHdr(tenant)
	web := createTestGroup(t, tenant, "spec033-sf-"+uuid.NewString()[:6])
	sl := statelessGroup(t, tenant, "spec033-sl-"+uuid.NewString()[:6])
	server, ports := serverWithPorts(t, r, rec, tenant, 1)
	setGroups(t, ports[0], web)
	net2, _ := createTestNetwork(t, tenant)
	free := freePortWithGroups(t, net2, tenant, sl)

	if got := attachInterface(t, r, hdr, server, `{"interfaceAttachment":{"port_id":"`+free+`"}}`); got != free {
		t.Fatalf("attached port %s; want %s", got, free)
	}
	if got := groupsOf(t, free); !slices.Equal(got, []string{sl}) {
		t.Errorf("the attached port: %v; want its stateless group, unchanged", got)
	}
	if got := groupsOf(t, ports[0]); !slices.Equal(got, []string{web}) {
		t.Errorf("the server's first port: %v; want web, unchanged", got)
	}
}

// A boot whose requested groups mix both kinds fails as upstream's build does when Neutron refuses the
// port: the server ends ERROR with a fault.
func TestBootWithMixedStatefulnessEndsInError(t *testing.T) {
	requireDB(t)
	network := neutron.NewService(testPool, "stub", nil)
	_, r := newBootService(t, network)
	netID, _ := createTestNetwork(t, seededProjectID)
	web := createTestGroup(t, seededProjectID, "spec033-boot-sf-"+uuid.NewString()[:6])
	sl := statelessGroup(t, seededProjectID, "spec033-boot-sl-"+uuid.NewString()[:6])

	const name = "spec033-boot-mixed"
	cleanupServerNamed(t, name)
	body := fmt.Sprintf(`{"server":{"name":%q,"flavorRef":"m1.small","imageRef":%q,"networks":[{"uuid":%q}],"security_groups":[{"name":%q},{"name":%q}]}}`,
		name, seededImageID(t), netID, groupName(t, web), groupName(t, sl))
	if code, resp := doReq(r, http.MethodPost, "/v2.1/servers", body, bootHdr()); code != http.StatusAccepted {
		t.Fatalf("create: code=%d body=%s; want 202 — the conflict is the port allocation's, after the answer", code, resp)
	}
	if got := awaitInstanceStatus(t, name, "ERROR"); got != "ERROR" {
		t.Fatalf("status = %q; want ERROR", got)
	}
	var message *string
	if err := testPool.QueryRow(t.Context(),
		`SELECT fault_message FROM instances WHERE name = $1 AND project_id = $2`, name, seededProjectID).Scan(&message); err != nil {
		t.Fatalf("read the fault: %v", err)
	}
	// On this host a boot fails later anyway (no staged image), so the fault must name the conflict:
	// upstream Neutron's SecurityGroupConflict message, which the tenant may read.
	if message == nil || !strings.Contains(*message, "stateful and stateless") {
		t.Errorf("fault = %v; want the allocation's refusal, naming stateful and stateless groups", deref(message))
	}
	var ports int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*) FROM ports WHERE network_id = $1`, netID).Scan(&ports); err != nil {
		t.Fatal(err)
	}
	if ports != 0 {
		t.Errorf("%d port(s) left on the network; want the refused allocation to leave none", ports)
	}
}

func deref(s *string) string {
	if s == nil {
		return "<none>"
	}
	return *s
}
