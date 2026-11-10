package nova

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database/dbtest"
)

// These tests drive the defensive error branches that log and answer 500 when a query
// fails, by making a table unavailable under the request.

// The binding quota check surfaces a failure to read the quota, rather than admitting the
// create.
func TestServerCreateQuotaReadError(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()
	project, image := quotaProject(t, map[string]int{"instances": 5})
	release := dbtest.Unavailable(t, testPool, "quotas")
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	code, body := doReqCtx(ctx, r, http.MethodPost, "/v2.1/servers", createBody("fr028-qerr", "m1.tiny", image), quotaHdr(project))
	if code != http.StatusInternalServerError {
		t.Errorf("create with quotas unavailable = %d %s; want 500", code, body)
	}
}

// The ip filter surfaces a failure to read the scope's ports.
func TestServerListIPFilterQueryError(t *testing.T) {
	requireDB(t)
	seedFilterFixtures(t)
	r := newNovaTestRouter()
	release := dbtest.Unavailable(t, testPool, "ports")
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	code, body := doReqCtx(ctx, r, http.MethodGet, "/v2.1/servers?ip=10.0.0.1", "", quotaHdr(filterProject))
	if code < 500 {
		t.Errorf("ip filter with ports unavailable = %d %s; want a 5xx", code, body)
	}
}

// Tenant usage surfaces a failure to read instances rather than reporting zeroes.
func TestTenantUsageReadError(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()
	project, _ := quotaProject(t, nil)
	release := dbtest.Unavailable(t, testPool, "instances")
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	code, body := doReqCtx(ctx, r, http.MethodGet, "/v2.1/os-simple-tenant-usage/"+project, "",
		map[string]string{"X-Test-Roles": "admin", "X-Test-Project": project, "X-Test-User": seededUserID})
	if code != http.StatusInternalServerError {
		t.Errorf("tenant usage with instances unavailable = %d %s; want 500", code, body)
	}
}
