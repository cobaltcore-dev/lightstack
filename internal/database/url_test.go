package database_test

import (
	"strings"
	"testing"

	"github.com/cobaltcore-dev/lightstack/internal/database"
)

// TestParseURL pins contracts/configuration-and-startup.md §1: which URLs select which
// backend, and the exact message for each refused one.
func TestParseURL(t *testing.T) {
	const accepted = "accepted schemes: postgres, postgresql, sqlite"
	invalidSQLite := func(shown string) string {
		return `database.url "` + shown + `" is not a valid SQLite URL; expected sqlite:///absolute/path/lightstack.db (absolute path, no host, no query parameters)`
	}
	for _, tc := range []struct {
		in      string
		backend database.Backend
		msg     string
	}{
		{"postgres://lightstack:secret@db:5432/lightstack?sslmode=disable", database.Postgres, ""},
		{"postgresql://db/lightstack", database.Postgres, ""},
		{"postgres:///lightstack?host=/var/run/postgresql", database.Postgres, ""},
		{"POSTGRES://db/lightstack", database.Postgres, ""},
		{"sqlite:///var/lib/lightstack/db/lightstack.db", database.SQLite, ""},
		{"", "", "database.url is empty; " + accepted},
		{"   ", "", "database.url is empty; " + accepted},
		{"host=db user=lightstack password=secret", "", "database.url has no scheme; " + accepted},
		{"mysql://db/lightstack", "", `database.url scheme "mysql" is not supported; ` + accepted},
		{"sqlite3:///x.db", "", `database.url scheme "sqlite3" is not supported; ` + accepted},
		{"file:/var/lib/x.db", "", `database.url scheme "file" is not supported; ` + accepted},
		{"sqlite://relative/x.db", "", invalidSQLite("sqlite://relative/x.db")},
		{"sqlite://host/x.db", "", invalidSQLite("sqlite://host/x.db")},
		{"sqlite:///x.db?mode=ro", "", invalidSQLite("sqlite:///x.db?mode=ro")},
		{"sqlite:///x.db#frag", "", invalidSQLite("sqlite:///x.db#frag")},
		{"sqlite:x.db", "", invalidSQLite("sqlite:x.db")},
		{"sqlite:///", "", invalidSQLite("sqlite:///")},
		{"sqlite:///var/lib/lightstack/", "", invalidSQLite("sqlite:///var/lib/lightstack/")},
		{"sqlite://", "", invalidSQLite("sqlite://")},
		{"sqlite://u:secret@host/x.db", "", invalidSQLite("sqlite://u:xxxxx@host/x.db")},
		{"sqlite:///x.db?password=secret", "", invalidSQLite("sqlite:///x.db?password=xxxxx")},
		{"postgres://admin:secret%@db/x", "", "database.url is not a valid URL; percent-encode reserved characters in the user name and password"},
	} {
		got, err := database.ParseURL(tc.in)
		switch {
		case tc.msg == "" && err != nil:
			t.Errorf("ParseURL(%q): %v; want %s", tc.in, err, tc.backend)
		case tc.msg == "" && got != tc.backend:
			t.Errorf("ParseURL(%q) = %s; want %s", tc.in, got, tc.backend)
		case tc.msg != "" && (err == nil || err.Error() != tc.msg):
			t.Errorf("ParseURL(%q) error = %v;\n want %s", tc.in, err, tc.msg)
		}
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Errorf("ParseURL(%q) printed the password: %v", tc.in, err)
		}
	}
}

// TestSQLiteFilePath pins the helper teardown uses to find the one file (and so the directory)
// a SQLite database.url names: the cleaned URL path for a SQLite URL, and (_, false) for a
// PostgreSQL or invalid URL so the caller removes nothing.
func TestSQLiteFilePath(t *testing.T) {
	for _, tc := range []struct {
		in       string
		wantPath string
		wantOK   bool
	}{
		{"sqlite:///var/lib/lightstack/db/lightstack.db", "/var/lib/lightstack/db/lightstack.db", true},
		{"sqlite:///var/lib/lightstack/custom/lightstack.db", "/var/lib/lightstack/custom/lightstack.db", true},
		{"sqlite:///var/lib/lightstack//db/x.db", "/var/lib/lightstack/db/x.db", true}, // cleaned
		{"postgres://u:p@db/lightstack", "", false},
		{"sqlite://relative/x.db", "", false}, // not a valid SQLite URL
		{"", "", false},
	} {
		got, ok := database.SQLiteFilePath(tc.in)
		if ok != tc.wantOK || got != tc.wantPath {
			t.Errorf("SQLiteFilePath(%q) = (%q, %v); want (%q, %v)", tc.in, got, ok, tc.wantPath, tc.wantOK)
		}
	}
}

// TestCheckDeclaredBackend pins §2: LIGHTSTACK_DB_BACKEND, when set, must name the
// backend the URL selects.
func TestCheckDeclaredBackend(t *testing.T) {
	for _, tc := range []struct {
		declared string
		backend  database.Backend
		msg      string
	}{
		{"", database.Postgres, ""},
		{"", database.SQLite, ""},
		{"postgres", database.Postgres, ""},
		{"sqlite", database.SQLite, ""},
		{"postgres", database.SQLite, "LIGHTSTACK_DB_BACKEND=postgres but database.url selects sqlite"},
		{"sqlite", database.Postgres, "LIGHTSTACK_DB_BACKEND=sqlite but database.url selects postgres"},
		{"postgresql", database.Postgres, `LIGHTSTACK_DB_BACKEND="postgresql" is not valid; accepted values: postgres, sqlite`},
		{"SQLite", database.SQLite, `LIGHTSTACK_DB_BACKEND="SQLite" is not valid; accepted values: postgres, sqlite`},
		{"mysql", database.Postgres, `LIGHTSTACK_DB_BACKEND="mysql" is not valid; accepted values: postgres, sqlite`},
	} {
		err := database.CheckDeclaredBackend(tc.declared, tc.backend)
		if tc.msg == "" && err != nil {
			t.Errorf("CheckDeclaredBackend(%q, %s): %v; want nil", tc.declared, tc.backend, err)
		}
		if tc.msg != "" && (err == nil || err.Error() != tc.msg) {
			t.Errorf("CheckDeclaredBackend(%q, %s) = %v;\n want %s", tc.declared, tc.backend, err, tc.msg)
		}
	}
}

// TestRedactURL: messages and logs never print a password.
func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"postgres://admin:s3cret@db:5432/x?sslmode=disable": "postgres://admin:xxxxx@db:5432/x?sslmode=disable",
		"postgres://db:5432/x":                              "postgres://db:5432/x",
		"sqlite:///var/lib/lightstack/db/lightstack.db":     "sqlite:///var/lib/lightstack/db/lightstack.db",
		"sqlite://u:s3cret@host/x.db":                       "sqlite://u:xxxxx@host/x.db",
		"postgres://db/x?password=s3cret":                   "postgres://db/x?password=xxxxx",
	} {
		if got := database.RedactURL(in); got != want {
			t.Errorf("RedactURL(%q) = %q; want %q", in, got, want)
		}
	}
	// Keyword/value strings are not URLs: never echoed, whatever they contain.
	if got := database.RedactURL("host=db password=s3cret"); strings.Contains(got, "s3cret") {
		t.Errorf("RedactURL kept the password of a keyword/value string: %q", got)
	}
}
