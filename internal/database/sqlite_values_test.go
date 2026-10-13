package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestBindValue pins research R7's bind rules: the SQLite implementation converts each
// argument so callers bind the same Go types on both backends (FR-019 – FR-023).
func TestBindValue(t *testing.T) {
	at := time.Date(2026, 10, 9, 8, 15, 30, 123456789, time.FixedZone("CEST", 2*3600))
	id := uuid.MustParse("3f1b4b52-4d3a-4f0e-9a51-0d6a0f6b1a01")
	type spec struct {
		Size int    `json:"size"`
		Name string `json:"name"`
	}
	cases := []struct {
		name string
		in   any
		want any
	}{
		{"time.Time → UTC text with six fractional digits", at, "2026-10-09T06:15:30.123456Z"},
		{"*time.Time", &at, "2026-10-09T06:15:30.123456Z"},
		{"nil *time.Time", (*time.Time)(nil), nil},
		{"[]byte → text", []byte(`{"a":1}`), `{"a":1}`},
		{"json.RawMessage → text", json.RawMessage(`[1,2]`), `[1,2]`},
		{"nil []byte → NULL, as pgx binds it", []byte(nil), nil},
		{"nil json.RawMessage → NULL", json.RawMessage(nil), nil},
		{"empty, non-nil []byte → empty text", []byte{}, ""},
		{"map → JSON text", map[string]string{"k": "v"}, `{"k":"v"}`},
		{"struct → JSON text", spec{Size: 1, Name: "n"}, `{"size":1,"name":"n"}`},
		{"[]string outside a list position → JSON array", []string{"a", "b"}, `["a","b"]`},
		{"empty []string → empty JSON array", []string{}, `[]`},
		{"bool stays bool (the driver binds 0/1)", true, true},
		{"uuid.UUID → canonical text", id, "3f1b4b52-4d3a-4f0e-9a51-0d6a0f6b1a01"},
		{"*string", ptr("x"), "x"},
		{"nil *string", (*string)(nil), nil},
		{"int stays", 42, 42},
		{"string stays", "s", "s"},
		{"nil stays", nil, nil},
		{"sql.NullString", sql.NullString{String: "s", Valid: true}, "s"},
		{"invalid sql.NullString → NULL", sql.NullString{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := bindValue(tc.in)
			if err != nil {
				t.Fatalf("bindValue: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("bindValue(%#v) = %#v; want %#v", tc.in, got, tc.want)
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

// errValuer is a driver.Valuer whose Value() fails, to cover bindValue's Valuer-error path.
type errValuer struct{}

func (errValuer) Value() (driver.Value, error) { return nil, errTestValuer }

var errTestValuer = errors.New("valuer failed")

// okValuer returns a string from Value(), to cover the Valuer success path and the
// nil-pointer-Valuer short-circuit.
type okValuer struct{ s string }

func (v okValuer) Value() (driver.Value, error) { return v.s, nil }

func TestBindValueErrorsAndValuer(t *testing.T) {
	if _, err := bindValue(errValuer{}); !errors.Is(err, errTestValuer) {
		t.Errorf("bindValue(errValuer) = %v; want the valuer's error", err)
	}
	if got, err := bindValue(okValuer{"ok"}); err != nil || got != "ok" {
		t.Errorf("bindValue(okValuer) = %v, %v; want \"ok\"", got, err)
	}
	if got, err := bindValue((*okValuer)(nil)); err != nil || got != nil {
		t.Errorf("bindValue(nil *okValuer) = %v, %v; want nil", got, err)
	}
	if got, err := bindValue(ptr(7)); err != nil || got != 7 {
		t.Errorf("bindValue(*int) = %v, %v; want 7", got, err)
	}
	type myStr string
	if got, err := bindValue(myStr("x")); err != nil || got != myStr("x") {
		t.Errorf("bindValue(named string) = %v, %v; want it passed through", got, err)
	}
}

// TestScanConversion pins research R7's scan rules: what SQLite returns as text is
// decoded into the destination types callers use with PostgreSQL.
func TestScanConversion(t *testing.T) {
	ts := "2026-10-09T06:15:30.123456Z"
	want := time.Date(2026, 10, 9, 6, 15, 30, 123456000, time.UTC)

	var tm time.Time
	if err := scanInto(ts, &tm); err != nil || !tm.Equal(want) {
		t.Fatalf("time.Time = %v, %v; want %v", tm, err, want)
	}
	var ptm *time.Time
	if err := scanInto(ts, &ptm); err != nil || ptm == nil || !ptm.Equal(want) {
		t.Fatalf("*time.Time = %v, %v; want %v", ptm, err, want)
	}
	ptm = &tm
	if err := scanInto(nil, &ptm); err != nil || ptm != nil {
		t.Fatalf("*time.Time from NULL = %v, %v; want nil", ptm, err)
	}
	var nt sql.NullTime
	if err := scanInto(ts, &nt); err != nil || !nt.Valid || !nt.Time.Equal(want) {
		t.Fatalf("sql.NullTime = %+v, %v", nt, err)
	}
	if err := scanInto("2026-10-09 06:15:30", &tm); err == nil || !strings.Contains(err.Error(), "2026-10-09 06:15:30") {
		t.Fatalf("a timestamp in another format = %v; want an error naming the value", err)
	}

	var m map[string]any
	if err := scanInto(`{"enable_snat":false,"n":1}`, &m); err != nil || m["enable_snat"] != false || m["n"] != float64(1) {
		t.Fatalf("map = %v, %v", m, err)
	}
	var list []string
	if err := scanInto(`["10.0.0.0/8","192.0.2.1"]`, &list); err != nil || !reflect.DeepEqual(list, []string{"10.0.0.0/8", "192.0.2.1"}) {
		t.Fatalf("[]string = %v, %v", list, err)
	}
	list = []string{"stale"}
	if err := scanInto(nil, &list); err != nil || list != nil {
		t.Fatalf("[]string from NULL = %v, %v; want nil", list, err)
	}
	var raw []byte
	if err := scanInto(`{"a":1}`, &raw); err != nil || string(raw) != `{"a":1}` {
		t.Fatalf("[]byte = %q, %v", raw, err)
	}
	// A named byte slice (json.RawMessage) scans as doc.go promises, which database/sql alone
	// refuses for anything but a plain []byte.
	var rawMsg json.RawMessage
	if err := scanInto(`{"a":1}`, &rawMsg); err != nil || string(rawMsg) != `{"a":1}` {
		t.Fatalf("json.RawMessage = %q, %v", rawMsg, err)
	}
	rawMsg = json.RawMessage(`{"stale":true}`)
	if err := scanInto(nil, &rawMsg); err != nil || rawMsg != nil {
		t.Fatalf("json.RawMessage from NULL = %q, %v; want nil", rawMsg, err)
	}
	var b bool
	if err := scanInto(int64(1), &b); err != nil || !b {
		t.Fatalf("bool from 1 = %v, %v", b, err)
	}
	var s string
	if err := scanInto(int64(7), &s); err != nil || s != "7" {
		t.Fatalf("string from INTEGER (->> on a JSON number) = %q, %v", s, err)
	}

	// Malformed JSON into a struct target is an error naming the destination type, not a
	// silent zero.
	var dst struct {
		N int `json:"n"`
	}
	if err := scanInto(`{bad json`, &dst); err == nil || !strings.Contains(err.Error(), "decode JSON") {
		t.Fatalf("malformed JSON into a struct = %v; want a decode error", err)
	}
	// NULL into *time.Time nested through a non-nil pointer is rejected, not scanned as zero.
	var nilT *time.Time
	if err := scanInto("not a timestamp", &nilT); err == nil {
		t.Fatalf("a non-timestamp into *time.Time = %v; want an error", err)
	}
}

// TestRegisteredFunctionArgErrors covers the argument-type guards of the SQLite functions
// (research R6): ls_add_seconds and regexp return errors or NULL for the operands they
// cannot use, rather than a wrong value.
func TestRegisteredFunctionArgErrors(t *testing.T) {
	db := openTestSQLite(t, filepath.Join(t.TempDir(), "fn.db"), Options{})
	ctx := context.Background()

	var out any
	for _, tc := range []struct {
		name, sql string
		wantErr   string
	}{
		{"add_seconds on a non-text timestamp", `SELECT ls_add_seconds(1, 2)`, "not text"},
		{"add_seconds with a malformed timestamp", `SELECT ls_add_seconds('nope', 2)`, ""},
		{"add_seconds with non-numeric seconds", `SELECT ls_add_seconds('2026-10-09T06:15:30.000000Z', 'x')`, "not a number"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := db.QueryRow(ctx, tc.sql).Scan(&out)
			if err == nil {
				t.Fatalf("%s: succeeded; want an error", tc.sql)
			}
			if tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("%s: error %q does not name %q", tc.sql, err, tc.wantErr)
			}
		})
	}

	// A NULL operand to add_seconds and to regexp yields NULL, not an error.
	var n sql.NullString
	if err := db.QueryRow(ctx, `SELECT ls_add_seconds(NULL, 1)`).Scan(&n); err != nil || n.Valid {
		t.Errorf("ls_add_seconds(NULL, 1) = %+v, %v; want NULL", n, err)
	}
	var m sql.NullInt64
	if err := db.QueryRow(ctx, `SELECT NULL REGEXP $1`, "^x$").Scan(&m); err != nil || m.Valid {
		t.Errorf("NULL REGEXP = %+v, %v; want NULL", m, err)
	}
	// An invalid pattern is an error naming it.
	if err := db.QueryRow(ctx, `SELECT 'x' REGEXP $1`, "(").Scan(&out); err == nil || !strings.Contains(err.Error(), "invalid regular expression") {
		t.Errorf("an invalid REGEXP pattern = %v; want an error", err)
	}
}
