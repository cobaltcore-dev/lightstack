package database

import (
	"strings"
	"testing"
)

// The helpers that shape every post-squash migration file (data-model §4): the header, the
// checksum of the body, and the applied_migrations record the migration writes about itself.

const (
	sha256Empty = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	sha256ABC   = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
)

func TestChecksumBody(t *testing.T) {
	for body, want := range map[string]string{"": sha256Empty, "abc": sha256ABC} {
		if got := checksumBody([]byte(body)); got != want {
			t.Errorf("checksumBody(%q) = %s; want %s", body, got, want)
		}
	}
}

func TestRenderUp(t *testing.T) {
	body := "ALTER TABLE x ADD COLUMN y TEXT;\n"
	sum := checksumBody([]byte(body))
	want := "-- release migration 002 (v0.7.0)\n" +
		"-- checksum: " + sum + "\n" +
		body +
		"INSERT INTO applied_migrations (version, checksum) VALUES (2, '" + sum + "');\n"
	if got := string(renderUp("release migration 002 (v0.7.0)", 2, []byte(body))); got != want {
		t.Errorf("renderUp:\n got: %q\nwant: %q", got, want)
	}
}

// A body without a final newline keeps its own bytes under the checksum; the newline that
// separates it from the record statement is added outside the checksummed bytes.
func TestRenderUpBodyWithoutFinalNewline(t *testing.T) {
	body := "SELECT 1;"
	sum := checksumBody([]byte(body))
	want := "-- h\n-- checksum: " + sum + "\nSELECT 1;\n" +
		"INSERT INTO applied_migrations (version, checksum) VALUES (3, '" + sum + "');\n"
	if got := string(renderUp("h", 3, []byte(body))); got != want {
		t.Errorf("renderUp:\n got: %q\nwant: %q", got, want)
	}
}

// A backend without a fragment of its own still gets the version: an empty body and its record.
func TestRenderUpEmptyBody(t *testing.T) {
	want := "-- h\n-- checksum: " + sha256Empty + "\n" +
		"INSERT INTO applied_migrations (version, checksum) VALUES (4, '" + sha256Empty + "');\n"
	if got := string(renderUp("h", 4, nil)); got != want {
		t.Errorf("renderUp:\n got: %q\nwant: %q", got, want)
	}
}

func TestRenderDown(t *testing.T) {
	body := "ALTER TABLE x DROP COLUMN y;\n"
	want := "-- release migration 002 (v0.7.0)\n" +
		"DELETE FROM applied_migrations WHERE version = 2;\n" +
		body
	if got := string(renderDown("release migration 002 (v0.7.0)", 2, []byte(body))); got != want {
		t.Errorf("renderDown:\n got: %q\nwant: %q", got, want)
	}
	// A body without a final newline still ends the file with one.
	if got := string(renderDown("h", 5, []byte("SELECT 1;"))); !strings.HasSuffix(got, "SELECT 1;\n") {
		t.Errorf("renderDown does not end with a newline: %q", got)
	}
}

func TestParseChecksumHeader(t *testing.T) {
	up := renderUp("release migration 002 (v0.7.0)", 2, []byte("SELECT 1;\n"))
	if got, err := parseChecksumHeader(up); err != nil || got != checksumBody([]byte("SELECT 1;\n")) {
		t.Fatalf("parseChecksumHeader(renderUp) = %q, %v", got, err)
	}
	// A checkout with CRLF line endings reads the same.
	crlf := []byte("-- h\r\n-- checksum: " + sha256ABC + "\r\nSELECT 1;\r\n")
	if got, err := parseChecksumHeader(crlf); err != nil || got != sha256ABC {
		t.Errorf("parseChecksumHeader(CRLF) = %q, %v; want %s", got, err, sha256ABC)
	}

	for name, file := range map[string]string{
		"one line only":         "-- h\n",
		"empty file":            "",
		"second line is SQL":    "-- h\nSELECT 1;\n",
		"wrong prefix":          "-- h\n-- sha256: " + sha256ABC + "\n",
		"63 hex digits":         "-- h\n-- checksum: " + sha256ABC[:63] + "\n",
		"65 hex digits":         "-- h\n-- checksum: " + sha256ABC + "0\n",
		"not hex":               "-- h\n-- checksum: " + strings.Repeat("g", 64) + "\n",
		"upper-case hex":        "-- h\n-- checksum: " + strings.ToUpper(sha256ABC) + "\n",
		"trailing text":         "-- h\n-- checksum: " + sha256ABC + " extra\n",
		"checksum on line one":  "-- checksum: " + sha256ABC + "\n-- h\n",
		"no newline after line": "-- h",
	} {
		if got, err := parseChecksumHeader([]byte(file)); err == nil {
			t.Errorf("%s: parseChecksumHeader = %q, nil; want an error", name, got)
		}
	}
}

func TestAppliedMigrationsDDL(t *testing.T) {
	for _, b := range []Backend{Postgres, SQLite} {
		ddl, ok := appliedMigrationsDDL[b]
		if !ok || !strings.Contains(ddl, "applied_migrations") {
			t.Fatalf("appliedMigrationsDDL[%s] = %q", b, ddl)
		}
		for _, col := range []string{"version", "checksum", "applied_at"} {
			if !strings.Contains(ddl, col) {
				t.Errorf("appliedMigrationsDDL[%s] lacks column %s", b, col)
			}
		}
	}
	if !strings.Contains(appliedMigrationsDDL[SQLite], "STRICT") {
		t.Errorf("the SQLite table must be STRICT like every table of the SQLite baseline")
	}
}
