package nova

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// TestAggregateMetadataMergesConcurrentUpdates: set_metadata merges into the stored
// metadata, and updates racing for one aggregate keep every key.
func TestAggregateMetadataMergesConcurrentUpdates(t *testing.T) {
	requireDB(t)
	name := "spec028-merge-" + uuid.NewString()[:8]
	var id string
	if err := testPool.QueryRow(context.Background(),
		`INSERT INTO host_aggregates (name, availability_zone, metadata) VALUES ($1, 'nova', '{"kept":"yes"}'::jsonb)
		 RETURNING uuid::text`, name).Scan(&id); err != nil {
		t.Fatalf("seed aggregate: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), `DELETE FROM host_aggregates WHERE uuid = $1`, id)
	})

	r := newNovaTestRouter()
	admin := testAuthHdr(seededProjectID, seededUserID)
	const writers, rounds = 4, 5
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range rounds {
				body := fmt.Sprintf(`{"set_metadata":{"metadata":{"w%d-%d":"v"}}}`, w, i)
				if code, resp := doReq(r, http.MethodPost, "/v2.1/os-aggregates/"+id+"/action", body, admin); code != http.StatusOK {
					t.Errorf("set_metadata %s: code=%d body=%s", body, code, resp)
				}
			}
		}()
	}
	wg.Wait()

	code, body := doReq(r, http.MethodGet, "/v2.1/os-aggregates/"+id, "", admin)
	if code != http.StatusOK {
		t.Fatalf("show: code=%d body=%s", code, body)
	}
	var got struct {
		Aggregate struct {
			Metadata map[string]any `json:"metadata"`
		} `json:"aggregate"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("decode: %v; body: %s", err, body)
	}
	if len(got.Aggregate.Metadata) != writers*rounds+1 || got.Aggregate.Metadata["kept"] != "yes" {
		t.Errorf("metadata has %d keys (%v); want %d: every update's key and the stored one",
			len(got.Aggregate.Metadata), got.Aggregate.Metadata, writers*rounds+1)
	}
}
