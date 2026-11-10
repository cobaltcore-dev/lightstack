package nova

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// quotaProject creates a project of its own with the given hard limits (none: the code
// defaults) and an active image it owns, and removes them when the test ends. Deleting
// the project removes its instances and quota rows with it.
func quotaProject(t *testing.T, limits map[string]int) (project, image string) {
	t.Helper()
	ctx := context.Background()
	project = uuid.NewString()
	createTestProject(t, project, "quota-"+project[:8])
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM images WHERE project_id = $1`, project)
		_, _ = testPool.Exec(context.Background(), `DELETE FROM projects WHERE id = $1`, project)
	})
	for resource, limit := range limits {
		if _, err := testPool.Exec(ctx,
			`INSERT INTO quotas (project_id, resource, hard_limit) VALUES ($1, $2, $3)`,
			project, resource, limit); err != nil {
			t.Fatalf("set the %s quota: %v", resource, err)
		}
	}
	if err := testPool.QueryRow(ctx,
		`INSERT INTO images (name, project_id, status, visibility, disk_format, container_format)
		 VALUES ('fr028-img', $1, 'active', 'private', 'qcow2', 'bare') RETURNING id::text`,
		project).Scan(&image); err != nil {
		t.Fatalf("seed an image: %v", err)
	}
	return project, image
}

// seedQuotaInstances writes n instances of m1.tiny straight into project.
func seedQuotaInstances(t *testing.T, project string, n int) {
	t.Helper()
	for range n {
		if _, err := testPool.Exec(context.Background(),
			`INSERT INTO instances (name, project_id, user_id, flavor_id, status)
			 VALUES ('quota-filler', $1, $2, '00000000-0000-0000-0000-000000000010', 'ACTIVE')`,
			project, seededUserID); err != nil {
			t.Fatalf("seed an instance: %v", err)
		}
	}
}

func quotaHdr(project string) map[string]string {
	return map[string]string{"X-Test-Project": project, "X-Test-User": seededUserID, "X-Test-Roles": "member"}
}

func createBody(name, flavor, image string) string {
	return fmt.Sprintf(`{"server":{"name":%q,"flavorRef":%q,"imageRef":%q}}`, name, flavor, image)
}

// TestServerCreateQuotaRefusalIs403 pins FR-035a: a create over the instances, cores or
// RAM limit answers 403 with upstream's message (TooManyInstances, nova/exception.py),
// as upstream answers it on create — not 413 overLimit.
func TestServerCreateQuotaRefusalIs403(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()
	for _, tc := range []struct {
		name   string
		limits map[string]int
		filled int // instances already there
		flavor string
		want   string
	}{
		{"instances", map[string]int{"instances": 1}, 1, "m1.tiny",
			"Quota exceeded for instances: Requested 1, but already used 1 of 1 instances"},
		{"cores", map[string]int{"cores": 2}, 1, "m1.medium",
			"Quota exceeded for cores: Requested 2, but already used 1 of 2 cores"},
		{"ram", map[string]int{"ram": 4096}, 1, "m1.medium",
			"Quota exceeded for ram: Requested 4096, but already used 512 of 4096 ram"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project, image := quotaProject(t, tc.limits)
			seedQuotaInstances(t, project, tc.filled)
			code, body := doReq(r, http.MethodPost, "/v2.1/servers", createBody("fr028-quota", tc.flavor, image), quotaHdr(project))
			want := fmt.Sprintf(`{"forbidden":{"code":403,"message":%q}}`, tc.want)
			if code != http.StatusForbidden || body != want {
				t.Errorf("create = %d %s;\n want 403 %s", code, body, want)
			}
		})
	}
}

// TestServerCreateQuotaNamesEveryExceededResource: when a create exceeds more than one
// quota at once, the refusal names them all, sorted, each field joined by ", " — as
// upstream does (nova/compute/utils.py get_over_quota_detail), not just the first.
func TestServerCreateQuotaNamesEveryExceededResource(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()
	// Limits cores=2, ram=4096; one m1.tiny already uses 1 core / 512 MB. An m1.medium
	// request (2 cores, 4096 MB) then exceeds cores (1+2 > 2) and ram (512+4096 > 4096) at
	// once.
	project, image := quotaProject(t, map[string]int{"cores": 2, "ram": 4096})
	seedQuotaInstances(t, project, 1)
	code, body := doReq(r, http.MethodPost, "/v2.1/servers", createBody("fr028-multi", "m1.medium", image), quotaHdr(project))
	const want = `{"forbidden":{"code":403,"message":"Quota exceeded for cores, ram: Requested 2, 4096, but already used 1, 512 of 2, 4096 cores, ram"}}`
	if code != http.StatusForbidden || body != want {
		t.Errorf("create = %d %s;\n want 403 %s", code, body, want)
	}
}

// TestServerCreateUnlimitedQuotaAdmits: a negative limit is upstream's "unlimited"
// (`openstack quota set --instances -1`, Horizon's "unlimited"), so a create is admitted
// however many instances already exist — not refused with "of -1 instances" (SRC-10).
func TestServerCreateUnlimitedQuotaAdmits(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()
	project, image := quotaProject(t, map[string]int{"instances": -1})
	seedQuotaInstances(t, project, 3)
	code, body := doReq(r, http.MethodPost, "/v2.1/servers", createBody("fr028-unlimited", "m1.tiny", image), quotaHdr(project))
	if code != http.StatusAccepted {
		t.Errorf("create with an unlimited (-1) instances quota = %d %s; want 202", code, body)
	}
}

// TestServerCreateQuotaHoldsUnderConcurrency pins SC-005 and FR-035: two creates racing
// for a project's last instance end as exactly one 202 and one 403, every time — for a
// project on the code defaults, which has no quotas row to lock, and for one with a row.
func TestServerCreateQuotaHoldsUnderConcurrency(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()
	for _, tc := range []struct {
		name   string
		limits map[string]int
		filled int
	}{
		{"code defaults", nil, quotaDefaults["instances"] - 1},
		{"quotas row", map[string]int{"instances": 2}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for run := range 20 {
				project, image := quotaProject(t, tc.limits)
				seedQuotaInstances(t, project, tc.filled)
				codes := make([]int, 2)
				start := make(chan struct{})
				var wg sync.WaitGroup
				for i := range codes {
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						codes[i], _ = doReq(r, http.MethodPost, "/v2.1/servers",
							createBody(fmt.Sprintf("fr028-race-%d", i), "m1.tiny", image), quotaHdr(project))
					}()
				}
				close(start)
				wg.Wait()
				accepted, refused := 0, 0
				for _, c := range codes {
					switch c {
					case http.StatusAccepted:
						accepted++
					case http.StatusForbidden:
						refused++
					}
				}
				if accepted != 1 || refused != 1 {
					t.Fatalf("run %d: codes %v; want one 202 and one 403", run+1, codes)
				}
			}
		})
	}
}
