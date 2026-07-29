package dispatcher

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"vornik.io/vornik/internal/conversation"
)

// AttachmentStore reads persisted attachment bytes by artifact id. The
// artifact store satisfies it; nil means "no store wired", in which case
// artifact-backed attachments fall through to the other branches.
type AttachmentStore interface {
	ReadArtifact(ctx context.Context, artifactID string) ([]byte, error)
}

// fetchReason names why a fetch failed, so the caller can report a
// specific handover reason instead of a generic failure. These strings are
// metric label values — keep them stable.
const (
	reasonNoFetchSeam = "no_fetch_seam"
	reasonFetchFailed = "fetch_failed"
)

// fetchError carries a stable reason alongside the underlying cause.
type fetchError struct {
	reason string
	err    error
}

func (e *fetchError) Error() string { return e.reason + ": " + e.err.Error() }
func (e *fetchError) Unwrap() error { return e.err }

// Reason returns the stable metric-label reason for this failure.
func (e *fetchError) Reason() string { return e.reason }

// fetchAttachmentBytes resolves an inbound attachment to its bytes.
//
// Resolution order, first match wins:
//
//  1. ArtifactID — read through the artifact store. The email channel's
//     shape: it persists inbound attachments before the dispatcher runs.
//  2. ChannelRef naming a readable file under one of allowedRoots — the
//     Telegram shape, where the channel downloaded the file to a host path
//     and no artifact exists yet. Root-gated on purpose: ChannelRef is
//     channel-supplied, so without the gate a channel (or anything that
//     can influence one) could hand the dispatcher an arbitrary host path
//     to read and then feed to a model.
//  3. The channel implements conversation.AttachmentFetcher — call it.
//  4. Nothing above applies — a no_fetch_seam error, which the caller
//     turns into a handover. Slack and GitHub land here today, correctly:
//     they hand media off rather than ignoring it.
//
// maxBytes caps the read; zero means unbounded. Over the cap is an error,
// not a truncation — half an image decodes to a wrong answer, and the
// design's whole premise is that a confident wrong answer is worse than an
// honest handover.
//
// see LLD § https://docs.vornik.io §4.3.0
func fetchAttachmentBytes(
	ctx context.Context,
	ch conversation.Channel,
	store AttachmentStore,
	a conversation.Attachment,
	allowedRoots []string,
	maxBytes int64,
) ([]byte, error) {
	if a.ArtifactID != "" && store != nil {
		data, err := store.ReadArtifact(ctx, a.ArtifactID)
		if err != nil {
			return nil, &fetchError{reason: reasonFetchFailed, err: err}
		}
		if maxBytes > 0 && int64(len(data)) > maxBytes {
			return nil, &fetchError{reason: reasonFetchFailed,
				err: fmt.Errorf("artifact %s is %d bytes, over the %d-byte cap", a.ArtifactID, len(data), maxBytes)}
		}
		return data, nil
	}

	if a.ChannelRef != "" {
		if path, ok := resolveAttachmentPath(a.ChannelRef, allowedRoots); ok {
			data, err := readCapped(path, maxBytes)
			if err != nil {
				return nil, &fetchError{reason: reasonFetchFailed, err: err}
			}
			return data, nil
		}
	}

	if f, ok := ch.(conversation.AttachmentFetcher); ok {
		rc, err := f.FetchAttachment(ctx, a)
		if err != nil {
			return nil, &fetchError{reason: reasonFetchFailed, err: err}
		}
		defer func() { _ = rc.Close() }()
		data, err := readAllCapped(rc, maxBytes)
		if err != nil {
			return nil, &fetchError{reason: reasonFetchFailed, err: err}
		}
		return data, nil
	}

	return nil, &fetchError{reason: reasonNoFetchSeam,
		err: fmt.Errorf("attachment %q has no artifact id, no readable channel_ref, and channel %T cannot fetch", a.Name, ch)}
}

// resolveAttachmentPath admits a channel-supplied path only when it
// resolves (symlinks included) under one of allowedRoots and names a
// regular file.
//
// Mirrors the executor's resolveStagingSrc discipline rather than
// inventing a second policy: absolute resolution first, then the
// containment check, so a path cannot escape via .. or a symlink farm.
func resolveAttachmentPath(ref string, allowedRoots []string) (string, bool) {
	abs, err := filepath.Abs(ref)
	if err != nil {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", false
	}
	fi, err := os.Stat(resolved)
	if err != nil || !fi.Mode().IsRegular() {
		return "", false
	}
	for _, root := range allowedRoots {
		if root == "" {
			continue
		}
		absRoot, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		// Resolve the root too: on this deployment /tmp and the data
		// dir can both be symlinks, and comparing a resolved path
		// against an unresolved root would reject every legitimate file.
		if r, err := filepath.EvalSymlinks(absRoot); err == nil {
			absRoot = r
		}
		if resolved == absRoot {
			continue // a root itself is a directory, not a file
		}
		if strings.HasPrefix(resolved, absRoot+string(os.PathSeparator)) {
			return resolved, true
		}
	}
	return "", false
}

func readCapped(path string, maxBytes int64) ([]byte, error) {
	if maxBytes > 0 {
		fi, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if fi.Size() > maxBytes {
			return nil, fmt.Errorf("%s is %d bytes, over the %d-byte cap", filepath.Base(path), fi.Size(), maxBytes)
		}
	}
	return os.ReadFile(path)
}

// readAllCapped reads at most maxBytes+1 and errors when the extra byte
// materialises, so an oversized stream is refused rather than silently
// truncated into a half-decodable image.
func readAllCapped(r io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 {
		return io.ReadAll(r)
	}
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("payload exceeds the %d-byte cap", maxBytes)
	}
	return data, nil
}
