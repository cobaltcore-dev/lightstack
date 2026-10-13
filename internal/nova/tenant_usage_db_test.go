package nova

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestTenantUsageTotals pins the usage arithmetic of both os-simple-tenant-usage forms:
// the flavor sums and the hours since each instance was created, counted against one clock
// reading. An instance without a flavor still contributes its hours and adds nothing to the
// flavor sums.
func TestTenantUsageTotals(t *testing.T) {
	requireDB(t)
	ctx := t.Context()
	project := uuid.NewString()
	createTestProject(t, project, "usage-"+project[:8])
	t.Cleanup(func() {
		// Deleting the project cascades to its instances.
		_, _ = testPool.Exec(context.Background(), "DELETE FROM projects WHERE id = $1", project)
	})

	const tiny = "00000000-0000-0000-0000-000000000010" // seeded m1.tiny: 1 vCPU, 512 MB, 1 GB
	now := time.Now().UTC()
	for _, inst := range []struct {
		age    time.Duration
		flavor *string
	}{
		{2 * time.Hour, ptr(tiny)},
		{3 * time.Hour, ptr(tiny)},
		{1 * time.Hour, nil},
	} {
		if _, err := testPool.Exec(ctx,
			`INSERT INTO instances (id, name, project_id, user_id, flavor_id, status, created_at)
			 VALUES ($1, 'usage', $2, $3, $4, 'ACTIVE', $5)`,
			uuid.NewString(), project, seededUserID, inst.flavor, now.Add(-inst.age)); err != nil {
			t.Fatalf("insert instance: %v", err)
		}
	}

	type usage struct {
		TenantID    string  `json:"tenant_id"`
		TotalHours  float64 `json:"total_hours"`
		VCPUs       float64 `json:"total_vcpus_usage"`
		MemoryMB    float64 `json:"total_memory_mb_usage"`
		LocalGB     float64 `json:"total_local_gb_usage"`
		ServerUsage []any   `json:"server_usages"`
	}
	check := func(form string, u usage) {
		t.Helper()
		if math.Abs(u.TotalHours-6) > 0.05 {
			t.Errorf("%s: total_hours = %.3f; want 6 (2 + 3 + 1)", form, u.TotalHours)
		}
		if u.VCPUs != 2 || u.MemoryMB != 1024 || u.LocalGB != 2 {
			t.Errorf("%s: vcpus=%v memory=%v local_gb=%v; want 2, 1024, 2", form, u.VCPUs, u.MemoryMB, u.LocalGB)
		}
	}

	r := newNovaTestRouter()
	admin := testAuthHdr(project, seededUserID)

	code, body := doReq(r, http.MethodGet, "/v2.1/os-simple-tenant-usage/"+project, "", admin)
	if code != http.StatusOK {
		t.Fatalf("GET one project: code=%d body=%s", code, body)
	}
	var one struct {
		TenantUsage usage `json:"tenant_usage"`
	}
	if err := json.Unmarshal([]byte(body), &one); err != nil {
		t.Fatalf("decode: %v; body: %s", err, body)
	}
	check("show", one.TenantUsage)

	code, body = doReq(r, http.MethodGet, "/v2.1/os-simple-tenant-usage", "", admin)
	if code != http.StatusOK {
		t.Fatalf("GET all projects: code=%d body=%s", code, body)
	}
	var all struct {
		TenantUsages []usage `json:"tenant_usages"`
	}
	if err := json.Unmarshal([]byte(body), &all); err != nil {
		t.Fatalf("decode: %v; body: %s", err, body)
	}
	found := false
	for _, u := range all.TenantUsages {
		if u.TenantID == project {
			found = true
			check("index", u)
		}
	}
	if !found {
		t.Errorf("index does not list project %s; body: %.300s", project, body)
	}
}

func ptr[T any](v T) *T { return &v }
