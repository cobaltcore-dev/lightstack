package database

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// Every migration from the squash on — the baseline, each release migration and the open release
// migration — has one shape (SPEC-032 data-model §4):
//
//	-- <header>
//	-- checksum: <SHA-256 of BODY, lower-case hex>
//	<BODY>
//	INSERT INTO applied_migrations (version, checksum) VALUES (<N>, '<checksum>');
//
// The record row is part of the migration's own SQL, so it commits in the migration's transaction:
// golang-migrate runs a PostgreSQL file as one multi-statement simple query (one implicit
// transaction) and wraps each SQLite file in a transaction. No crash and no second node can see a
// migration without its record. The stale-database guard compares the record a database holds for
// its version with the checksum header of the file the view serves for that version (FR-019,
// FR-020); the header line before it differs between the open and the frozen form of a migration,
// which is why only BODY is checksummed.

const checksumPrefix = "-- checksum: "

// checksumBody is the lower-case hex SHA-256 of a migration's body.
func checksumBody(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// renderUp builds the up file of a post-squash migration: the header line, the checksum of body,
// body itself, and the statement that records the migration in applied_migrations. header is the
// comment text without its leading "-- ".
func renderUp(header string, version uint64, body []byte) []byte {
	sum := checksumBody(body)
	var b bytes.Buffer
	fmt.Fprintf(&b, "-- %s\n%s%s\n", header, checksumPrefix, sum)
	writeBody(&b, body)
	fmt.Fprintf(&b, "INSERT INTO applied_migrations (version, checksum) VALUES (%d, '%s');\n", version, sum)
	return b.Bytes()
}

// renderDown builds the down file of a post-squash migration: the header line, the statement that
// removes the migration's record, then body.
func renderDown(header string, version uint64, body []byte) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "-- %s\nDELETE FROM applied_migrations WHERE version = %d;\n", header, version)
	writeBody(&b, body)
	return b.Bytes()
}

// writeBody writes body and, when it does not end with one, a newline, so whatever follows starts
// on its own line. renderUp and renderDown add it after the checksummed BODY; between fragments it
// is part of the BODY (fragmentBody).
func writeBody(b *bytes.Buffer, body []byte) {
	b.Write(body)
	if len(body) > 0 && body[len(body)-1] != '\n' {
		b.WriteByte('\n')
	}
}

// parseChecksumHeader returns the checksum an up file records: the 64 lower-case hex digits after
// "-- checksum: " on its second line. A trailing carriage return (a CRLF checkout) is ignored.
func parseChecksumHeader(up []byte) (string, error) {
	lines := strings.SplitN(string(up), "\n", 3)
	if len(lines) < 2 {
		return "", fmt.Errorf("migration has no checksum header: it ends before its second line")
	}
	line := strings.TrimSuffix(lines[1], "\r")
	if !strings.HasPrefix(line, checksumPrefix) {
		return "", fmt.Errorf("migration has no checksum header: its second line must start with %q", checksumPrefix)
	}
	sum := strings.TrimPrefix(line, checksumPrefix)
	if len(sum) != sha256.Size*2 || !isLowerHex(sum) {
		return "", fmt.Errorf("migration checksum header %q is not 64 lower-case hex digits", line)
	}
	return sum, nil
}

func isLowerHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
