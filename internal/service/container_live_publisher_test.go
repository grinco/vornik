package service

import "testing"

// TestPostgresDSNFromConfig_ShapesDSN pins the DSN string the
// listener feeds lib/pq. Drift here would silently break
// cross-replica fanout on the next deploy.
func TestPostgresDSNFromConfig_ShapesDSN(t *testing.T) {
	got := postgresDSNFromConfig("localhost", 5432, "vornik", "secret", "vornik_test", "disable")
	want := "host=localhost port=5432 user=vornik password=secret dbname=vornik_test sslmode=disable"
	if got != want {
		t.Errorf("DSN = %q, want %q", got, want)
	}
}

// TestPostgresDSNFromConfig_DefaultsSSLMode confirms an empty
// sslmode coerces to "disable". The daemon's no-surprise-SSL
// stance.
func TestPostgresDSNFromConfig_DefaultsSSLMode(t *testing.T) {
	got := postgresDSNFromConfig("h", 5432, "u", "p", "d", "")
	if got != "host=h port=5432 user=u password=p dbname=d sslmode=disable" {
		t.Errorf("DSN = %q", got)
	}
}

// TestEscapeDSN_BareValuePassthrough — alphanumeric values
// don't get quoted (libpq accepts them as-is).
func TestEscapeDSN_BareValuePassthrough(t *testing.T) {
	for _, v := range []string{"vornik", "vornik_test", "127.0.0.1", "5432", "verify-full"} {
		got := escapeDSN(v)
		if got != v {
			t.Errorf("escapeDSN(%q) = %q, want passthrough", v, got)
		}
	}
}

// TestEscapeDSN_SpaceQuotes — values containing whitespace
// must be wrapped in single quotes so libpq parses them as one
// field.
func TestEscapeDSN_SpaceQuotes(t *testing.T) {
	got := escapeDSN("hello world")
	want := "'hello world'"
	if got != want {
		t.Errorf("escapeDSN = %q, want %q", got, want)
	}
}

// TestEscapeDSN_SingleQuoteEscaped — values with an embedded
// quote get escaped + wrapped. Without this libpq's parser
// stops at the inner quote and the remainder of the DSN gets
// dropped.
func TestEscapeDSN_SingleQuoteEscaped(t *testing.T) {
	got := escapeDSN("it's")
	want := `'it\'s'`
	if got != want {
		t.Errorf("escapeDSN = %q, want %q", got, want)
	}
}

// TestEscapeDSN_BackslashEscaped — same shape for embedded
// backslashes.
func TestEscapeDSN_BackslashEscaped(t *testing.T) {
	got := escapeDSN(`a\b`)
	want := `'a\\b'`
	if got != want {
		t.Errorf("escapeDSN = %q, want %q", got, want)
	}
}

// TestEscapeDSN_EmptyPassthrough — empty input round-trips to
// empty. libpq treats blank fields as "use the default" for
// many params; preserving the blank lets that pathway work.
func TestEscapeDSN_EmptyPassthrough(t *testing.T) {
	if got := escapeDSN(""); got != "" {
		t.Errorf("escapeDSN(\"\") = %q, want empty", got)
	}
}
