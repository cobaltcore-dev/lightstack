package nova

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type failUnshelveDB struct{ database.DB }
type failUnshelveTx struct{ database.Tx }

func (d failUnshelveDB) Begin(ctx context.Context) (database.Tx, error) {
	tx, err := d.DB.Begin(ctx)
	return failUnshelveTx{tx}, err
}
func (tx failUnshelveTx) Exec(ctx context.Context, q string, args ...any) (database.Result, error) {
	if strings.Contains(q, "UPDATE instances") {
		return nil, errors.New("injected state failure")
	}
	return tx.Tx.Exec(ctx, q, args...)
}

func TestFailedUnshelveRetainsSnapshot(t *testing.T) {
	requireDB(t)
	id := seedSpec028Instance(t, "review-unshelve", false)
	r := newNovaTestRouter()
	payload, _ := json.Marshal(map[string]any{"shelve": nil})
	if code, body := doReq(r, http.MethodPost, "/v2.1/servers/"+id+"/action", string(payload), testAuthHdr(seededProjectID, seededUserID)); code != 202 {
		t.Fatalf("shelve: %d %s", code, body)
	}
	svc := &Service{pool: failUnshelveDB{testPool}}
	failing := gin.New()
	failing.POST("/unshelve", func(c *gin.Context) { svc.UnshelveInstance(c, id, seededProjectID) })
	if code, body := doReq(failing, http.MethodPost, "/unshelve", "", nil); code != 500 {
		t.Fatalf("unshelve: %d %s", code, body)
	}
	var count int
	if err := testPool.QueryRow(t.Context(), "SELECT COUNT(*) FROM instance_snapshots WHERE instance_id=$1", id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("lost snapshot: count=%d err=%v", count, err)
	}
}

func TestServerGroupRulesPersistAndValidate(t *testing.T) {
	requireDB(t)
	r := newNovaTestRouter()
	hdr := testAuthHdr(seededProjectID, seededUserID)
	name := "review-group-" + uuid.NewString()
	t.Cleanup(func() { testPool.Exec(context.Background(), "DELETE FROM server_groups WHERE name=$1", name) })
	for _, tc := range []struct {
		policy string
		rules  map[string]int
		code   int
	}{
		{"affinity", map[string]int{"max_server_per_host": 2}, 400},
		{"anti-affinity", map[string]int{"max_server_per_host": 0}, 400},
		{"anti-affinity", map[string]int{"unknown": 2}, 400},
		{"anti-affinity", map[string]int{"max_server_per_host": 2}, 200},
	} {
		payload, _ := json.Marshal(map[string]any{"server_group": map[string]any{"name": name, "policy": tc.policy, "rules": tc.rules}})
		code, body := doReq(r, http.MethodPost, "/v2.1/os-server-groups", string(payload), hdr)
		if code != tc.code {
			t.Fatalf("%+v: %d %s", tc, code, body)
		}
		if code != 200 {
			continue
		}
		var response struct {
			ServerGroup map[string]any `json:"server_group"`
		}
		if err := json.Unmarshal([]byte(body), &response); err != nil {
			t.Fatal(err)
		}
		id := response.ServerGroup["id"].(string)
		for _, path := range []string{"/v2.1/os-server-groups/" + id, "/v2.1/os-server-groups"} {
			code, body = doReq(r, http.MethodGet, path, "", hdr)
			if code != 200 {
				t.Fatalf("read: %d %s", code, body)
			}
			var doc map[string]any
			if err := json.Unmarshal([]byte(body), &doc); err != nil {
				t.Fatal(err)
			}
			group, ok := doc["server_group"].(map[string]any)
			if !ok {
				for _, item := range doc["server_groups"].([]any) {
					candidate := item.(map[string]any)
					if candidate["id"] == id {
						group = candidate
					}
				}
			}
			rules, ok := group["rules"].(map[string]any)
			if !ok || rules["max_server_per_host"] != float64(2) {
				t.Fatalf("rules lost: %s", body)
			}
		}
	}
}
