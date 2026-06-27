package sqlite

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// DBTX is the narrow surface every SQLite repository depends on —
// aliased to persistence.DBTX so the shared test suite at
// internal/persistence/repotest can construct repos from either
// backend with one type.
type DBTX = persistence.DBTX

// sqliteStringArray round-trips a Go []string through a JSON-encoded
// TEXT column. Implements sql.Scanner + driver.Valuer so repos can
// pass it directly to ExecContext / Scan.
//
// Empty slices marshal to `[]` rather than NULL so downstream
// json.Unmarshal calls don't need to handle the nil-string case;
// matches the Postgres-side TEXT[] DEFAULT '{}' semantics.
type sqliteStringArray []string

func (a sqliteStringArray) Value() (driver.Value, error) {
	if a == nil {
		return "[]", nil
	}
	b, err := json.Marshal([]string(a))
	if err != nil {
		return nil, fmt.Errorf("sqlite: marshal string array: %w", err)
	}
	return string(b), nil
}

func (a *sqliteStringArray) Scan(src interface{}) error {
	if src == nil {
		*a = nil
		return nil
	}
	var raw []byte
	switch v := src.(type) {
	case string:
		raw = []byte(v)
	case []byte:
		raw = v
	default:
		return fmt.Errorf("sqlite: cannot scan %T into string array", src)
	}
	if len(raw) == 0 {
		*a = nil
		return nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return fmt.Errorf("sqlite: unmarshal string array: %w", err)
	}
	*a = out
	return nil
}

// sqliteTime renders a time.Time as RFC3339Nano. modernc.org/sqlite
// doesn't auto-convert TEXT → time.Time on Scan, so the round-trip
// goes through string at both ends — sqliteTime to encode + scanTime
// / sqlTime to decode.
func sqliteTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// sqliteTimePtr is the nullable companion; returns nil when t is
// the zero pointer so the column lands as NULL rather than "".
func sqliteTimePtr(t *time.Time) interface{} {
	if t == nil {
		return nil
	}
	return sqliteTime(*t)
}

// sqlTime implements sql.Scanner over a time.Time backing field.
// Accepts both `time.Time` and TEXT representations the modernc
// driver emits (RFC3339 / RFC3339Nano / SQLite default
// "YYYY-MM-DD HH:MM:SS"). Use this in struct scan paths where the
// column is non-nullable.
type sqlTime struct {
	Time time.Time
}

func (s *sqlTime) Scan(src interface{}) error {
	if src == nil {
		s.Time = time.Time{}
		return nil
	}
	switch v := src.(type) {
	case time.Time:
		s.Time = v
		return nil
	case string:
		t, err := parseSqliteTime(v)
		if err != nil {
			return fmt.Errorf("sqlite: parse time %q: %w", v, err)
		}
		s.Time = t
		return nil
	case []byte:
		t, err := parseSqliteTime(string(v))
		if err != nil {
			return fmt.Errorf("sqlite: parse time %q: %w", v, err)
		}
		s.Time = t
		return nil
	default:
		return fmt.Errorf("sqlite: cannot scan %T into time.Time", src)
	}
}

// sqlNullTime mirrors sql.NullTime but adds the same multi-format
// parser as sqlTime so it round-trips through SQLite's TEXT columns.
type sqlNullTime struct {
	Time  time.Time
	Valid bool
}

func (s *sqlNullTime) Scan(src interface{}) error {
	if src == nil {
		s.Time, s.Valid = time.Time{}, false
		return nil
	}
	var inner sqlTime
	if err := inner.Scan(src); err != nil {
		return err
	}
	s.Time = inner.Time
	s.Valid = !inner.Time.IsZero()
	return nil
}

// ptrFloatOrNil converts a *float64 to an untyped nil (SQL NULL) when the
// pointer is nil, or to the dereferenced float64 value when non-nil.
// This mirrors the postgres ptrFloatOrNil helper so that nullable numeric
// columns (e.g. commission_usd) bind correctly across both backends.
func ptrFloatOrNil(f *float64) any {
	if f == nil {
		return nil
	}
	return *f
}

func parseSqliteTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	// Try the formats most likely to appear, ordered by what the
	// daemon writes (RFC3339Nano via sqliteTime) followed by the
	// SQLite default datetime('now') format.
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("no matching layout")
}
