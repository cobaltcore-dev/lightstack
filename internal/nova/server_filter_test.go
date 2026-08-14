package nova

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// SPEC-023 US1: the server listing's scope and paging, asserted on the rendered
// SQL rather than through a handler.
//
// These need no database, which is the point. The listing paths are otherwise
// covered only end-to-end by DB-backed handler tests, so without a test DB
// configured nothing pins the placeholder arithmetic at all — and that
// arithmetic is where this type earns its keep. beforeKeyset and limitOffset
// each compute `len(args)-1` by hand, which is exactly what serverFilter's own
// doc comment says a caller must never do.
//
// The keystone counterpart is TestEqDomainResolvesTheDefaultAlias.

// filterCtx builds a request context carrying a raw query string, a project and
// a role. The raw form matters: `?all_tenants` with no value is the spelling
// Horizon sends, and it does not survive a map fixture.
func serverFilterCtx(rawQuery, projectID string, admin bool) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/v2.1/servers?"+rawQuery, nil)
	c.Set("project_id", projectID)
	if admin {
		c.Set("roles", []string{"admin"})
	} else {
		c.Set("roles", []string{"member"})
	}
	return c
}

// TestNewServerFilterScope: FR-005, FR-005b. Widening is opt-in AND
// administrator-only, so three of these four cases must stay scoped. The
// admin-without-all_tenants case is the compatibility guarantee — an operator's
// ordinary listing must not silently start spanning the deployment.
func TestNewServerFilterScope(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const project = "00000000-0000-0000-0000-0000000000a1"

	for _, tc := range []struct {
		name      string
		query     string
		admin     bool
		wantWhere string
	}{
		{"member, no opt-in", "", false, " WHERE project_id = $1"},
		{"member opting in is ignored", "all_tenants=1", false, " WHERE project_id = $1"},
		{"admin without opt-in stays scoped", "", true, " WHERE project_id = $1"},
		{"admin opting in widens", "all_tenants=1", true, ""},
		{"admin, valueless form widens", "all_tenants", true, ""},
		{"admin, false value stays scoped", "all_tenants=no", true, " WHERE project_id = $1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newServerFilter(serverFilterCtx(tc.query, project, tc.admin), "")
			if got := f.Where(); got != tc.wantWhere {
				t.Errorf("where() = %q, want %q", got, tc.wantWhere)
			}
			if tc.wantWhere == "" {
				if len(f.Args()) != 0 {
					t.Errorf("args = %v, want none — a widened listing binds no project", f.Args())
				}
				return
			}
			if len(f.Args()) != 1 || f.Args()[0] != project {
				t.Errorf("args = %v, want [%q]", f.Args(), project)
			}
		})
	}
}

// TestNewServerFilterAlias: the alias is applied where the predicate is written,
// so no caller has to rewrite the rendered SQL to insert it. The detail listing
// joins and needs "i.", the summary listing does not.
func TestNewServerFilterAlias(t *testing.T) {
	gin.SetMode(gin.TestMode)
	f := newServerFilter(serverFilterCtx("", "p1", false), "i.")
	if want := " WHERE i.project_id = $1"; f.Where() != want {
		t.Errorf("where() = %q, want %q", f.Where(), want)
	}
}

// TestBeforeKeysetIsComposite: FR-005, contract L5. created_at alone is not
// unique, so a plain `<` skips or repeats rows sharing a timestamp. The
// comparison must name both columns and bind both values, in order.
func TestBeforeKeysetIsComposite(t *testing.T) {
	gin.SetMode(gin.TestMode)
	at := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	const id = "00000000-0000-0000-0000-0000000000b1"

	f := newServerFilter(serverFilterCtx("", "p1", false), "i.")
	f.beforeKeyset("i.", at, id)

	want := " WHERE i.project_id = $1 AND (i.created_at, i.id) < ($2, $3)"
	if got := f.Where(); got != want {
		t.Errorf("where() = %q, want %q", got, want)
	}
	if len(f.Args()) != 3 || f.Args()[1] != at || f.Args()[2] != id {
		t.Errorf("args = %v, want [p1 %v %q] — the keyset values must follow the scope", f.Args(), at, id)
	}
}

// TestLimitOffsetContinuesTheNumbering: the clause and the arguments it reads
// are produced together so they cannot drift. This is the case a hand-counted
// "$4/$5" would get wrong the moment a predicate is added above it.
func TestLimitOffsetContinuesTheNumbering(t *testing.T) {
	gin.SetMode(gin.TestMode)
	at := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

	f := newServerFilter(serverFilterCtx("", "p1", false), "i.")
	f.beforeKeyset("i.", at, "b1")
	clause := f.limitOffset(25, 50)

	if want := " LIMIT $4 OFFSET $5"; clause != want {
		t.Errorf("limitOffset() = %q, want %q", clause, want)
	}
	if len(f.Args()) != 5 || f.Args()[3] != 25 || f.Args()[4] != 50 {
		t.Errorf("args = %v, want [p1 %v b1 25 50]", f.Args(), at)
	}
}

// TestLimitOffsetOnAWidenedListing: the numbering has to follow the arguments
// actually bound, not a fixed position. With no project predicate the paging
// values are $1 and $2 — a filter that assumed a scope was always present would
// bind them one slot too high and silently page the wrong window.
func TestLimitOffsetOnAWidenedListing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	f := newServerFilter(serverFilterCtx("all_tenants", "p1", true), "")

	if f.Where() != "" {
		t.Fatalf("where() = %q, want empty — this case exists to test the unscoped path", f.Where())
	}
	if want := " LIMIT $1 OFFSET $2"; f.limitOffset(10, 0) != want {
		t.Errorf("limitOffset() = %q, want %q", f.limitOffset(10, 0), want)
	}
}

// TestServerFilterEmpty: no predicates renders no WHERE at all, rather than a
// dangling keyword or a "WHERE true" that every later predicate has to append to.
func TestServerFilterEmpty(t *testing.T) {
	var f serverFilter
	if f.Where() != "" {
		t.Errorf("where() = %q, want empty", f.Where())
	}
	if len(f.Args()) != 0 {
		t.Errorf("args = %v, want none", f.Args())
	}
}
