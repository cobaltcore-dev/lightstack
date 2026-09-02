package nova

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// TestAttachInterfaceAppliesTheGuestAttachRule: attaching an interface to a running server uses
// the rule booting uses (neutron.GuestMayAttach). It used to accept only the server's own
// networks — refusing a shared one, which upstream allows — and knew nothing of external
// networks, which upstream reserves for administrators on os-interface exactly as on boot
// (network:attach_external_network). Answered as upstream's os-interface answers: 404 for a
// network the caller may not use, 403 for an unshared external one.
func TestAttachInterfaceAppliesTheGuestAttachRule(t *testing.T) {
	requireDB(t)
	seedAuthzFixtures(t) // projects A and B
	ctx := context.Background()
	r := newNovaTestRouter()

	network := func(name, owner string, shared, external bool) string {
		t.Helper()
		id := uuid.NewString()
		if _, err := testPool.Exec(ctx,
			`INSERT INTO networks (id, name, project_id, status, admin_state_up, shared, external)
			 VALUES ($1, $2, $3, 'ACTIVE', true, $4, $5)`, id, name, owner, shared, external); err != nil {
			t.Fatalf("seeding network %s: %v", name, err)
		}
		if _, err := testPool.Exec(ctx,
			`INSERT INTO subnets (id, name, network_id, project_id, cidr, gateway_ip, ip_version, enable_dhcp, created_at, updated_at)
			 VALUES ($1, $2, $3, $4, '10.248.0.0/24', '10.248.0.1', 4, false, now(), now())`,
			uuid.NewString(), name+"-subnet", id, owner); err != nil {
			t.Fatalf("seeding subnet for %s: %v", name, err)
		}
		t.Cleanup(func() {
			_, _ = testPool.Exec(context.Background(), `DELETE FROM interface_attachments WHERE port_id IN (SELECT id FROM ports WHERE network_id = $1)`, id)
			_, _ = testPool.Exec(context.Background(), `DELETE FROM ports WHERE network_id = $1`, id)
			_, _ = testPool.Exec(context.Background(), `DELETE FROM subnets WHERE network_id = $1`, id)
			_, _ = testPool.Exec(context.Background(), `DELETE FROM networks WHERE id = $1`, id)
		})
		return id
	}

	// The server belongs to project A; the caller is a member of A, or an administrator.
	serverA, _ := seedAuthzFixtures(t)
	member := map[string]string{"X-Test-Project": authzProjectA, "X-Test-User": seededUserID, "X-Test-Roles": "member"}
	admin := map[string]string{"X-Test-Project": authzProjectA, "X-Test-User": seededUserID, "X-Test-Roles": "admin"}

	own := network("spec025-iface-own", authzProjectA, false, false)
	foreignShared := network("spec025-iface-shared", authzProjectB, true, false)
	foreignExternal := network("spec025-iface-external", authzProjectB, false, true)
	foreignPrivate := network("spec025-iface-private", authzProjectB, false, false)

	for _, tc := range []struct {
		name    string
		network string
		hdr     map[string]string
		want    int
	}{
		{"own network", own, member, http.StatusOK},
		{"another project's shared network", foreignShared, member, http.StatusOK},
		{"an unshared external network, as a member", foreignExternal, member, http.StatusForbidden},
		{"an unshared external network, as an administrator", foreignExternal, admin, http.StatusOK},
		{"another project's private network", foreignPrivate, member, http.StatusNotFound},
		{"a network that does not exist", uuid.NewString(), member, http.StatusNotFound},
	} {
		code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+serverA+"/os-interface",
			fmt.Sprintf(`{"interfaceAttachment":{"net_id":%q}}`, tc.network), tc.hdr)
		if code != tc.want {
			t.Errorf("attach to %s = %d; want %d. body: %.200s", tc.name, code, tc.want, body)
		}
	}
}
