package keystone

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

type failCredentialDB struct{ database.DB }
type failCredentialTx struct{ database.Tx }

func (d failCredentialDB) Begin(ctx context.Context) (database.Tx, error) {
	tx, err := d.DB.Begin(ctx)
	return failCredentialTx{tx}, err
}
func (tx failCredentialTx) Exec(ctx context.Context, query string, args ...any) (database.Result, error) {
	if strings.Contains(query, "INSERT INTO application_credential_roles") {
		return nil, errors.New("injected role failure")
	}
	return tx.Tx.Exec(ctx, query, args...)
}

func TestCredentialRolesAreAtomic(t *testing.T) {
	requireDB(t)
	ctx := context.Background()
	userID, roleID, name := uuid.NewString(), uuid.NewString(), uuid.NewString()
	if _, err := testPool.Exec(ctx, "INSERT INTO users (id,name,password_hash,domain_id) VALUES ($1,$2,'x',$3)", userID, userID, defaultDomainID); err != nil {
		t.Fatal(err)
	}
	if _, err := testPool.Exec(ctx, "INSERT INTO roles (id,name) VALUES ($1,$2)", roleID, roleID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		testPool.Exec(ctx, "DELETE FROM application_credentials WHERE user_id=$1", userID)
		testPool.Exec(ctx, "DELETE FROM roles WHERE id=$1", roleID)
		testPool.Exec(ctx, "DELETE FROM users WHERE id=$1", userID)
	})
	svc := NewService(failCredentialDB{testPool}, nil, nil)
	r := gin.New()
	r.POST("/users/:id/application_credentials", func(c *gin.Context) { c.Set("user_id", userID) }, svc.CreateApplicationCredential)
	payload := func(roles []map[string]string) string {
		b, err := json.Marshal(map[string]any{"application_credential": map[string]any{"name": name, "roles": roles}})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	path := "/users/" + userID + "/application_credentials"
	body := payload([]map[string]string{{"id": roleID}})
	if code, resp := doReqBody(r, http.MethodPost, path, body, nil); code != 500 {
		t.Fatalf("injected role failure: %d %s", code, resp)
	}
	var count int
	if err := testPool.QueryRow(ctx, "SELECT COUNT(*) FROM application_credentials WHERE user_id=$1", userID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("credential survived failed role write: %d %v", count, err)
	}
	svc.pool = testPool
	if code, resp := doReqBody(r, http.MethodPost, path, payload([]map[string]string{{}}), nil); code != 400 {
		t.Fatalf("malformed role: %d %s", code, resp)
	}
	body = payload([]map[string]string{{"id": roleID}, {"name": roleID}})
	if code, resp := doReqBody(r, http.MethodPost, path, body, nil); code != 201 {
		t.Fatalf("role aliases: %d %s", code, resp)
	}
	if err := testPool.QueryRow(ctx, "SELECT COUNT(*) FROM application_credential_roles WHERE role_id=$1", roleID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("stored roles: %d %v", count, err)
	}
}
