package email

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/safepath"
)

// ParsedAttachment is the in-memory representation of one inbound
// MIME attachment lifted off a multipart message. Holds the decoded
// bytes plus enough header context to write a sensible artifact
// record. The channel layer feeds these to PersistAttachments which
// writes the bytes to disk and creates the corresponding Artifact
// row.
type ParsedAttachment struct {
	// Filename is the operator-visible name (from Content-Disposition's
	// filename param, falling back to Content-Type's name param,
	// then to a synthesised "attachment-N.bin"). Sanitised at
	// persistence time to defeat path-traversal.
	Filename string

	// ContentType is the parsed media type (text before the ";").
	// Empty when the part advertised no Content-Type.
	ContentType string

	// Content holds the decoded body bytes — base64 / quoted-printable
	// already expanded, ready to write to disk verbatim.
	Content []byte

	// SizeBytes is the byte length of Content. Held explicitly (rather
	// than computed) so callers asserting on it don't have to recompute
	// in tests, and the persistence layer can record it without a
	// second len() pass.
	SizeBytes int64
}

// PersistedAttachment is the return value of PersistAttachments.
// Each entry holds the filesystem path the bytes landed at and a
// reference to the freshly-created Artifact row so the channel can
// surface them on conversation.ChannelMessage.Attachments.
type PersistedAttachment struct {
	// StoragePath is the absolute path on disk where the attachment
	// bytes were written.
	StoragePath string

	// Artifact is the persistence record. ID is populated by the
	// caller-provided ID-mint helper (defaults to a content hash +
	// timestamp suffix).
	Artifact *persistence.Artifact
}

// persistAttachmentDeps bundles the dependencies PersistAttachments
// needs. Holding them in one struct keeps the call signature stable
// when slice 3 wires charset transcoding / DKIM signed-blob storage.
type persistAttachmentDeps struct {
	// Repo is the artifact repository. Nil means "no artifact wiring
	// available" — the channel falls back to dropping attachments
	// with a warning.
	Repo persistence.ArtifactRepository

	// StoreDir is the on-disk directory under which per-message
	// attachment files land. Empty means "no storage configured" —
	// PersistAttachments returns an empty slice with no error so the
	// channel can degrade gracefully when the operator hasn't set up
	// attachment ingestion.
	StoreDir string

	// ProjectID scopes the Artifact row to the channel's pinned
	// project (slice 1 wiring binds the email channel to a single
	// project; slice 3 per-project routing rewires this).
	ProjectID string

	// MessageID is the inbound RFC 5322 Message-ID (angle-stripped).
	// Used to namespace the per-message subdirectory under StoreDir
	// so two messages with identical filenames don't collide.
	MessageID string

	// Now is the clock injection point for tests. Nil falls back to
	// time.Now (UTC).
	Now func() time.Time
}

// PersistAttachments writes each ParsedAttachment to disk under
// deps.StoreDir and creates a corresponding Artifact row via
// deps.Repo. Returns the persisted handles in input order so the
// caller can populate conversation.ChannelMessage.Attachments with
// the right SizeBytes / Name / ChannelRef metadata.
//
// Degradation posture:
//   - deps.Repo nil OR deps.StoreDir empty: returns ([], nil) — the
//     channel is configured to drop attachments rather than block
//     inbound delivery on missing wiring.
//   - filesystem error on any attachment: aborts and returns the
//     wrapped error. Already-written bytes from earlier attachments
//     in the batch are left on disk; the channel layer logs the
//     partial state for operator follow-up.
//   - repo Create error: same as filesystem error — abort and wrap.
func PersistAttachments(ctx context.Context, deps persistAttachmentDeps, atts []ParsedAttachment) ([]PersistedAttachment, error) {
	if deps.Repo == nil || strings.TrimSpace(deps.StoreDir) == "" {
		return nil, nil
	}
	if len(atts) == 0 {
		return nil, nil
	}
	clock := deps.Now
	if clock == nil {
		clock = func() time.Time { return time.Now().UTC() }
	}
	projectID, err := safepath.CleanPathComponent(deps.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("email: invalid attachment project id %q: %w", deps.ProjectID, err)
	}

	// Per-message subdir keeps two messages with identically-named
	// attachments from colliding. Use the angle-stripped Message-ID
	// (hashed if it contains path-unfriendly chars). deps.StoreDir is
	// already the per-project root (the container builder appends
	// projectID + "email-inbound" before handing it in for the default
	// path, or passes the operator-set path verbatim) — re-appending
	// either segment here produced the double-nested
	// .../email-inbound/<projectID>/<projectID>/email-inbound/<msg-id>/
	// shape that operators saw on artifact paths through 2026-05-21.
	subdir := safeMessageDir(deps.MessageID)
	dir := filepath.Join(deps.StoreDir, subdir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("email: mkdir attachment store %q: %w", dir, err)
	}

	out := make([]PersistedAttachment, 0, len(atts))
	for i, att := range atts {
		filename := safeAttachmentFilename(att.Filename, i)
		path, filename, err := writeAttachmentUnique(dir, filename, att.Content)
		if err != nil {
			return out, fmt.Errorf("email: write attachment %q: %w", path, err)
		}
		mime := att.ContentType
		size := att.SizeBytes
		art := &persistence.Artifact{
			ID:            mintAttachmentArtifactID(deps.MessageID, filename, clock()),
			ProjectID:     projectID,
			Name:          filename,
			ArtifactClass: persistence.ArtifactClassInput,
			StoragePath:   path,
			SizeBytes:     &size,
			MimeType:      &mime,
			CreatedAt:     clock(),
			Origin:        persistence.ArtifactOriginUpload,
		}
		if err := deps.Repo.Create(ctx, art); err != nil {
			return out, fmt.Errorf("email: record attachment artifact %q: %w", filename, err)
		}
		out = append(out, PersistedAttachment{StoragePath: path, Artifact: art})
	}
	return out, nil
}

// writeAttachmentUnique writes content under dir without clobbering
// an existing file. Duplicate filenames inside one MIME message, or
// repeated deliveries with the same Message-ID, get a stable numeric
// suffix ("report-2.pdf", "report-3.pdf", ...). O_EXCL closes the
// race where two poll cycles try to persist the same name at once.
func writeAttachmentUnique(dir, filename string, content []byte) (path string, finalName string, err error) {
	stem := strings.TrimSuffix(filename, filepath.Ext(filename))
	ext := filepath.Ext(filename)
	for n := 0; n < 10_000; n++ {
		candidate := filename
		if n > 0 {
			candidate = fmt.Sprintf("%s-%d%s", stem, n+1, ext)
		}
		path = filepath.Join(dir, candidate)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				continue
			}
			return path, candidate, err
		}
		n, err := f.Write(content)
		if err != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return path, candidate, err
		}
		if n != len(content) {
			_ = f.Close()
			_ = os.Remove(path)
			return path, candidate, fmt.Errorf("short write: wrote %d of %d bytes", n, len(content))
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(path)
			return path, candidate, err
		}
		return path, candidate, nil
	}
	return filepath.Join(dir, filename), filename, fmt.Errorf("too many duplicate attachment filenames for %q", filename)
}

// enforceAttachmentCap rejects a batch of parsed attachments when
// their total byte count exceeds cap. A zero cap means "unlimited"
// — the default when the operator hasn't set a project-level cap.
//
// Mirrors maxWebhookBodyBytes's defensive style: we refuse early so
// the channel never has to babysit a 25-MiB inbound through the rest
// of the pipeline.
func enforceAttachmentCap(atts []ParsedAttachment, cap int64) error {
	if cap <= 0 {
		return nil
	}
	var total int64
	for _, a := range atts {
		total += a.SizeBytes
	}
	if total > cap {
		return fmt.Errorf("email: attachments total %d bytes exceeds cap %d bytes", total, cap)
	}
	return nil
}

// safeMessageDir turns a Message-ID into a filesystem-safe path
// component. RFC 5322 Message-IDs may contain `<`, `>`, `@`, `.`,
// and various ASCII punctuation. We keep alphanumerics and the
// canonical "id@host" separator after replacing `@` with `_at_`,
// and substitute everything else with `-`. Empty input falls back
// to a content-hash-style placeholder so two anonymous messages
// don't collide.
func safeMessageDir(messageID string) string {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" {
		// Synthesise from a hash of the empty string + a high-precision
		// timestamp would race-collide under load; use a deterministic
		// "unknown" bucket and rely on the per-attachment filename
		// suffix for uniqueness.
		return "unknown"
	}
	var b strings.Builder
	for _, r := range messageID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '@':
			b.WriteString("_at_")
		case r == '.':
			b.WriteByte('.')
		case r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	// Cap length to avoid blowing past OS path limits on pathological
	// Message-IDs (RFC sets no upper bound).
	out := b.String()
	if len(out) > 120 {
		// Keep first 100 chars + 8-char content hash suffix so the
		// truncated form stays unique-ish across same-prefix IDs.
		h := sha256.Sum256([]byte(messageID))
		out = out[:100] + "-" + hex.EncodeToString(h[:4])
	}
	return out
}

// safeAttachmentFilename sanitises a filename so it can't escape the
// per-message directory via "../" or other path-traversal tricks,
// and supplies a deterministic fallback when the upstream MIME part
// didn't advertise a name.
func safeAttachmentFilename(raw string, index int) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Sprintf("attachment-%d.bin", index)
	}
	// Strip directory components — only the basename matters; an MUA
	// passing "../../etc/passwd" intends to traverse, not to name a
	// file with slashes in it.
	raw = filepath.Base(raw)
	// Refuse ".", "..", and any name that's still path-traversal-ish
	// after Base. filepath.Base already collapses "../etc" → "etc"
	// on Unix; the explicit guard catches the edge case where the
	// platform's Base doesn't normalise.
	if raw == "." || raw == ".." || raw == "" {
		return fmt.Sprintf("attachment-%d.bin", index)
	}
	// Disallow control chars and the filesystem-sensitive bytes that
	// some Windows-friendly bytes (\0, slash, backslash on cross-mount
	// archive extraction) can still cause grief on. Keep a permissive
	// alpha-num + common-punctuation allowlist.
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r == 0:
			b.WriteByte('_')
		case r == '/' || r == '\\':
			b.WriteByte('_')
		case r < 0x20:
			b.WriteByte('_')
		default:
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" {
		return fmt.Sprintf("attachment-%d.bin", index)
	}
	return out
}

// mintAttachmentArtifactID builds a deterministic-but-unique-ish
// artifact ID. SHA-256 of (Message-ID || filename || timestamp)
// truncated to 16 hex chars, prefixed "email-att-". Deterministic
// within a single second + same Message-ID + same filename; unique
// across distinct messages.
func mintAttachmentArtifactID(messageID, filename string, now time.Time) string {
	h := sha256.New()
	h.Write([]byte(messageID))
	h.Write([]byte{0})
	h.Write([]byte(filename))
	h.Write([]byte{0})
	h.Write([]byte(now.UTC().Format(time.RFC3339Nano)))
	return "email-att-" + hex.EncodeToString(h.Sum(nil))[:16]
}
