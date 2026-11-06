package nova

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/neutron"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// All three port writers share one last address. An exhausted request must
// fail without creating a second port holding it.
func TestConcurrentPortWritersReserveOneAddress(t *testing.T) {
	requireDB(t)
	serverID := seedConsoleInstance(t, seededProjectID)
	networkID, subnetID := uuid.NewString(), uuid.NewString()
	if _, err := testPool.Exec(t.Context(), `INSERT INTO networks (id, name, project_id, status)
		VALUES ($1, 'concurrent-ip', $2, 'ACTIVE')`, networkID, seededProjectID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM interface_attachments WHERE port_id IN (SELECT id FROM ports WHERE network_id=$1)`, networkID)
		_, _ = testPool.Exec(context.Background(), "DELETE FROM networks WHERE id=$1", networkID)
	})
	if _, err := testPool.Exec(t.Context(), `INSERT INTO subnets (id, name, network_id, project_id, cidr)
		VALUES ($1, 'one-address', $2, $3, '192.0.2.7/32')`, subnetID, networkID, seededProjectID); err != nil {
		t.Fatal(err)
	}
	if _, err := neutron.EnsureDefaultSecurityGroup(t.Context(), testPool, seededProjectID); err != nil {
		t.Fatal(err)
	}
	novaRouter := newNovaTestRouter()
	networkSvc := neutron.NewService(testPool, "stub", nil)
	portRouter := gin.New()
	portRouter.Use(func(c *gin.Context) {
		c.Set("project_id", seededProjectID)
		c.Set("roles", []string{"member"})
	})
	networkSvc.RegisterRoutes(&portRouter.RouterGroup)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	const count = 12
	start := make(chan struct{})
	type result struct {
		code int
		body string
	}
	results := make(chan result, count)
	for i := range count {
		go func() {
			<-start
			if i%3 == 0 {
				_, err := networkSvc.AllocatePortForInstance(ctx, networkID, seededProjectID, serverID, nil, false)
				if err == nil {
					results <- result{http.StatusCreated, ""}
					return
				}
				code := http.StatusInternalServerError
				if strings.Contains(err.Error(), "failed to allocate a free IP") {
					code = http.StatusConflict
				}
				results <- result{code, err.Error()}
				return
			}
			router, path, body := portRouter, "/v2.0/ports", fmt.Sprintf(`{"port":{"network_id":%q}}`, networkID)
			if i%3 == 1 {
				router, path = novaRouter, "/v2.1/servers/"+serverID+"/os-interface"
				body = fmt.Sprintf(`{"interfaceAttachment":{"net_id":%q,"fixed_ip":"192.0.2.7"}}`, networkID)
			}
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			for k, v := range testAuthHdr(seededProjectID, seededUserID) {
				req.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			results <- result{w.Code, w.Body.String()}
		}()
	}
	close(start)
	successes := 0
	for range count {
		r := <-results
		if r.code == http.StatusOK || r.code == http.StatusCreated {
			successes++
		} else if r.code != http.StatusConflict {
			t.Errorf("unexpected response %d %s", r.code, r.body)
		}
	}
	var ports int
	if err := testPool.QueryRow(t.Context(), "SELECT count(*) FROM ports WHERE network_id=$1", networkID).Scan(&ports); err != nil {
		t.Fatal(err)
	}
	if successes != 1 || ports != 1 {
		t.Errorf("successes=%d ports=%d; want one address claimed exactly once", successes, ports)
	}
}
