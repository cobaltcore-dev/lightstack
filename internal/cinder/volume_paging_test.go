package cinder

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/cobaltcore-dev/o3k/internal/database"
	"github.com/google/uuid"
)

// Marker paging over a non-total order (branch review, third-party finding).
//
// Both volume listings resolved a marker to `created_at < $n` while the detail listing ordered by
// `created_at DESC` alone. created_at is not unique — volumes created by one Terraform apply or a
// seed share it — so the order was undefined among ties and the predicate excluded every tied row
// rather than only those before the marker. Walking the pages dropped them.
//
// Nova fixed the same thing in #549 with a composite keyset matching a composite ORDER BY. Seeding
// an identical created_at is what makes it observable; with distinct timestamps both forms pass.
func TestVolumeMarkerPagingDoesNotDropTiedRows(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	r := newCinderTestRouter()

	shared := time.Now().UTC().Truncate(time.Microsecond)
	ids := make(map[string]bool, 5)
	for i := range 5 {
		id := uuid.NewString()
		ids[id] = true
		if _, err := database.DB.Exec(ctx, `
			INSERT INTO volumes (id, name, project_id, size_gb, status, created_at, updated_at)
			VALUES ($1, $2, $3, 1, 'available', $4, $4)
		`, id, "pagetie-"+id[:8], seededProjectID, shared); err != nil {
			t.Fatalf("seed volume %d: %v", i, err)
		}
		t.Cleanup(func() {
			database.DB.Exec(ctx, "DELETE FROM volumes WHERE id = $1", id) //nolint:errcheck // best-effort
		})
	}

	// Both listings page: the summary and the detail view, which had different ORDER BY clauses.
	for _, path := range []string{"/v3/%s/volumes", "/v3/%s/volumes/detail"} {
		t.Run(path, func(t *testing.T) {
			seen := map[string]int{}
			marker := ""
			for page := 0; page < 12; page++ {
				url := fmt.Sprintf(path, seededProjectID) + "?limit=1"
				if marker != "" {
					url += "&marker=" + marker
				}
				code, body := doReq(r, http.MethodGet, url, "", cinderAuthHdr(seededProjectID, seededUserID))
				if code != http.StatusOK {
					t.Fatalf("page %d: code=%d body=%s", page, code, body)
				}
				var resp struct {
					Volumes []struct {
						ID string `json:"id"`
					} `json:"volumes"`
				}
				if err := json.Unmarshal([]byte(body), &resp); err != nil {
					t.Fatalf("page %d: parse: %v", page, err)
				}
				if len(resp.Volumes) == 0 {
					break
				}
				for _, v := range resp.Volumes {
					if ids[v.ID] {
						seen[v.ID]++
					}
				}
				marker = resp.Volumes[len(resp.Volumes)-1].ID
			}

			for id := range ids {
				switch seen[id] {
				case 1: // exactly once, as a page walk must
				case 0:
					t.Errorf("volume %s was SKIPPED by the page walk; a marker on a non-unique sort "+
						"key excludes every row sharing its timestamp", id[:8])
				default:
					t.Errorf("volume %s was returned %d times; the page boundary is not stable", id[:8], seen[id])
				}
			}
		})
	}
}
