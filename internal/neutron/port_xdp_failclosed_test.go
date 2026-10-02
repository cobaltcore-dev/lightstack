package neutron

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cobaltcore-dev/lightstack/pkg/networking"
	"github.com/gin-gonic/gin"
)

// TestCreatePortFailsClosedWhenXDPAttachFails: in eBPF mode the XDP program on a port's
// TAP is what checks the frames that port sends against each destination's rules. If it
// cannot be attached, the port must not be created: a 201 would hand back an interface
// whose traffic skips every destination's security groups.
//
// A stub-mode SecurityGroupManager refuses every eBPF call ("only supported in eBPF
// mode"), which makes the attach fail the way a real one can (no XDP support in the
// driver, the hook already taken).
func TestCreatePortFailsClosedWhenXDPAttachFails(t *testing.T) {
	requireDB(t)
	ctx := context.Background()

	svc := NewService(testPool, "stub", nil)
	svc.mode = "ebpf"
	mgr, err := networking.NewSecurityGroupManager("stub", "")
	if err != nil {
		t.Fatalf("stub security group manager: %v", err)
	}
	svc.sgManager = mgr

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("project_id", seededProjectID) })
	svc.RegisterRoutes(&r.RouterGroup)

	// Through the API, so the network's bridge exists. The eBPF path only runs for a port
	// with a fixed IP, so the network also gets a subnet.
	code, body := doReqBody(r, http.MethodPost, "/v2.0/networks", `{"network":{"name":"xdp-failclosed"}}`, nil)
	if code != http.StatusCreated {
		t.Fatalf("create network: code=%d body=%s", code, body)
	}
	var created struct {
		Network struct {
			ID string `json:"id"`
		} `json:"network"`
	}
	if err := json.Unmarshal([]byte(body), &created); err != nil || created.Network.ID == "" {
		t.Fatalf("create network: no id in %s (%v)", body, err)
	}
	netID := created.Network.ID
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM subnets WHERE network_id = $1`, netID)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM networks WHERE id = $1`, netID)
	})
	code, body = doReqBody(r, http.MethodPost, "/v2.0/subnets",
		`{"subnet":{"network_id":"`+netID+`","cidr":"10.66.0.0/24","ip_version":4,"enable_dhcp":false}}`, nil)
	if code != http.StatusCreated {
		t.Fatalf("create subnet: code=%d body=%s", code, body)
	}

	code, body = doReqBody(r, http.MethodPost, "/v2.0/ports", `{"port":{"network_id":"`+netID+`"}}`, nil)
	if code != http.StatusInternalServerError {
		t.Fatalf("POST /v2.0/ports with a failing XDP attach: code=%d body=%s; want 500 (fail closed)", code, body)
	}
	var left int
	if err := testPool.QueryRow(ctx, `SELECT COUNT(*) FROM ports WHERE network_id = $1`, netID).Scan(&left); err != nil {
		t.Fatalf("count ports: %v", err)
	}
	if left != 0 {
		t.Errorf("%d port row(s) left on the network; want the port rolled back", left)
	}
}
