package artifacts

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/secrets"
)

// auditNewStore builds an artifact Store with a real MultiDetector in
// the default (redact) action, backed by t.TempDir for isolation.
// Distinct helper name to avoid clashing with the existing
// newTestStoreWithSecrets helper in store_secrets_test.go.
func auditNewStore(t *testing.T) *Store {
	t.Helper()
	det, err := secrets.NewMultiDetector(secrets.Config{})
	require.NoError(t, err)
	store, err := New(
		WithBasePath(t.TempDir()),
		WithRepository(NewMockArtifactRepo()),
		WithLogger(zerolog.Nop()),
		WithSecrets(det, nil),
	)
	require.NoError(t, err)
	return store
}

func auditWriteSource(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(p, []byte(body), 0644))
	return p
}

// auditAllowlistGapNames covers the credential-bearing text formats
// that the old enumerated extension allowlist (mimeIsScanCandidate
// only) let bypass redaction: .log, .csv, .env, .conf, .pem, .sh, and
// an extension-less file. Pre-fix, detectMimeType returned
// application/octet-stream (or text/plain for none of these), and the
// narrow allowlist skipped the scan, so the secret was stored
// verbatim. Post-fix, the body-sniff backstop scans them.
var auditAllowlistGapNames = []string{
	"app.log",
	"export.csv",
	".env",
	"settings.conf",
	"server.pem",
	"deploy.sh",
	"credentials", // no extension -> application/octet-stream
}

// TestAudit_AllowlistGapFormatsAreRedacted is the headline regression
// test for the scan-allowlist-bypass finding. Each of these textual
// formats carrying an OpenAI key must be redacted before the bytes
// land in storage. Pre-fix these were served verbatim on download.
func TestAudit_AllowlistGapFormatsAreRedacted(t *testing.T) {
	const secret = "sk-proj1234567890abcdefghijklmnopqrstuv"
	for _, name := range auditAllowlistGapNames {
		t.Run(name, func(t *testing.T) {
			store := auditNewStore(t)
			src := auditWriteSource(t, name,
				"config line\nAPI_KEY="+secret+"\ntrailing safe text")

			art, err := store.Store(context.Background(), "p1", "e1", "t1", name, src)
			require.NoError(t, err)

			stored, err := os.ReadFile(art.StoragePath)
			require.NoError(t, err)
			assert.NotContains(t, string(stored), secret,
				"textual artifact %q must be scanned+redacted, not served verbatim", name)
			assert.Contains(t, string(stored), "[REDACTED:openai_key]",
				"%q should carry the redaction marker", name)
			assert.Contains(t, string(stored), "trailing safe text",
				"%q surrounding text must survive redaction", name)
		})
	}
}

// TestAudit_BinaryBlobStillSkipped guards against the body-sniff
// backstop over-scanning genuine binary content. A .png with a NUL
// run and high-entropy bytes must pass through byte-for-byte (a
// regression here would corrupt binary artifacts).
func TestAudit_BinaryBlobStillSkipped(t *testing.T) {
	store := auditNewStore(t)
	bin := make([]byte, 512)
	for i := range bin {
		bin[i] = byte(i % 256)
	}
	bin[10] = 0x00 // explicit NUL: unambiguous binary marker
	src := filepath.Join(t.TempDir(), "blob.png")
	require.NoError(t, os.WriteFile(src, bin, 0644))

	art, err := store.Store(context.Background(), "p1", "e1", "t1", "blob.png", src)
	require.NoError(t, err)

	stored, err := os.ReadFile(art.StoragePath)
	require.NoError(t, err)
	assert.Equal(t, bin, stored, "binary types must be stored byte-for-byte")
}

// TestAudit_OctetStreamWithNULSkipped ensures an unknown-extension
// artifact whose bytes contain a NUL (binary signal) is NOT scanned —
// shouldScanBody must treat NUL-bearing octet-stream as binary so
// detector regexes can't corrupt it.
func TestAudit_OctetStreamWithNULSkipped(t *testing.T) {
	store := auditNewStore(t)
	body := append([]byte("sk-proj1234567890abcdefghijklmnopqrstuv"), 0x00, 0x01, 0x02)
	src := filepath.Join(t.TempDir(), "weird.bin")
	require.NoError(t, os.WriteFile(src, body, 0644))

	art, err := store.Store(context.Background(), "p1", "e1", "t1", "weird.bin", src)
	require.NoError(t, err)

	stored, err := os.ReadFile(art.StoragePath)
	require.NoError(t, err)
	assert.Equal(t, body, stored,
		"NUL-bearing octet-stream is binary; must pass through unmodified")
}

// TestAudit_ShouldScanBodyDecisions exercises the decision primitive
// directly across the categories the finding cares about.
func TestAudit_ShouldScanBodyDecisions(t *testing.T) {
	textBody := []byte("plain readable text with API_KEY=value here")
	binBody := []byte{0x00, 0x01, 0x02, 0xff, 0xfe}

	cases := []struct {
		name string
		mime string
		body []byte
		want bool
	}{
		{"known text mime", "text/plain", textBody, true},
		{"newly added csv mime", "text/csv", textBody, true},
		{"newly added sh mime", "application/x-sh", textBody, true},
		{"image is binary", "image/png", textBody, false},
		{"pdf is binary", "application/pdf", textBody, false},
		{"zip is binary", "application/zip", binBody, false},
		{"octet-stream textual body sniffs as scannable", "application/octet-stream", textBody, true},
		{"octet-stream binary body skipped", "application/octet-stream", binBody, false},
		{"empty body skipped", "application/octet-stream", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, shouldScanBody(c.mime, c.body))
		})
	}
}

// TestAudit_DetectMimeTypeCoversTextFormats verifies the secondary
// improvement: the previously-unmapped text extensions now resolve to
// scannable text MIME types rather than application/octet-stream.
func TestAudit_DetectMimeTypeCoversTextFormats(t *testing.T) {
	cases := map[string]string{
		"a.csv":  "text/csv",
		"a.xml":  "text/xml",
		"a.toml": "application/toml",
		"a.sh":   "application/x-sh",
		"a.log":  "text/plain",
		"a.ini":  "text/plain",
		"a.conf": "text/plain",
		"a.env":  "text/plain",
		"a.pem":  "text/plain",
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			got := detectMimeType(name)
			assert.Equal(t, want, got)
			assert.True(t, mimeIsScanCandidate(got),
				"%s -> %s must be a scan candidate", name, got)
		})
	}
}
