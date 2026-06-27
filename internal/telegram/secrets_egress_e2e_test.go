package telegram

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/secrets"
)

// secrets_egress_e2e_test.go — cross-layer E2E for the chat-reply
// egress sink (Tier 1, https://docs.vornik.io: "secret redaction across
// persistence").
//
// Why this exists alongside secrets_redact_test.go: that file
// inlines the scan+Redact logic and asserts on the helper output.
// It never drives the real sendMessage / sendForumMessage egress
// path, so it cannot prove the secret is gone from the bytes that
// actually leave the process. This test wires a REAL
// secrets.MultiDetector (the production pattern corpus, not a
// fake) into a Bot and captures the outbound HTTP request body the
// telegram transport would put on the wire. The assertion is the
// security-relevant one: the raw secret never reaches the sink.
//
// Characterization: redaction is already wired at both egress
// points, so these tests PASS. A failure here means a real secret
// leak at the chat-reply boundary.

// captureTransport records the body of every outbound request and
// answers with a canned Telegram "ok" response so the egress code
// path runs to completion (parse + return message_id). No network.
type captureTransport struct {
	bodies []string
}

func (c *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		c.bodies = append(c.bodies, string(b))
	}
	const ok = `{"ok":true,"result":{"message_id":42,"chat":{"id":1}}}`
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(ok))),
		Request:    req,
	}, nil
}

// onWire returns the concatenation of every captured request body so
// a test can assert across all sends in one shot.
func (c *captureTransport) onWire() string { return strings.Join(c.bodies, "\n") }

// realDetectorBot builds a Bot with the production secrets detector
// wired and a capture transport in place of the network. forumChatID
// is set so sendForumMessage's preconditions pass.
func realDetectorBot(t *testing.T) (*Bot, *captureTransport) {
	t.Helper()
	det, err := secrets.NewMultiDetector(secrets.Config{}) // default = full production corpus
	require.NoError(t, err)
	ct := &captureTransport{}
	b := &Bot{
		baseURL:         "https://api.telegram.org/botTEST",
		httpClient:      &http.Client{Transport: ct},
		logger:          zerolog.Nop(),
		secretsDetector: det,
		forumChatID:     999,
	}
	return b, ct
}

// secretFixtures are real-shaped credentials drawn from the
// production DefaultPatterns() corpus. Each must be scrubbed from
// any egress body; the typed marker proves the detector fired with
// the right classification.
var secretFixtures = []struct {
	name   string
	raw    string
	marker string
}{
	{"openai_key", "sk-proj1234567890abcdefghijklmnopqrstuv", "[REDACTED:openai_key]"},
	// NB: NOT the canonical AKIAIOSFODNN7EXAMPLE — that contains
	// "EXAMPLE", which DefaultAllowlist() suppresses on purpose (AWS
	// docs placeholder). A realistic key with no allowlisted token
	// must redact. See TestChatReplyEgress_AWSDocsPlaceholderAllowlisted.
	{"aws_access_key", "AKIA2E0A8F3B9C1D7K4Q", "[REDACTED:aws_access_key]"},
	{"github_pat", "ghp_abcdefghijklmnopqrstuvwxyz0123456789", "[REDACTED:github_pat]"},
}

// TestChatReplyEgress_RedactsBeforeWire drives the real
// sendMessageWithMarkup egress path (the Telegram chat-reply sink)
// with a leaked secret and asserts the raw value never appears in
// the bytes that hit the transport — the typed marker does.
func TestChatReplyEgress_RedactsBeforeWire(t *testing.T) {
	for _, f := range secretFixtures {
		t.Run(f.name, func(t *testing.T) {
			b, ct := realDetectorBot(t)
			msg := "here is the leaked credential: " + f.raw + " — rotate it now"

			err := b.sendMessageWithMarkup(context.Background(), 12345, msg, nil)
			require.NoError(t, err)
			require.Len(t, ct.bodies, 1, "exactly one send should have hit the transport")

			wire := ct.onWire()
			assert.NotContains(t, wire, f.raw,
				"raw %s must NOT reach the telegram wire (chat-reply leak)", f.name)
			assert.Contains(t, wire, f.marker,
				"typed redaction marker must be present in the wire body")
			// Surrounding prose must survive — no over-redaction.
			assert.Contains(t, wire, "rotate it now")
		})
	}
}

// TestForumReplyEgress_RedactsBeforeWire mirrors the chat-reply test
// for the forum-topic egress path (sendForumMessage). A poisoned
// task summary delivered to a forum thread must be scrubbed on the
// same boundary.
func TestForumReplyEgress_RedactsBeforeWire(t *testing.T) {
	for _, f := range secretFixtures {
		t.Run(f.name, func(t *testing.T) {
			b, ct := realDetectorBot(t)
			msg := "task summary mentioned " + f.raw + " in the build log"

			id, err := b.sendForumMessage(context.Background(), 77, msg)
			require.NoError(t, err)
			assert.Equal(t, int64(42), id)
			require.Len(t, ct.bodies, 1)

			wire := ct.onWire()
			assert.NotContains(t, wire, f.raw,
				"raw %s must NOT reach the forum wire", f.name)
			assert.Contains(t, wire, f.marker)
			assert.Contains(t, wire, "in the build log")
		})
	}
}

// TestChatReplyEgress_MultipleSecretsOneMessage — a single message
// carrying several distinct secret types must have ALL of them
// scrubbed in one pass at the egress boundary (the Redact left-to-
// right pass over Scan's ordered findings).
func TestChatReplyEgress_MultipleSecretsOneMessage(t *testing.T) {
	b, ct := realDetectorBot(t)
	msg := "dump: openai=sk-proj1234567890abcdefghijklmnopqrstuv " +
		"aws=AKIA2E0A8F3B9C1D7K4Q " +
		"gh=ghp_abcdefghijklmnopqrstuvwxyz0123456789 done"

	require.NoError(t, b.sendMessageWithMarkup(context.Background(), 1, msg, nil))
	wire := ct.onWire()

	for _, f := range secretFixtures {
		assert.NotContainsf(t, wire, f.raw, "%s leaked to the wire", f.name)
		assert.Containsf(t, wire, f.marker, "%s marker missing", f.name)
	}
	assert.Contains(t, wire, "done", "trailing prose must survive multi-redaction")
}

// TestChatReplyEgress_NotDisableable_RealCorpus — there is no
// per-message / per-checkpoint knob that turns OFF redaction on the
// chat-reply egress path: the only gate is whether a detector is
// wired at all (production always wires one). This is the egress-
// boundary analogue of the ResolveAction memory clamp covered in
// internal/secrets (TestResolveAction_MemoryScanNonDisableable).
// Here we assert the behavioural invariant: with the production
// detector present, the secret is unconditionally scrubbed — the
// send path exposes no "detect-only" escape that would let the raw
// value through.
func TestChatReplyEgress_NotDisableable_RealCorpus(t *testing.T) {
	b, ct := realDetectorBot(t)
	// Even VORNIK_ALLOW_UNSCANNED_MEMORY (the memory-checkpoint escape
	// hatch) must not influence the chat egress backstop — it is
	// scoped to ResolveAction(memory), a different sink. Set it to
	// prove the chat path ignores it.
	t.Setenv("VORNIK_ALLOW_UNSCANNED_MEMORY", "1")

	const raw = "sk-proj1234567890abcdefghijklmnopqrstuv"
	require.NoError(t, b.sendMessageWithMarkup(context.Background(), 1,
		"leak "+raw+" end", nil))

	wire := ct.onWire()
	assert.NotContains(t, wire, raw,
		"chat-reply redaction must not be disableable via the memory escape hatch")
	assert.Contains(t, wire, "[REDACTED:openai_key]")
}

// TestChatReplyEgress_AWSDocsPlaceholderAllowlisted — characterizes a
// deliberate non-redaction: the canonical AWS docs key
// AKIAIOSFODNN7EXAMPLE matches the aws_access_key regex but is
// suppressed by DefaultAllowlist()'s "example" rule, so it passes
// through to the wire. This is by design (it is a documented
// placeholder, not a live credential); the test pins the behaviour
// so a future allowlist change that would start redacting it — or,
// worse, one that broadens "example" suppression to swallow real
// keys — surfaces as a deliberate decision rather than a silent
// drift. A REAL key (covered above) still redacts.
func TestChatReplyEgress_AWSDocsPlaceholderAllowlisted(t *testing.T) {
	b, ct := realDetectorBot(t)
	const placeholder = "AKIAIOSFODNN7EXAMPLE"
	require.NoError(t, b.sendMessageWithMarkup(context.Background(), 1,
		"sample config: "+placeholder, nil))
	wire := ct.onWire()
	assert.Contains(t, wire, placeholder,
		"the AWS docs placeholder is allowlisted and intentionally not redacted")
	assert.NotContains(t, wire, "[REDACTED",
		"no redaction marker — the allowlist suppressed the only finding")
}

// TestChatReplyEgress_CleanMessageUntouched — the common path: a
// message with no secret must traverse the egress boundary byte-for-
// byte (modulo JSON encoding), proving the backstop doesn't mutate
// clean traffic.
func TestChatReplyEgress_CleanMessageUntouched(t *testing.T) {
	b, ct := realDetectorBot(t)
	const clean = "your task finished successfully with 3 artifacts"
	require.NoError(t, b.sendMessageWithMarkup(context.Background(), 1, clean, nil))
	assert.Contains(t, ct.onWire(), clean)
	assert.NotContains(t, ct.onWire(), "[REDACTED")
}
