package nova

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cobaltcore-dev/lightstack/internal/database"
	"github.com/cobaltcore-dev/lightstack/internal/database/dbtest"
	"github.com/gin-gonic/gin"
)

func seedAttachVolume(t *testing.T) string {
	t.Helper()
	var id string
	if err := testPool.QueryRow(t.Context(), `INSERT INTO volumes (name, project_id, user_id, size_gb, status)
		VALUES ('attach-review', $1, $2, 1, 'available') RETURNING id`, seededProjectID, seededUserID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), "DELETE FROM volume_attachments WHERE volume_id = $1", id)
		_, _ = testPool.Exec(context.Background(), "DELETE FROM volumes WHERE id = $1", id)
	})
	return id
}

func attachWithPool(ctx context.Context, pool database.DB, instanceID, volumeID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
		fmt.Sprintf(`{"volumeAttachment":{"volumeId":%q}}`, volumeID))).WithContext(ctx)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = gin.Params{{Key: "id", Value: instanceID}}
	c.Set("project_id", seededProjectID)
	c.Set("user_id", seededUserID)
	c.Set("roles", []string{"member"})
	svc := &Service{pool: pool}
	svc.AttachVolume(c)
	return w
}

func TestAttachVolumeWithOneConnection(t *testing.T) {
	requireDB(t)
	instanceID, volumeID := seedConsoleInstance(t, seededProjectID), seedAttachVolume(t)
	pool, err := database.Open(t.Context(), dbtest.URL(), database.Options{
		Pool: database.PoolConfig{MaxConns: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	w := attachWithPool(ctx, pool, instanceID, volumeID)
	if w.Code != http.StatusOK {
		t.Fatalf("attach with one connection: %d %s", w.Code, w.Body.String())
	}
}

func TestConcurrentVolumeAttachmentsUseDistinctDevices(t *testing.T) {
	requireDB(t)
	instanceID := seedConsoleInstance(t, seededProjectID)
	const count = 8
	volumes := make([]string, count)
	for i := range volumes {
		volumes[i] = seedAttachVolume(t)
	}
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, count)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for _, id := range volumes {
		go func() {
			<-start
			results <- attachWithPool(ctx, testPool, instanceID, id)
		}()
	}
	close(start)
	for range count {
		w := <-results
		if w.Code != http.StatusOK {
			t.Errorf("concurrent attach: %d %s", w.Code, w.Body.String())
		}
	}
	var total, distinct int
	if err := testPool.QueryRow(t.Context(), `SELECT count(*), count(DISTINCT device)
		FROM volume_attachments WHERE instance_id = $1`, instanceID).Scan(&total, &distinct); err != nil {
		t.Fatal(err)
	}
	if total != count || distinct != count {
		t.Errorf("attachments=%d distinct devices=%d; want %d each", total, distinct, count)
	}
}
