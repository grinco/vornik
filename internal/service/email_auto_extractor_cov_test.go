package service

// Coverage-uplift sweep (2026-06-18). Complements
// email_auto_extractor_test.go (MIME dispatch + cache-hit + happy
// path) by pinning the remaining no-DB surface:
//   - dispatcherAutoExtractorAdapter: the dispatcher-side shim that
//     reuses the email extractor (request/response translation,
//     nil-deps guard, nil-summary passthrough).
//   - isGenericMIME: the catch-all true branches.
//   - decodeMetadata: empty + invalid-JSON best-effort decode.
//   - summarizeCached: title/author/section mapping with chunks=0.
//   - materializeSource: opener-error + nil-opener passthrough.
//
// No Postgres, podman, network, or LLM — the extractor registry is
// empty so AutoExtract short-circuits on unknown MIME without running
// a parser.

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/email"
	"vornik.io/vornik/internal/extractor"
	"vornik.io/vornik/internal/persistence"
)

func TestIsGenericMIME(t *testing.T) {
	for _, in := range []string{
		"application/octet-stream",
		"  Application/Octet-Stream  ", // case-fold + trim
		"binary/octet-stream",
		"application/x-binary",
	} {
		assert.True(t, isGenericMIME(in), "expected %q generic", in)
	}
	for _, in := range []string{
		"application/epub+zip",
		"application/pdf",
		"",
		"text/plain",
	} {
		assert.False(t, isGenericMIME(in), "expected %q non-generic", in)
	}
}

func TestDecodeMetadata(t *testing.T) {
	// Empty blob → zero-value metadata, no panic.
	assert.Equal(t, extractor.Metadata{}, decodeMetadata(nil))
	assert.Equal(t, extractor.Metadata{}, decodeMetadata([]byte{}))

	// Valid JSON populates the struct.
	meta := decodeMetadata([]byte(`{"title":"T","author":"A"}`))
	assert.Equal(t, "T", meta.Title)
	assert.Equal(t, "A", meta.Author)

	// Invalid JSON is best-effort: returns zero value, swallows the error.
	assert.Equal(t, extractor.Metadata{}, decodeMetadata([]byte(`{not json`)))
}

func TestSummarizeCached(t *testing.T) {
	doc := &persistence.ExtractedDocument{
		ID:           "extdoc_1",
		SectionCount: 4,
		MetadataBlob: []byte(`{"title":"Cached Book","author":"Author X"}`),
	}
	got := summarizeCached(doc)
	require.NotNil(t, got)
	assert.Equal(t, "extdoc_1", got.ExtractedDocumentID)
	assert.Equal(t, "Cached Book", got.Title)
	assert.Equal(t, "Author X", got.Author)
	assert.Equal(t, 4, got.SectionCount)
	// Cache hits never re-run the indexer.
	assert.Equal(t, 0, got.ChunksIngested)
}

// --- materializeSource ---

type emailCovErrOpener struct{ err error }

func (o emailCovErrOpener) Open(context.Context, string) (io.ReadCloser, error) {
	return nil, o.err
}

func TestMaterializeSource_NilOpenerPassesStoragePath(t *testing.T) {
	e := &emailAutoExtractor{} // opener nil
	path, cleanup, err := e.materializeSource(context.Background(), email.AutoExtractRequest{
		StoragePath: "/local/path.epub",
	})
	require.NoError(t, err)
	require.NotNil(t, cleanup)
	cleanup() // must be a safe no-op
	assert.Equal(t, "/local/path.epub", path)
}

func TestMaterializeSource_OpenerErrorPropagates(t *testing.T) {
	e := &emailAutoExtractor{opener: emailCovErrOpener{err: errors.New("artifact gone")}}
	_, cleanup, err := e.materializeSource(context.Background(), email.AutoExtractRequest{
		ArtifactID: "art-x",
		Name:       "f.epub",
	})
	require.Error(t, err)
	require.NotNil(t, cleanup)
	cleanup() // no-op cleanup on the error path
}

// --- dispatcherAutoExtractorAdapter ---

func TestDispatcherAutoExtractor_NilDepsErrors(t *testing.T) {
	var d *dispatcherAutoExtractorAdapter
	_, err := d.AutoExtract(context.Background(), dispatcher.AutoExtractRequest{})
	require.Error(t, err)

	// nil inner also surfaces a configured-error.
	d2 := &dispatcherAutoExtractorAdapter{}
	_, err = d2.AutoExtract(context.Background(), dispatcher.AutoExtractRequest{})
	require.Error(t, err)
}

func TestDispatcherAutoExtractor_UnknownMimeReturnsNilNil(t *testing.T) {
	// Empty registry → unknown MIME → inner returns (nil, nil); the
	// adapter must forward that as (nil, nil), NOT a zero-value struct.
	reg := extractor.NewRegistry()
	docs := &stubExtractedDocsRepo{}
	runner := &extractor.Runner{Repo: docs, BasePath: t.TempDir()}
	d := newDispatcherAutoExtractor(reg, runner, docs, nil, nil, zerolog.Nop())

	out, err := d.AutoExtract(context.Background(), dispatcher.AutoExtractRequest{
		ProjectID: "p", ArtifactID: "a", Name: "blob.bin",
		MimeType: "application/octet-stream",
	})
	require.NoError(t, err)
	assert.Nil(t, out)
}

func TestDispatcherAutoExtractor_TranslatesCacheHit(t *testing.T) {
	// Seed a cache hit so the inner extractor short-circuits without a
	// parser, then assert the dispatcher.AttachmentExtraction carries
	// the translated fields.
	reg := extractor.NewRegistry()
	docs := &stubExtractedDocsRepo{
		cached: &persistence.ExtractedDocument{
			ID:               "extdoc_cached",
			SourceArtifactID: "art-1",
			ExtractorName:    "epub",
			ExtractorVersion: "v-test",
			Status:           persistence.ExtractedDocumentStatusOK,
			SectionCount:     9,
			MetadataBlob:     []byte(`{"title":"Disp","author":"Y"}`),
		},
	}
	// Register a stub extractor whose name/version match the cached row
	// so the cache-hit branch fires.
	require.NoError(t, reg.Register(emailCovStubExtractor{}, "application/epub+zip"))
	runner := &extractor.Runner{Repo: docs, BasePath: t.TempDir()}
	d := newDispatcherAutoExtractor(reg, runner, docs, nil, nil, zerolog.Nop())

	out, err := d.AutoExtract(context.Background(), dispatcher.AutoExtractRequest{
		ProjectID:  "p",
		ArtifactID: "art-1",
		Name:       "book.epub",
		MimeType:   "application/epub+zip",
	})
	require.NoError(t, err)
	require.NotNil(t, out)
	assert.Equal(t, "extdoc_cached", out.ExtractedDocumentID)
	assert.Equal(t, "Disp", out.Title)
	assert.Equal(t, "Y", out.Author)
	assert.Equal(t, 9, out.SectionCount)
	assert.Equal(t, 0, out.ChunksIngested)
	// Cache hit must not re-Upsert.
	assert.Empty(t, docs.upserts)
}

// emailCovStubExtractor is a minimal extractor.Extractor whose Name +
// Version match the seeded cache row so the cache-hit branch fires
// without invoking Extract.
type emailCovStubExtractor struct{}

func (emailCovStubExtractor) Name() string    { return "epub" }
func (emailCovStubExtractor) Version() string { return "v-test" }
func (emailCovStubExtractor) Extract(context.Context, extractor.Source) (extractor.Result, error) {
	return extractor.Result{}, errors.New("should not be called on cache hit")
}
