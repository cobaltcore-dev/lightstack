package keystone

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/database"

	"github.com/gin-gonic/gin"
)

// TestNotFoundOr500 verifies FR-010: a real not-found maps to the verbatim 404,
// while a genuine backend error surfaces as 5xx (never masked as 404).
func TestNotFoundOr500(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("ErrNoRows -> 404 verbatim", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		notFoundOr500(c, database.ErrNoRows, "user", "abc")
		if w.Code != http.StatusNotFound {
			t.Fatalf("code = %d, want 404", w.Code)
		}
		if want := "Could not find user: abc."; !strings.Contains(w.Body.String(), want) {
			t.Fatalf("body %q missing %q", w.Body.String(), want)
		}
	})

	t.Run("genuine error -> 500, not masked", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		notFoundOr500(c, errors.New("connection reset by peer"), "user", "abc")
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("code = %d, want 500", w.Code)
		}
	})
}

// TestExistsOr404 covers both branches of the EXISTS-style helper: a genuine
// error is a 500 (not masked), and a well-formed-but-absent row is a verbatim 404.
func TestExistsOr404(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("genuine error -> 500, not masked", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		if !existsOr404(c, errors.New("db down"), false, "user", "abc") {
			t.Fatal("want true (response written)")
		}
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("code = %d, want 500", w.Code)
		}
	})

	t.Run("not exists -> 404 verbatim", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		if !existsOr404(c, nil, false, "user", "abc") {
			t.Fatal("want true (response written)")
		}
		if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "Could not find user: abc.") {
			t.Fatalf("code=%d body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("exists -> false, no response", func(t *testing.T) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		if existsOr404(c, nil, true, "user", "abc") {
			t.Fatal("want false (caller continues)")
		}
		if w.Body.Len() != 0 {
			t.Fatalf("unexpected body written: %s", w.Body.String())
		}
	})
}
