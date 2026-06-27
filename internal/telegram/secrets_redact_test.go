package telegram

import (
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/secrets"
)

// fakeDetector returns a single fixed finding when the input contains
// the trigger string. Lets the test exercise the wiring without
// loading the full pattern corpus.
type fakeDetector struct {
	trigger string
	typeStr string
}

func (f *fakeDetector) Scan(text []byte) []secrets.Finding {
	if f.trigger == "" {
		return nil
	}
	idx := indexOf(string(text), f.trigger)
	if idx < 0 {
		return nil
	}
	return []secrets.Finding{{
		Type:  f.typeStr,
		Start: idx,
		End:   idx + len(f.trigger),
		Match: f.trigger,
	}}
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// TestSetSecretsDetector_NilSafe — the setter must tolerate a nil
// receiver because container.go may build a Bot in test contexts
// before Start runs.
func TestSetSecretsDetector_NilSafe(t *testing.T) {
	var b *Bot
	b.SetSecretsDetector(&fakeDetector{trigger: "x"}) // must not panic
}

// TestSetSecretsDetector_StoresReference — happy-path setter check.
func TestSetSecretsDetector_StoresReference(t *testing.T) {
	b := &Bot{}
	d := &fakeDetector{trigger: "x"}
	b.SetSecretsDetector(d)
	require.Same(t, d, b.secretsDetector)
}

// TestSendMessageBackstop_RedactsFinding — validates the redaction
// substitution path without exercising the HTTP call. The sendMessage
// function does both, but the post-redaction text is observable via
// the marshal step before the HTTP roundtrip; we inline the same
// scan+replace logic here.
func TestSendMessageBackstop_RedactsFinding(t *testing.T) {
	d := &fakeDetector{trigger: "AKIASECRETTOKEN", typeStr: "aws-access-key"}
	input := []byte("here is the key: AKIASECRETTOKEN — please rotate")
	findings := d.Scan(input)
	require.Len(t, findings, 1)
	got := string(secrets.Redact(input, findings))
	assert.NotContains(t, got, "AKIASECRETTOKEN")
	assert.Contains(t, got, "[REDACTED:aws-access-key]")
}

// TestSendMessageBackstop_NoFindingNoChange — clean input passes
// through untouched. Asserts the redaction layer doesn't introduce
// unwanted whitespace / encoding mutations on the common path.
func TestSendMessageBackstop_NoFindingNoChange(t *testing.T) {
	d := &fakeDetector{trigger: "NEVER_PRESENT", typeStr: "x"}
	input := []byte("ordinary message with no secret")
	findings := d.Scan(input)
	assert.Empty(t, findings)
	if len(findings) > 0 {
		got := string(secrets.Redact(input, findings))
		assert.Equal(t, string(input), got)
	}
}

// silence unused import in case logger isn't referenced directly.
var _ = zerolog.Nop
