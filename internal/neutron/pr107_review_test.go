package neutron

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/google/uuid"
)

type failInterfaceDB struct{ database.DB }
type failInterfaceTx struct{ database.Tx }

func (d failInterfaceDB) Begin(ctx context.Context) (database.Tx, error) {
	tx, err := d.DB.Begin(ctx)
	return failInterfaceTx{tx}, err
}
func (tx failInterfaceTx) Exec(ctx context.Context, query string, args ...any) (database.Result, error) {
	if strings.Contains(query, "INSERT INTO router_interfaces") {
		return nil, errors.New("injected interface failure")
	}
	return tx.Tx.Exec(ctx, query, args...)
}

func TestRouterAttachmentRollbackAndExhaustion(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	netID, subnetID, _ := seedSpec028NetSubnet(t, ctx, "review")
	t.Cleanup(func() { cleanupSpec028Net(t, ctx, netID) })
	routerID, portID := uuid.NewString(), uuid.NewString()
	if _, err := testPool.Exec(ctx, "INSERT INTO routers (id,name,project_id) VALUES ($1,$2,$3)", routerID, routerID, seededProjectID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testPool.Exec(ctx, "DELETE FROM routers WHERE id=$1", routerID) })
	if _, err := testPool.Exec(ctx, "UPDATE subnets SET cidr='10.99.0.5/32', gateway_ip=NULL WHERE id=$1", subnetID); err != nil {
		t.Fatal(err)
	}
	fixed := fmt.Sprintf(`[{"subnet_id":"%s","ip_address":"10.99.0.5"}]`, subnetID)
	if _, err := testPool.Exec(ctx, "INSERT INTO ports (id,name,network_id,project_id,mac_address,fixed_ips) VALUES ($1,$2,$3,$4,$5,$6::jsonb)", portID, portID, netID, seededProjectID, "02:00:00:00:98:01", fixed); err != nil {
		t.Fatal(err)
	}
	r, svc := newNeutronTestRouterWithService()
	hdr := map[string]string{"X-Test-Project": seededProjectID, "X-Test-Roles": "member"}
	path := "/v2.0/routers/" + routerID + "/add_router_interface"
	body := fmt.Sprintf(`{"subnet_id":"%s"}`, subnetID)
	if code, resp := doReqBody(r, http.MethodPut, path, body, hdr); code != 409 {
		t.Fatalf("exhausted subnet: %d %s", code, resp)
	}
	body = fmt.Sprintf(`{"port_id":"%s","subnet_id":"%s"}`, portID, subnetID)
	if code, resp := doReqBody(r, http.MethodPut, path, body, hdr); code != 400 {
		t.Fatalf("ambiguous selector: %d %s", code, resp)
	}
	svc.pool = failInterfaceDB{testPool}
	body = fmt.Sprintf(`{"port_id":"%s"}`, portID)
	if code, resp := doReqBody(r, http.MethodPut, path, body, hdr); code != 500 {
		t.Fatalf("injected failure: %d %s", code, resp)
	}
	var free bool
	if err := testPool.QueryRow(ctx, "SELECT device_id IS NULL AND device_owner IS NULL FROM ports WHERE id=$1", portID).Scan(&free); err != nil || !free {
		t.Fatalf("port not rolled back: free=%v err=%v", free, err)
	}
	svc.pool = testPool
	if code, resp := doReqBody(r, http.MethodPut, path, body, hdr); code != 200 {
		t.Fatalf("existing port on full subnet: %d %s", code, resp)
	}
}
