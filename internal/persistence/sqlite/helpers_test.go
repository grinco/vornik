package sqlite

import (
	"testing"
	"time"
)

// TestSqliteStringArray_RoundTrip walks every branch of Scan + Value.
func TestSqliteStringArray_RoundTrip(t *testing.T) {
	// Nil scan.
	var a sqliteStringArray
	if err := a.Scan(nil); err != nil {
		t.Fatalf("Scan(nil): %v", err)
	}
	if a != nil {
		t.Errorf("nil scan should yield nil, got %v", a)
	}

	// Empty bytes.
	if err := a.Scan([]byte{}); err != nil {
		t.Fatalf("Scan(empty): %v", err)
	}
	if a != nil {
		t.Errorf("empty scan should yield nil")
	}

	// String input.
	if err := a.Scan(`["x","y"]`); err != nil {
		t.Fatalf("Scan(string): %v", err)
	}
	if len(a) != 2 || a[0] != "x" || a[1] != "y" {
		t.Errorf("after scan = %v", a)
	}

	// Byte input.
	if err := a.Scan([]byte(`["z"]`)); err != nil {
		t.Fatalf("Scan(bytes): %v", err)
	}
	if len(a) != 1 || a[0] != "z" {
		t.Errorf("after bytes scan = %v", a)
	}

	// Unsupported type.
	if err := a.Scan(42); err == nil {
		t.Error("Scan(int) should error")
	}

	// Bad JSON.
	if err := a.Scan(`not-json`); err == nil {
		t.Error("Scan(bad json) should error")
	}

	// Value()
	var nilArr sqliteStringArray
	v, err := nilArr.Value()
	if err != nil || v != "[]" {
		t.Errorf("nil Value = %v / %v", v, err)
	}
	arr := sqliteStringArray{"a", "b"}
	v, err = arr.Value()
	if err != nil {
		t.Fatalf("Value: %v", err)
	}
	if s, ok := v.(string); !ok || s != `["a","b"]` {
		t.Errorf("Value = %v", v)
	}
}

// TestSqliteTimeHelpers exercises sqliteTime/sqliteTimePtr/parseSqliteTime
// across the various input shapes.
func TestSqliteTimeHelpers(t *testing.T) {
	t0 := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	if got := sqliteTime(t0); got != "2026-05-20T12:00:00Z" {
		t.Errorf("sqliteTime = %q", got)
	}
	if v := sqliteTimePtr(nil); v != nil {
		t.Errorf("nil ptr should yield nil interface, got %v", v)
	}
	if v := sqliteTimePtr(&t0); v == nil {
		t.Error("non-nil ptr should yield string")
	}

	// parseSqliteTime — RFC3339Nano + alternatives.
	for _, layout := range []string{
		"2026-05-20T12:00:00Z",
		"2026-05-20T12:00:00.123456789Z",
		"2026-05-20 12:00:00",
		"2026-05-20 12:00:00.123",
	} {
		if _, err := parseSqliteTime(layout); err != nil {
			t.Errorf("parseSqliteTime(%q): %v", layout, err)
		}
	}
	if _, err := parseSqliteTime("not-a-time"); err == nil {
		t.Error("expected parse failure")
	}
	if v, err := parseSqliteTime(""); err != nil || !v.IsZero() {
		t.Errorf("empty should yield zero, got v=%v err=%v", v, err)
	}
}

// TestSqlTime_Scan tries the supported source types.
func TestSqlTime_Scan(t *testing.T) {
	var st sqlTime
	if err := st.Scan(nil); err != nil {
		t.Errorf("Scan(nil): %v", err)
	}
	if !st.Time.IsZero() {
		t.Error("nil scan should yield zero")
	}
	now := time.Now().UTC()
	if err := st.Scan(now); err != nil {
		t.Fatalf("Scan(time): %v", err)
	}
	if !st.Time.Equal(now) {
		t.Error("Scan(time) lost value")
	}
	if err := st.Scan("2026-05-20T12:00:00Z"); err != nil {
		t.Fatalf("Scan(string): %v", err)
	}
	if err := st.Scan([]byte("2026-05-20T12:00:00Z")); err != nil {
		t.Fatalf("Scan(bytes): %v", err)
	}
	if err := st.Scan(42); err == nil {
		t.Error("Scan(int) should error")
	}
	if err := st.Scan("not-a-time"); err == nil {
		t.Error("Scan(bad string) should error")
	}
}

// TestSqlNullTime_Scan covers the null + non-null branches.
func TestSqlNullTime_Scan(t *testing.T) {
	var nt sqlNullTime
	if err := nt.Scan(nil); err != nil {
		t.Fatalf("Scan(nil): %v", err)
	}
	if nt.Valid {
		t.Error("nil should be invalid")
	}
	if err := nt.Scan("2026-05-20T12:00:00Z"); err != nil {
		t.Fatalf("Scan(string): %v", err)
	}
	if !nt.Valid {
		t.Error("non-zero time should be Valid")
	}
	if err := nt.Scan(42); err == nil {
		t.Error("Scan(int) should error")
	}
}
