package database

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"reflect"
	"time"
)

// Values on SQLite (research R7, FR-019 – FR-023). The SQLite implementation converts at
// the boundary, so callers bind and scan the same Go types on both backends:
//
//   - times are text, UTC, RFC 3339 with exactly six fractional digits — fixed width
//     and one zone, so string order is time order;
//   - JSON documents and lists are JSON text;
//   - []byte is bound as text, because a STRICT TEXT column refuses a BLOB;
//   - booleans are 0/1, ids canonical text, NULL handling unchanged.

// timeLayout is the one timestamp format on SQLite. Format it from UTC only.
const timeLayout = "2006-01-02T15:04:05.000000Z"

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("timestamp %q is not in the SQLite storage format %s", s, timeLayout)
	}
	return t, nil
}

// bindValue converts one argument for SQLite.
func bindValue(v any) (any, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case time.Time:
		return formatTime(x), nil
	case *time.Time:
		if x == nil {
			return nil, nil
		}
		return formatTime(*x), nil
	case []byte:
		if x == nil {
			return nil, nil // NULL, as pgx binds a nil slice
		}
		return string(x), nil
	case json.RawMessage:
		if x == nil {
			return nil, nil
		}
		return string(x), nil
	case string, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return x, nil
	}
	rv := reflect.ValueOf(v)
	if valuer, ok := v.(driver.Valuer); ok {
		if rv.Kind() == reflect.Pointer && rv.IsNil() {
			return nil, nil
		}
		dv, err := valuer.Value()
		if err != nil {
			return nil, err
		}
		return bindValue(dv)
	}
	switch rv.Kind() {
	case reflect.Pointer:
		if rv.IsNil() {
			return nil, nil
		}
		return bindValue(rv.Elem().Interface())
	case reflect.Slice:
		if rv.IsNil() {
			return nil, nil
		}
		if rv.Type().Elem().Kind() == reflect.Uint8 {
			return string(rv.Bytes()), nil
		}
		return marshalJSON(v)
	case reflect.Map:
		if rv.IsNil() {
			return nil, nil
		}
		return marshalJSON(v)
	case reflect.Struct, reflect.Array:
		return marshalJSON(v)
	}
	return v, nil // named basic kinds: database/sql converts them
}

func marshalJSON(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("encode %T as JSON for SQLite: %w", v, err)
	}
	return string(b), nil
}

// scanTarget returns what to hand to database/sql in place of dest, and a function that
// converts the scanned text into dest afterwards (nil when dest is scanned directly).
func scanTarget(dest any) (any, func() error) {
	switch p := dest.(type) {
	case *time.Time:
		return timeTarget(func(t time.Time, valid bool) error {
			if !valid {
				return fmt.Errorf("cannot scan NULL into *time.Time")
			}
			*p = t
			return nil
		})
	case **time.Time:
		return timeTarget(func(t time.Time, valid bool) error {
			if valid {
				*p = &t
			} else {
				*p = nil
			}
			return nil
		})
	case *sql.NullTime:
		return timeTarget(func(t time.Time, valid bool) error {
			*p = sql.NullTime{Time: t, Valid: valid}
			return nil
		})
	}
	if _, ok := dest.(sql.Scanner); ok {
		return dest, nil
	}
	rv := reflect.ValueOf(dest)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return dest, nil
	}
	el := rv.Elem()
	switch el.Kind() {
	case reflect.Map, reflect.Struct:
		return jsonTarget(dest, el)
	case reflect.Slice:
		if el.Type().Elem().Kind() != reflect.Uint8 {
			return jsonTarget(dest, el)
		}
		if el.Type() == reflect.TypeOf([]byte(nil)) { // plain []byte: database/sql scans it
			return dest, nil
		}
		// A named byte slice (json.RawMessage): database/sql converts a TEXT source only into
		// *[]byte/*RawBytes, so take it as a string and convert into the named type.
		var s sql.NullString
		return &s, func() error {
			if !s.Valid {
				el.Set(reflect.Zero(el.Type()))
				return nil
			}
			el.Set(reflect.ValueOf([]byte(s.String)).Convert(el.Type()))
			return nil
		}
	}
	return dest, nil
}

// timeTarget scans a time column as text and hands the parsed value to set; valid is false for a
// NULL, so each target decides what a NULL means.
func timeTarget(set func(t time.Time, valid bool) error) (any, func() error) {
	var s sql.NullString
	return &s, func() error {
		if !s.Valid {
			return set(time.Time{}, false)
		}
		t, err := parseTime(s.String)
		if err != nil {
			return err
		}
		return set(t, true)
	}
}

// jsonTarget scans a JSON column as text and decodes it into dest (a *map, *struct or *slice); a
// NULL zeroes the destination.
func jsonTarget(dest any, el reflect.Value) (any, func() error) {
	var s sql.NullString
	return &s, func() error {
		if !s.Valid {
			el.Set(reflect.Zero(el.Type()))
			return nil
		}
		if err := json.Unmarshal([]byte(s.String), dest); err != nil {
			return fmt.Errorf("decode JSON into %T: %w", dest, err)
		}
		return nil
	}
}

// scanConverted scans through database/sql, then converts what SQLite stores as text.
func scanConverted(scan func(dest ...any) error, dest []any) error {
	targets := make([]any, len(dest))
	var finish []func() error
	for i, d := range dest {
		t, fin := scanTarget(d)
		targets[i] = t
		if fin != nil {
			finish = append(finish, fin)
		}
	}
	if err := scan(targets...); err != nil {
		return err
	}
	for _, f := range finish {
		if err := f(); err != nil {
			return err
		}
	}
	return nil
}
