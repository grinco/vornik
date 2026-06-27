// Package extractor — binary-attachment → text+structure conversion.
//
// The document-extraction pipeline
// (https://docs.vornik.io) lifts the
// "read this EPUB / PDF / audio file" responsibility OUT of the LLM
// context. Each MIME type binds to an Extractor implementation that
// emits structured text + metadata; the LLM only ever sees the
// structured output via the document_* tool surface.
//
// Phase 0 ships the interface + Registry only. Phase 1 wires the
// EPUB implementation. Future phases add PDF / audio / video /
// images. Adding a new format is: implement Extractor, register it
// against one or more MIME types — no other code change.
package extractor

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
)

// ErrNoExtractor is returned by Registry.For when no registered
// extractor claims the given MIME type. Callers route this to a
// user-visible "no extractor for application/x-foo" error rather
// than retrying — extraction can't succeed without a binding.
var ErrNoExtractor = errors.New("extractor: no extractor registered for MIME type")

// Metadata is the operator-visible header for an extracted document.
// Fields are deliberately superset-style so each extractor populates
// what it can; consumers (the indexer, the document_get_metadata
// tool, the operator UI) tolerate missing fields gracefully.
//
// Title / Author / PublicationDate are the load-bearing trio for
// memory citations. ISBN is book-specific; Duration is audio/video-
// specific; PageCount is PDF-specific. Language is ISO 639-1 when
// the extractor can sniff it.
type Metadata struct {
	Title           string `json:"title,omitempty"`
	Author          string `json:"author,omitempty"`
	Publisher       string `json:"publisher,omitempty"`
	PublicationDate string `json:"publication_date,omitempty"`
	ISBN            string `json:"isbn,omitempty"`
	DurationSeconds int    `json:"duration_seconds,omitempty"`
	PageCount       int    `json:"page_count,omitempty"`
	Language        string `json:"language,omitempty"`
	// Extra carries extractor-specific metadata that doesn't fit the
	// well-known fields above. Free-form JSON; the document_get_metadata
	// tool surfaces this verbatim for the LLM to interpret.
	Extra map[string]string `json:"extra,omitempty"`
}

// OutlineEntry is one node in the table-of-contents tree, flattened
// to a list with explicit Depth (depth=0 == top-level chapter).
// Order in the slice is reading order.
type OutlineEntry struct {
	// SectionID is the stable identifier for this section. Used as
	// the filename suffix in <storage>/sections/<section_id>.md and
	// as the key for document_read_section. Must be filesystem-safe
	// — no slashes, no whitespace, lowercase alphanumeric + hyphen.
	SectionID string `json:"section_id"`

	// Title is the operator-visible section heading.
	Title string `json:"title"`

	// Depth is the nesting level. 0 = top-level chapter, 1 = section
	// within a chapter, etc. The extractor decides where to draw
	// the structural line.
	Depth int `json:"depth"`

	// PageStart / TimestampStartSec are extractor-specific anchors.
	// PageStart is 1-indexed; TimestampStartSec is seconds from
	// start of media. Both are optional.
	PageStart         int `json:"page_start,omitempty"`
	TimestampStartSec int `json:"timestamp_start_sec,omitempty"`

	// TextBytes is the size of the section's extracted markdown.
	// The indexer uses this to decide whether to embed the section
	// as one chunk or split.
	TextBytes int `json:"text_bytes"`
}

// Section is one extracted unit — content matches the file on disk
// at <storage>/sections/<SectionID>.md. The extractor returns
// sections in reading order alongside the outline that references
// them by ID.
type Section struct {
	SectionID string
	Title     string
	// Content is the markdown body. The Extractor.Extract caller
	// writes this to disk; consumers read from disk for everything
	// except the immediate post-extraction indexer pass.
	Content string
}

// Result is the structured output of an Extractor.Extract call.
// Sections + outline are paired: every OutlineEntry.SectionID must
// match a Section.SectionID. Metadata is freestanding.
type Result struct {
	Metadata Metadata
	Outline  []OutlineEntry
	Sections []Section
}

// TotalTextBytes returns the sum of all section content lengths.
// Used by Runner to populate ExtractedDocument.TotalTextBytes
// without re-iterating in the caller.
func (r *Result) TotalTextBytes() int64 {
	var total int64
	for _, s := range r.Sections {
		total += int64(len(s.Content))
	}
	return total
}

// Source is the input handed to an Extractor.Extract call.
// FilePath is the host path of the binary artifact; MimeType is the
// declared MIME (extractors may verify via libmagic but the
// registry's dispatch already matched on this).
type Source struct {
	FilePath string
	MimeType string
	// OriginalName preserves the operator-visible filename so
	// extractors can use it as a metadata fallback (e.g. EPUB with
	// no <dc:title>). Optional.
	OriginalName string
}

// Extractor is the contract every plugin implements. The interface
// is intentionally minimal — Name / Version / Extract — so adding
// a new format is straightforward and the registry stays simple.
//
// Implementations must be concurrency-safe: Registry.For may return
// the same Extractor to multiple goroutines simultaneously. Holding
// no per-call state in struct fields is the easiest way to satisfy
// this.
type Extractor interface {
	// Name is the unique extractor identifier ("vornik-extract-epub",
	// "vornik-extract-pdf", etc.). Persisted on every
	// extracted_documents row so re-extraction with a different
	// extractor name produces a new row.
	Name() string

	// Version is the extractor's semver-ish version string. Bumped
	// when the extraction logic changes in a way that justifies
	// re-running over historical artifacts. Stored on every row
	// alongside Name.
	Version() string

	// Extract reads the binary at src.FilePath and returns the
	// structured output. The caller is responsible for persisting
	// the Result to disk and the extracted_documents row.
	//
	// Implementations should respect ctx cancellation (large files,
	// OCR loops). On error the Result may be partial — callers
	// should still inspect it for diagnostic info (e.g. metadata
	// extracted before the section-parse step crashed).
	Extract(ctx context.Context, src Source) (Result, error)
}

// Registry maps MIME types to extractor implementations. Lookup is
// case-insensitive on the MIME type and tolerant of charset
// parameters (`application/pdf; charset=utf-8` matches
// `application/pdf`).
//
// Wildcards: an extractor can register against `audio/*` to claim
// every audio type. A specific type (`audio/mpeg`) wins over a
// wildcard. Wildcards must match the `<type>/*` shape — no
// double-wildcards.
type Registry struct {
	exact    map[string]Extractor // "application/pdf" -> Extractor
	wildcard map[string]Extractor // "audio" -> Extractor (for audio/*)
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		exact:    make(map[string]Extractor),
		wildcard: make(map[string]Extractor),
	}
}

// Register binds an Extractor to one or more MIME types. Re-registering
// a MIME type overwrites the previous binding — last-call-wins so
// operators can override the bundled defaults via configs/extractors.yaml.
// Returns an error when mimeTypes is empty or contains malformed
// wildcards.
func (r *Registry) Register(ext Extractor, mimeTypes ...string) error {
	if ext == nil {
		return errors.New("extractor is nil")
	}
	if len(mimeTypes) == 0 {
		return errors.New("at least one MIME type is required")
	}
	for _, mt := range mimeTypes {
		canon := canonicalMime(mt)
		if canon == "" {
			return fmt.Errorf("invalid MIME type %q", mt)
		}
		major, sub, ok := splitMime(canon)
		if !ok || major == "" || sub == "" {
			return fmt.Errorf("invalid MIME type %q (must be \"<type>/<subtype>\")", mt)
		}
		if sub == "*" {
			r.wildcard[major] = ext
			continue
		}
		// Forbid wildcards anywhere except the entire subtype.
		// "audio/**" or "audio/*/bar" would slip through the
		// HasSuffix("/*") check; explicit reject keeps the
		// registry's matching contract unambiguous.
		if strings.Contains(sub, "*") {
			return fmt.Errorf("invalid MIME type %q (wildcards only allowed as a bare subtype \"<type>/*\")", mt)
		}
		r.exact[canon] = ext
	}
	return nil
}

// splitMime splits a canonical "type/subtype" into its two halves.
// Returns ok=false when the input doesn't contain exactly one '/'.
func splitMime(canon string) (major, sub string, ok bool) {
	i := strings.IndexByte(canon, '/')
	if i < 0 {
		return "", "", false
	}
	if strings.IndexByte(canon[i+1:], '/') >= 0 {
		return "", "", false // more than one '/'
	}
	return canon[:i], canon[i+1:], true
}

// For returns the Extractor claiming this MIME type. Exact matches
// win over wildcards. Returns ErrNoExtractor when nothing matches —
// callers map that to a "no extractor for <mime>" surfaced error.
func (r *Registry) For(mimeType string) (Extractor, error) {
	canon := canonicalMime(mimeType)
	if canon == "" {
		return nil, fmt.Errorf("%w: %q", ErrNoExtractor, mimeType)
	}
	if ext, ok := r.exact[canon]; ok {
		return ext, nil
	}
	// Try wildcard against the major type.
	if i := strings.IndexByte(canon, '/'); i > 0 {
		major := canon[:i]
		if ext, ok := r.wildcard[major]; ok {
			return ext, nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrNoExtractor, mimeType)
}

// SupportedMimeTypes returns every MIME pattern the registry will
// match against, sorted for deterministic output. Used by the
// daemon's preflight check (`vornikctl doctor`) and by the
// auto-ingest allowlist validator. Wildcards are returned in their
// stored form (`audio/*`).
func (r *Registry) SupportedMimeTypes() []string {
	out := make([]string, 0, len(r.exact)+len(r.wildcard))
	for mt := range r.exact {
		out = append(out, mt)
	}
	for major := range r.wildcard {
		out = append(out, major+"/*")
	}
	sort.Strings(out)
	return out
}

// MimeFromFilename is a fallback for callers that don't have a
// declared Content-Type — uses the filename extension to guess.
// Returns "" when nothing matches; the caller decides whether to
// reject the artifact or pass it through libmagic before giving up.
//
// The map is deliberately small — it covers the format set the
// extractor registry actually supports. Callers wanting "every MIME
// type ever" should reach for a real magic-bytes library.
func MimeFromFilename(filename string) string {
	ext := strings.ToLower(path.Ext(filename))
	switch ext {
	case ".epub":
		return "application/epub+zip"
	case ".pdf":
		return "application/pdf"
	case ".txt":
		return "text/plain"
	case ".md", ".markdown":
		return "text/markdown"
	case ".html", ".htm":
		return "text/html"
	case ".docx":
		return "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	case ".mp3":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	case ".m4a":
		return "audio/mp4"
	case ".ogg", ".oga":
		return "audio/ogg"
	case ".flac":
		return "audio/flac"
	case ".mp4":
		return "video/mp4"
	case ".mkv":
		return "video/x-matroska"
	case ".webm":
		return "video/webm"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	}
	return ""
}

// canonicalMime strips parameters ("; charset=utf-8") and
// lowercases the remaining type/subtype. Returns "" for input that
// doesn't contain a slash (which would be ambiguous with a
// wildcard major-type lookup).
func canonicalMime(mt string) string {
	mt = strings.TrimSpace(strings.ToLower(mt))
	if mt == "" {
		return ""
	}
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	if mt == "" || !strings.Contains(mt, "/") {
		return ""
	}
	return mt
}
