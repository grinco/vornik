package secrets

import "testing"

func TestFilterAllowlisted(t *testing.T) {
	findings := []Finding{
		{Type: FindingTypeGenericKV, Match: "password: hunter2-xY9pQ", Start: 0, End: 23},
		{Type: "openai_key", Match: "sk-proj123", Start: 30, End: 40},
	}

	t.Run("empty allowlist is identity", func(t *testing.T) {
		got := FilterAllowlisted(findings, nil)
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2", len(got))
		}
	})

	t.Run("drops heuristic finding overlapping an allowlisted value (value inside match)", func(t *testing.T) {
		got := FilterAllowlisted(findings, [][]byte{[]byte("hunter2-xY9pQ")})
		if len(got) != 1 || got[0].Type != "openai_key" {
			t.Fatalf("got %+v, want only the openai_key finding", got)
		}
	})

	t.Run("drops heuristic finding when match is inside the allowlisted value", func(t *testing.T) {
		got := FilterAllowlisted([]Finding{{Type: FindingTypeGenericKV, Match: "hunter2x"}}, [][]byte{[]byte("hunter2x-xY9-full")})
		if len(got) != 0 {
			t.Fatalf("got %+v, want empty (match is substring of allowed)", got)
		}
	})

	// SECURITY: a strong-pattern finding is NEVER eligible for allowlisting,
	// even if an allowlisted value is a substring of its matched span — so an
	// allowlisted credential can't be used to suppress a real key.
	t.Run("strong-pattern finding not suppressed even when allowlisted value is a substring", func(t *testing.T) {
		strong := []Finding{{Type: "openai_key", Match: "hunter2xsk-proj1234567890abcdef"}}
		got := FilterAllowlisted(strong, [][]byte{[]byte("hunter2xsk-proj")})
		if len(got) != 1 {
			t.Fatalf("strong finding must survive allowlisting, got %+v", got)
		}
	})

	// A too-short allowlist entry is ignored (would match too broadly).
	t.Run("short allowlist entry is ignored", func(t *testing.T) {
		got := FilterAllowlisted([]Finding{{Type: FindingTypeGenericKV, Match: "password: abc"}}, [][]byte{[]byte("abc")})
		if len(got) != 1 {
			t.Fatalf("short allowlist entry must not suppress, got %+v", got)
		}
	})

	t.Run("does not drop unrelated strong secret", func(t *testing.T) {
		got := FilterAllowlisted(findings, [][]byte{[]byte("some-other-value")})
		if len(got) != 2 {
			t.Fatalf("len = %d, want 2 (nothing allowlisted)", len(got))
		}
	})

	t.Run("empty allowlist entries are ignored", func(t *testing.T) {
		got := FilterAllowlisted(findings, [][]byte{{}, []byte("hunter2-xY9pQ")})
		if len(got) != 1 {
			t.Fatalf("len = %d, want 1", len(got))
		}
	})
}
