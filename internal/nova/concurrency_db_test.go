package nova

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/internal/database/dbtest"
	"github.com/google/uuid"
)

// TestNovaConcurrencyMatrix runs compute's read-then-write transactions under concurrent
// requests, once per SQLite transaction mode (FR-039). The table it logs decides the
// default (FR-010, research R4); the default must hold in every case.
func TestNovaConcurrencyMatrix(t *testing.T) {
	requireDB(t)
	const n = 8
	seedServer := func(t *testing.T, project string) string {
		id := uuid.NewString()
		if _, err := testPool.Exec(context.Background(),
			`INSERT INTO instances (id, name, project_id, user_id, flavor_id, status)
			 VALUES ($1, 'conc', $2, $3, '00000000-0000-0000-0000-000000000010', 'ACTIVE')`, id, project, seededUserID); err != nil {
			t.Fatalf("seed a server: %v", err)
		}
		return id
	}
	cases := []dbtest.ConcurrencyCase{
		{Name: "quota check and server create", Run: func(t *testing.T, db database.DB) string {
			project, image := quotaProject(t, map[string]int{"instances": 3})
			r := novaRouterOn(db)
			codes := make([]int, n)
			dbtest.Concurrently(n, func(i int) {
				codes[i], _ = doReq(r, http.MethodPost, "/v2.1/servers",
					createBody(fmt.Sprintf("conc-%d", i), "m1.tiny", image), quotaHdr(project))
			})
			return dbtest.Outcome(codes, map[string]int{"2xx": 3, "403": n - 3})
		}},
		{Name: "server delete", Run: func(t *testing.T, db database.DB) string {
			project, _ := quotaProject(t, nil)
			server := seedServer(t, project)
			r := novaRouterOn(db)
			codes := make([]int, n)
			dbtest.Concurrently(n, func(i int) {
				codes[i], _ = doReq(r, http.MethodDelete, "/v2.1/servers/"+server, "", quotaHdr(project))
			})
			return dbtest.Outcome(codes, map[string]int{"2xx": 1, "404": n - 1})
		}},
		{Name: "server metadata replacement", Run: func(t *testing.T, db database.DB) string {
			project, _ := quotaProject(t, nil)
			server := seedServer(t, project)
			r := novaRouterOn(db)
			codes := make([]int, n)
			dbtest.Concurrently(n, func(i int) {
				codes[i], _ = doReq(r, http.MethodPut, "/v2.1/servers/"+server+"/metadata",
					fmt.Sprintf(`{"metadata":{"writer":"%d"}}`, i), quotaHdr(project))
			})
			return dbtest.Outcome(codes, map[string]int{"2xx": n})
		}},
		{Name: "aggregate metadata merge", Run: func(t *testing.T, db database.DB) string {
			var id string
			if err := testPool.QueryRow(context.Background(),
				`INSERT INTO host_aggregates (name) VALUES ($1) RETURNING uuid::text`, "conc-"+uuid.NewString()[:8]).Scan(&id); err != nil {
				t.Fatalf("seed an aggregate: %v", err)
			}
			t.Cleanup(func() { _, _ = testPool.Exec(context.Background(), `DELETE FROM host_aggregates WHERE uuid = $1`, id) })
			r := novaRouterOn(db)
			admin := testAuthHdr(seededProjectID, seededUserID)
			codes := make([]int, n)
			dbtest.Concurrently(n, func(i int) {
				codes[i], _ = doReq(r, http.MethodPost, "/v2.1/os-aggregates/"+id+"/action",
					fmt.Sprintf(`{"set_metadata":{"metadata":{"k%d":"v"}}}`, i), admin)
			})
			if failure := dbtest.Outcome(codes, map[string]int{"2xx": n}); failure != "" {
				return failure
			}
			_, body := doReq(r, http.MethodGet, "/v2.1/os-aggregates/"+id, "", admin)
			var got struct {
				Aggregate struct {
					Metadata map[string]any `json:"metadata"`
				} `json:"aggregate"`
			}
			if err := json.Unmarshal([]byte(body), &got); err != nil || len(got.Aggregate.Metadata) != n {
				return fmt.Sprintf("%d of %d keys kept", len(got.Aggregate.Metadata), n)
			}
			return ""
		}},
	}
	for name, failure := range dbtest.RunConcurrencyMatrix(t, cases)["immediate"] {
		if failure != "" {
			t.Errorf("immediate transactions, %s: %s", name, failure)
		}
	}
}
