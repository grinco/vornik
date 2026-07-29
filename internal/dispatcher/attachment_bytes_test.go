package dispatcher

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vornik.io/vornik/internal/conversation"
)

type stubStore struct {
	data map[string][]byte
	err  error
}

func (s stubStore) ReadArtifact(_ context.Context, id string) ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	d, ok := s.data[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return d, nil
}

// plainChannel implements conversation.Channel but NOT AttachmentFetcher —
// the Slack/GitHub shape.
type plainChannel struct{ conversation.Channel }

// fetchingChannel implements the optional interface.
type fetchingChannel struct {
	conversation.Channel
	payload []byte
	err     error
}

func (f fetchingChannel) FetchAttachment(_ context.Context, _ conversation.Attachment) (io.ReadCloser, error) {
	if f.err != nil {
		return nil, f.err
	}
	return io.NopCloser(strings.NewReader(string(f.payload))), nil
}

// Email's shape: the channel persisted the attachment, so ArtifactID wins.
func TestFetchAttachmentBytes_ArtifactIDPath(t *testing.T) {
	store := stubStore{data: map[string][]byte{"art-1": []byte("PIXELS")}}
	got, err := fetchAttachmentBytes(context.Background(), plainChannel{}, store,
		conversation.Attachment{Name: "photo.jpg", ArtifactID: "art-1"}, nil, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != "PIXELS" {
		t.Errorf("got %q", got)
	}
}

// Telegram's shape: no artifact yet, bytes are on disk under an allowed root.
func TestFetchAttachmentBytes_ChannelRefUnderAllowedRoot(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "photo.jpg")
	if err := os.WriteFile(p, []byte("JPEGBYTES"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := fetchAttachmentBytes(context.Background(), plainChannel{}, nil,
		conversation.Attachment{Name: "photo.jpg", ChannelRef: p}, []string{dir}, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != "JPEGBYTES" {
		t.Errorf("got %q", got)
	}
}

// A channel-supplied path outside the allowlist must be refused, not read.
// ChannelRef is channel-controlled input; without this gate a path could be
// read off the host and fed to a model.
func TestFetchAttachmentBytes_ChannelRefOutsideRootsRefused(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	p := filepath.Join(outside, "secret.jpg")
	if err := os.WriteFile(p, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := fetchAttachmentBytes(context.Background(), plainChannel{}, nil,
		conversation.Attachment{Name: "secret.jpg", ChannelRef: p}, []string{dir}, 0)
	if err == nil {
		t.Fatal("a path outside the allowed roots must not be read")
	}
	var fe *fetchError
	if !errors.As(err, &fe) || fe.Reason() != reasonNoFetchSeam {
		t.Errorf("want a no_fetch_seam refusal, got %v", err)
	}
}

// Traversal out of an allowed root must not succeed either.
func TestFetchAttachmentBytes_TraversalRefused(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "..", "escaped.jpg")
	if err := os.WriteFile(filepath.Clean(outside), []byte("X"), 0o600); err != nil {
		t.Skipf("cannot stage traversal fixture: %v", err)
	}
	defer func() { _ = os.Remove(filepath.Clean(outside)) }()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := fetchAttachmentBytes(context.Background(), plainChannel{}, nil,
		conversation.Attachment{Name: "x", ChannelRef: filepath.Join(sub, "..", "..", "escaped.jpg")},
		[]string{sub}, 0)
	if err == nil {
		t.Fatal("traversal above the allowed root must be refused")
	}
}

func TestFetchAttachmentBytes_OptionalFetcherUsedLast(t *testing.T) {
	got, err := fetchAttachmentBytes(context.Background(),
		fetchingChannel{payload: []byte("FROMCHANNEL")}, nil,
		conversation.Attachment{Name: "x.png"}, nil, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(got) != "FROMCHANNEL" {
		t.Errorf("got %q", got)
	}
}

// A channel with no seam at all must produce the specific no_fetch_seam
// reason so the caller reports WHY it handed over rather than a generic
// failure. Slack and GitHub are in this state today.
func TestFetchAttachmentBytes_NoSeamGivesStableReason(t *testing.T) {
	_, err := fetchAttachmentBytes(context.Background(), plainChannel{}, nil,
		conversation.Attachment{Name: "x.png"}, nil, 0)
	var fe *fetchError
	if !errors.As(err, &fe) {
		t.Fatalf("want *fetchError, got %T: %v", err, err)
	}
	if fe.Reason() != reasonNoFetchSeam {
		t.Errorf("reason = %q, want %q", fe.Reason(), reasonNoFetchSeam)
	}
}

// Over the cap is an error, never a truncation: half an image decodes to a
// confidently wrong reading, which is the failure class this design exists
// to remove.
func TestFetchAttachmentBytes_OverCapRefusedNotTruncated(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.jpg")
	if err := os.WriteFile(p, make([]byte, 2048), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fetchAttachmentBytes(context.Background(), plainChannel{}, nil,
		conversation.Attachment{Name: "big.jpg", ChannelRef: p}, []string{dir}, 1024); err == nil {
		t.Error("an over-cap file must be refused")
	}

	// Same via the artifact store...
	store := stubStore{data: map[string][]byte{"art-big": make([]byte, 2048)}}
	if _, err := fetchAttachmentBytes(context.Background(), plainChannel{}, store,
		conversation.Attachment{Name: "big.jpg", ArtifactID: "art-big"}, nil, 1024); err == nil {
		t.Error("an over-cap artifact must be refused")
	}

	// ...and via the optional fetcher, where the size is not known up front.
	if _, err := fetchAttachmentBytes(context.Background(),
		fetchingChannel{payload: make([]byte, 2048)}, nil,
		conversation.Attachment{Name: "big.jpg"}, nil, 1024); err == nil {
		t.Error("an over-cap stream must be refused, not truncated")
	}
}

func TestFetchAttachmentBytes_StoreErrorIsFetchFailed(t *testing.T) {
	_, err := fetchAttachmentBytes(context.Background(), plainChannel{},
		stubStore{err: errors.New("db down")}, conversation.Attachment{ArtifactID: "a"}, nil, 0)
	var fe *fetchError
	if !errors.As(err, &fe) || fe.Reason() != reasonFetchFailed {
		t.Errorf("want fetch_failed, got %v", err)
	}
}

func TestFetchAttachmentBytes_FetcherErrorIsFetchFailed(t *testing.T) {
	_, err := fetchAttachmentBytes(context.Background(),
		fetchingChannel{err: errors.New("upstream 500")}, nil,
		conversation.Attachment{Name: "x"}, nil, 0)
	var fe *fetchError
	if !errors.As(err, &fe) || fe.Reason() != reasonFetchFailed {
		t.Errorf("want fetch_failed, got %v", err)
	}
}

// A directory handed over as ChannelRef must not resolve — only regular
// files are readable payloads.
func TestFetchAttachmentBytes_DirectoryRefRefused(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := fetchAttachmentBytes(context.Background(), plainChannel{}, nil,
		conversation.Attachment{Name: "sub", ChannelRef: sub}, []string{dir}, 0); err == nil {
		t.Error("a directory must not be fetchable")
	}
}

func TestFetchError_ErrorAndUnwrap(t *testing.T) {
	inner := errors.New("boom")
	fe := &fetchError{reason: reasonFetchFailed, err: inner}
	if !strings.Contains(fe.Error(), reasonFetchFailed) || !strings.Contains(fe.Error(), "boom") {
		t.Errorf("Error() should carry reason and cause, got %q", fe.Error())
	}
	if !errors.Is(fe, inner) {
		t.Error("Unwrap must expose the cause")
	}
}

// An unreadable file (permissions) is fetch_failed, not no_fetch_seam: the
// seam existed, the read did not work — the caller reports different things.
func TestFetchAttachmentBytes_UnreadableFileIsFetchFailed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses file permissions")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "locked.jpg")
	if err := os.WriteFile(p, []byte("X"), 0o000); err != nil {
		t.Fatal(err)
	}
	_, err := fetchAttachmentBytes(context.Background(), plainChannel{}, nil,
		conversation.Attachment{Name: "locked.jpg", ChannelRef: p}, []string{dir}, 0)
	var fe *fetchError
	if !errors.As(err, &fe) || fe.Reason() != reasonFetchFailed {
		t.Errorf("want fetch_failed, got %v", err)
	}
}

// A root entry that is empty or unresolvable must be skipped, not treated as
// a match — an empty root would otherwise prefix-match everything.
func TestResolveAttachmentPath_IgnoresEmptyAndBadRoots(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.jpg")
	if err := os.WriteFile(p, []byte("X"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := resolveAttachmentPath(p, []string{"", "/nonexistent-root-xyz"}); ok {
		t.Error("empty and nonexistent roots must not admit a path")
	}
	if _, ok := resolveAttachmentPath(p, []string{"", dir}); !ok {
		t.Error("a valid root later in the list must still admit the path")
	}
}

// The root itself is a directory, never a payload.
func TestResolveAttachmentPath_RootItselfRejected(t *testing.T) {
	dir := t.TempDir()
	if _, ok := resolveAttachmentPath(dir, []string{dir}); ok {
		t.Error("the root directory must not resolve as a file")
	}
}

func TestReadAllCapped_UnboundedReadsEverything(t *testing.T) {
	got, err := readAllCapped(strings.NewReader("abcdef"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "abcdef" {
		t.Errorf("got %q", got)
	}
}

func TestReadCapped_MissingFile(t *testing.T) {
	if _, err := readCapped(filepath.Join(t.TempDir(), "nope.jpg"), 1024); err == nil {
		t.Error("a missing file must error")
	}
}
