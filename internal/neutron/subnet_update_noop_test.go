package neutron

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// Second-review finding AGY-2 — UpdateSubnet issued `UPDATE subnets SET name = $1 WHERE id = $2`
// with the name it had just read, so an update supplying no fields wrote the row back to itself.
//
// The second review proposed refusing such a request with 400. That was rejected: upstream Neutron
// accepts an empty update and answers 200 with the unchanged resource, and this project's first
// principle is that existing Terraform and Horizon flows work unmodified. The fix is to skip the
// write, not the request — so this test pins BOTH halves, because a fix that returns 400 would look
// like it addressed the finding while breaking compatibility.
func TestUpdateSubnetWithNoFieldsSucceedsAndChangesNothing(t *testing.T) {
	requireDB(t)
	r := newNeutronTestRouter()
	ctx := context.Background()

	netID := seedExternalNetwork(t, "noop-net-"+uuid.NewString()[:8], seededProjectID, false, false)
	subnetID := uuid.New().String()
	const name = "noop-sub"
	if _, err := testPool.Exec(ctx, `
		INSERT INTO subnets (id, name, network_id, project_id, cidr, gateway_ip, ip_version, enable_dhcp, created_at, updated_at)
		VALUES ($1, $2, $3, $4, '10.77.0.0/24', '10.77.0.1', 4, true, now(), now())
	`, subnetID, name, netID, seededProjectID); err != nil {
		t.Fatalf("seed subnet: %v", err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, "DELETE FROM subnets WHERE id = $1", subnetID) //nolint:errcheck // best-effort cleanup
	})

	code, body := doReqBody(r, http.MethodPut, "/v2.0/subnets/"+subnetID, `{"subnet":{}}`,
		neutronAuthHdr(seededProjectID, false))

	// Upstream answers 200 for an empty update. A 400 here is the compatibility regression the
	// second review's suggested fix would have introduced.
	if code != http.StatusOK {
		t.Fatalf("PUT with an empty subnet body = %d, want 200 — upstream accepts an empty update "+
			"and Terraform/Horizon rely on it (body %q)", code, body)
	}

	// And the row is untouched: same name, same gateway.
	var gotName string
	var gotGateway *string
	if err := testPool.QueryRow(ctx,
		"SELECT name, gateway_ip FROM subnets WHERE id = $1", subnetID).Scan(&gotName, &gotGateway); err != nil {
		t.Fatalf("read back subnet: %v", err)
	}
	if gotName != name {
		t.Errorf("name = %q after an empty update; want %q unchanged", gotName, name)
	}
	if gotGateway == nil || *gotGateway != "10.77.0.1" {
		t.Errorf("gateway_ip = %v after an empty update; want 10.77.0.1 unchanged", gotGateway)
	}
}
