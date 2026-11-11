package cinder

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/internal/database/dbtest"
	"github.com/google/uuid"
)

// concurrentRequests is how many requests each case sends at once.
const concurrentRequests = 8

// TestCinderConcurrencyMatrix runs block storage's read-then-write transactions under
// concurrent requests, once per SQLite transaction mode (FR-039). The table it logs
// decides the default (FR-010, research R4); the default must hold in every case.
func TestCinderConcurrencyMatrix(t *testing.T) {
	requireDB(t)
	const n = concurrentRequests
	seedServer := func(t *testing.T, project string) string {
		id := uuid.NewString()
		if _, err := testPool.Exec(context.Background(),
			`INSERT INTO instances (id, name, project_id, user_id, status) VALUES ($1, 'conc', $2, $3, 'ACTIVE')`,
			id, project, seededUserID); err != nil {
			t.Fatalf("seed a server: %v", err)
		}
		return id
	}
	seedAvailable := func(t *testing.T, project string) string {
		id := uuid.NewString()
		if _, err := testPool.Exec(context.Background(),
			`INSERT INTO volumes (id, name, project_id, user_id, size_gb, status) VALUES ($1, 'conc', $2, $3, 1, 'available')`,
			id, project, seededUserID); err != nil {
			t.Fatalf("seed a volume: %v", err)
		}
		return id
	}
	cases := []dbtest.ConcurrencyCase{
		{Name: "quota helper (volume create)", Run: func(t *testing.T, db database.DB) string {
			project := cinderProject(t, map[string]int{"volumes": 3})
			r := cinderRouter(NewService(db, "stub", "", "", testNodeName))
			codes := make([]int, n)
			dbtest.Concurrently(n, func(i int) {
				codes[i], _ = doReq(r, http.MethodPost, "/v3/volumes", volumeCreateBody(fmt.Sprintf("conc-%d", i), ""), cinderAuthHdr(project, seededUserID))
			})
			return dbtest.Outcome(codes, map[string]int{"2xx": 3, "413": n - 3})
		}},
		{Name: "attach", Run: func(t *testing.T, db database.DB) string {
			project := cinderProject(t, nil)
			volume, server := seedAvailable(t, project), seedServer(t, project)
			r := cinderRouter(NewService(db, "stub", "", "", testNodeName))
			codes := make([]int, n)
			dbtest.Concurrently(n, func(i int) {
				codes[i], _ = doReq(r, http.MethodPost, "/v3/volumes/"+volume+"/action",
					fmt.Sprintf(`{"os-attach":{"instance_uuid":%q,"mountpoint":"/dev/vdb"}}`, server), cinderAuthHdr(project, seededUserID))
			})
			return dbtest.Outcome(codes, map[string]int{"2xx": 1, "400": n - 1})
		}},
		{Name: "detach", Run: func(t *testing.T, db database.DB) string {
			project := cinderProject(t, nil)
			volume, server := seedAvailable(t, project), seedServer(t, project)
			r := cinderRouter(NewService(db, "stub", "", "", testNodeName))
			hdr := cinderAuthHdr(project, seededUserID)
			if code, body := doReq(r, http.MethodPost, "/v3/volumes/"+volume+"/action",
				fmt.Sprintf(`{"os-attach":{"instance_uuid":%q,"mountpoint":"/dev/vdb"}}`, server), hdr); code != http.StatusAccepted {
				return fmt.Sprintf("attach before the detaches: %d %s", code, body)
			}
			codes := make([]int, n)
			dbtest.Concurrently(n, func(i int) {
				codes[i], _ = doReq(r, http.MethodPost, "/v3/volumes/"+volume+"/action", `{"os-detach":{}}`, hdr)
			})
			return dbtest.Outcome(codes, map[string]int{"2xx": n})
		}},
		{Name: "volume metadata replacement", Run: func(t *testing.T, db database.DB) string {
			project := cinderProject(t, nil)
			volume := seedAvailable(t, project)
			r := cinderRouter(NewService(db, "stub", "", "", testNodeName))
			codes := make([]int, n)
			dbtest.Concurrently(n, func(i int) {
				codes[i], _ = doReq(r, http.MethodPost, "/v3/volumes/"+volume+"/metadata",
					fmt.Sprintf(`{"metadata":{"writer":"%d"}}`, i), cinderAuthHdr(project, seededUserID))
			})
			return dbtest.Outcome(codes, map[string]int{"2xx": n})
		}},
		{Name: "snapshot metadata replacement", Run: func(t *testing.T, db database.DB) string {
			project := cinderProject(t, nil)
			volume := seedAvailable(t, project)
			snapshot := uuid.NewString()
			if _, err := testPool.Exec(context.Background(),
				`INSERT INTO snapshots (id, name, volume_id, project_id, size_gb, status) VALUES ($1, 'conc', $2, $3, 1, 'available')`,
				snapshot, volume, project); err != nil {
				t.Fatalf("seed a snapshot: %v", err)
			}
			r := cinderRouter(NewService(db, "stub", "", "", testNodeName))
			codes := make([]int, n)
			dbtest.Concurrently(n, func(i int) {
				codes[i], _ = doReq(r, http.MethodPost, "/v3/snapshots/"+snapshot+"/metadata",
					fmt.Sprintf(`{"metadata":{"writer":"%d"}}`, i), cinderAuthHdr(project, seededUserID))
			})
			return dbtest.Outcome(codes, map[string]int{"2xx": n})
		}},
		{Name: "transfer create", Run: func(t *testing.T, db database.DB) string {
			project := cinderProject(t, nil)
			volume := seedAvailable(t, project)
			r := cinderRouter(NewService(db, "stub", "", "", testNodeName))
			codes := make([]int, n)
			dbtest.Concurrently(n, func(i int) {
				codes[i], _ = doReq(r, http.MethodPost, "/v3/os-volume-transfer",
					fmt.Sprintf(`{"transfer":{"volume_id":%q}}`, volume), cinderAuthHdr(project, seededUserID))
			})
			return dbtest.Outcome(codes, map[string]int{"2xx": 1, "400": n - 1})
		}},
		{Name: "transfer delete", Run: func(t *testing.T, db database.DB) string {
			project := cinderProject(t, nil)
			volume := seedAvailable(t, project)
			r := cinderRouter(NewService(db, "stub", "", "", testNodeName))
			hdr := cinderAuthHdr(project, seededUserID)
			code, body := doReq(r, http.MethodPost, "/v3/os-volume-transfer", fmt.Sprintf(`{"transfer":{"volume_id":%q}}`, volume), hdr)
			if code != http.StatusAccepted {
				return fmt.Sprintf("transfer create before the deletes: %d %s", code, body)
			}
			transfer := strings.Split(strings.SplitN(body, `"id":"`, 2)[1], `"`)[0]
			codes := make([]int, n)
			dbtest.Concurrently(n, func(i int) {
				codes[i], _ = doReq(r, http.MethodDelete, "/v3/os-volume-transfer/"+transfer, "", hdr)
			})
			return dbtest.Outcome(codes, map[string]int{"2xx": 1, "404": n - 1})
		}},
	}
	for name, failure := range dbtest.RunConcurrencyMatrix(t, cases)["immediate"] {
		if failure != "" {
			t.Errorf("immediate transactions, %s: %s", name, failure)
		}
	}
}
