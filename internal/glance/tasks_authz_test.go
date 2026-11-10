package glance

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestTaskListAndGetAreOwnerScoped(t *testing.T) {
	requireDB(t)
	r := newGlanceTestRouter(t)
	ownerID, foreignID := seededUserID, uuid.NewString()
	ownerTaskID, foreignTaskID := uuid.NewString(), uuid.NewString()
	now := time.Now().UTC()

	for _, task := range []struct{ id, owner, secret string }{
		{ownerTaskID, ownerID, "owner-private-input"},
		{foreignTaskID, foreignID, "foreign-private-input"},
	} {
		input, err := json.Marshal(map[string]string{"private": task.secret})
		if err != nil {
			t.Fatal(err)
		}
		result, err := json.Marshal(map[string]string{"private_result": task.secret})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := testPool.Exec(t.Context(), `
			INSERT INTO image_tasks (id, type, status, input, result, message, owner, created_at, updated_at)
			VALUES ($1, 'import', 'success', $2, $3, $4, $5, $6, $6)
		`, task.id, input, result, task.secret+" internal message", task.owner, now); err != nil {
			t.Fatalf("seed task %s: %v", task.id, err)
		}
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(context.Background(), "DELETE FROM image_tasks WHERE id IN ($1, $2)", ownerTaskID, foreignTaskID)
	})

	member := userHdr(ownerID, "member")
	code, body := doReq(r, http.MethodGet, "/tasks", member)
	if code != http.StatusOK {
		t.Fatalf("member list tasks: code=%d body=%s", code, body)
	}
	var listed struct {
		Tasks []map[string]any `json:"tasks"`
	}
	if err := json.Unmarshal([]byte(body), &listed); err != nil {
		t.Fatalf("decode task list: %v", err)
	}
	if len(listed.Tasks) != 1 || listed.Tasks[0]["id"] != ownerTaskID {
		t.Errorf("member task list = %s; want only task %s", body, ownerTaskID)
	}
	if strings.Contains(body, "foreign-private-input") {
		t.Errorf("member task list leaked foreign task data: %s", body)
	}

	code, body = doReq(r, http.MethodGet, "/tasks/"+ownerTaskID, member)
	if code != http.StatusOK || !strings.Contains(body, "owner-private-input") {
		t.Errorf("owner get task: code=%d body=%s; want 200 with own task data", code, body)
	}
	code, body = doReq(r, http.MethodGet, "/tasks/"+foreignTaskID, member)
	if code != http.StatusNotFound || strings.Contains(body, "foreign-private-input") || strings.Contains(body, "internal message") {
		t.Errorf("foreign get task: code=%d body=%s; want opaque 404", code, body)
	}

	admin := userHdr(uuid.NewString(), "admin")
	code, body = doReq(r, http.MethodGet, "/tasks", admin)
	if code != http.StatusOK || !strings.Contains(body, ownerTaskID) || !strings.Contains(body, foreignTaskID) {
		t.Errorf("admin list tasks: code=%d body=%s; want both tasks", code, body)
	}
	code, body = doReq(r, http.MethodGet, "/tasks/"+foreignTaskID, admin)
	if code != http.StatusOK || !strings.Contains(body, "foreign-private-input") {
		t.Errorf("admin get foreign task: code=%d body=%s; want 200", code, body)
	}
}
