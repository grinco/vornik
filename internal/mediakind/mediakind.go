// Package mediakind answers two questions that the dispatcher, the
// executor's staging path, and the model-capability surfaces all need to
// answer the same way:
//
//  1. What kind of media is this attachment? (Classify)
//  2. Can the model I am about to call actually perceive it? (Capabilities)
//
// Both used to be implicit. Classification was inferred ad hoc from file
// extensions in three places (the agent entrypoint's image detection, the
// extractor runner's MIME dispatch, and the executor's staging skip), and
// capability was a single naming heuristic inside the Ollama-compat proxy
// that only decided what the daemon *advertised* — nothing consulted it
// before actually sending pixels. The gap produced T-1df3: a photo whose
// pixels were dropped before the container and whose task was routed to a
// text-only model, with the operator told images are forwarded to
// vision-capable models automatically.
//
// The package is deliberately a leaf: no dependency on config, chat,
// persistence, or registry, so every layer can import it without a cycle.
// Callers pass their own declared-capability map in.
//
// see LLD § https://docs.vornik.io §4.1
package mediakind

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Kind is the broad media class of an attachment. The distinction that
// matters throughout this design is KindDocument versus everything else:
// a document's extraction can stand in for the original, and other kinds'
// extractions cannot (see ExtractionSufficient).
type Kind int

// Media classes. KindUnknown is an attachment we cannot classify, and is
// treated conservatively: the raw bytes stay available rather than being
// assumed redundant.
const (
	KindUnknown Kind = iota
	KindImage
	KindAudio
	KindVideo
	KindDocument
)

func (k Kind) String() string {
	switch k {
	case KindImage:
		return "image"
	case KindAudio:
		return "audio"
	case KindVideo:
		return "video"
	case KindDocument:
		return "document"
	default:
		return "unknown"
	}
}

// Modality is something a model can perceive.
type Modality int

// Perceivable modalities. ModalityText is universal — every chat model
// reads text — so Capabilities always includes it.
const (
	ModalityText Modality = iota
	ModalityVision
	ModalityAudio
)

func (m Modality) String() string {
	switch m {
	case ModalityVision:
		return "vision"
	case ModalityAudio:
		return "audio"
	default:
		return "text"
	}
}

// ParseModality maps an operator-written modality name onto a Modality.
// Unknown names are an error rather than a silent drop: a typo'd
// "visoin" that quietly resolved to text-only would look exactly like a
// deliberate text-only declaration, and the operator would have no way
// to tell why their model was handing over.
func ParseModality(s string) (Modality, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "text":
		return ModalityText, nil
	case "vision", "image":
		return ModalityVision, nil
	case "audio":
		return ModalityAudio, nil
	default:
		return ModalityText, fmt.Errorf("unknown modality %q (want text, vision, or audio)", s)
	}
}

// ParseModalities maps a list of names, failing on the first bad entry.
func ParseModalities(names []string) ([]Modality, error) {
	out := make([]Modality, 0, len(names))
	for _, n := range names {
		m, err := ParseModality(n)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, nil
}

// Set is a set of modalities a model can perceive.
type Set uint8

// NewSet builds a Set from modalities.
func NewSet(ms ...Modality) Set {
	var s Set
	for _, m := range ms {
		s |= 1 << uint(m)
	}
	return s
}

// Can reports whether the set includes a modality.
func (s Set) Can(m Modality) bool { return s&(1<<uint(m)) != 0 }

// String renders the set in declaration order for logs and metrics.
func (s Set) String() string {
	var parts []string
	for _, m := range []Modality{ModalityText, ModalityVision, ModalityAudio} {
		if s.Can(m) {
			parts = append(parts, m.String())
		}
	}
	return strings.Join(parts, ",")
}

// ExtractionSufficient reports whether an extractor's output can stand in
// for the original file.
//
// True only for documents. An EPUB's extracted sections carry the book;
// an image's OCR-plus-EXIF extraction does not carry the picture, a
// transcript does not carry what a video looked like, and an
// unclassifiable file's extraction cannot be assumed to carry anything.
//
// This is the predicate two separate bugs got wrong by treating "an
// extraction exists" as "the original is no longer needed":
//
//   - the executor dropped raw media from container staging (T-1df3), and
//   - the dispatcher's attachment trailer told the lead the work was
//     already done.
//
// Both consult this function so they cannot disagree.
func ExtractionSufficient(k Kind) bool { return k == KindDocument }

// genericMIMEs carry no classification signal, so Classify falls through
// to the filename extension for these.
var genericMIMEs = map[string]bool{
	"":                         true,
	"application/octet-stream": true,
	"binary/octet-stream":      true,
	"application/binary":       true,
}

// documentMIMEs are the non-text/* MIME types that classify as documents.
// Mirrors the set the chat layer's DocumentURL block accepts plus the
// formats the extractor pipeline handles.
var documentMIMEs = map[string]bool{
	"application/pdf":          true,
	"application/epub+zip":     true,
	"application/msword":       true,
	"application/rtf":          true,
	"application/vnd.ms-excel": true,
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document": true,
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":       true,
	"application/vnd.oasis.opendocument.text":                                 true,
}

// extKinds maps a lowercase extension (no dot) to its Kind. Broader than
// what any single consumer accepts on purpose: Classify says what a file
// *is*, and each caller applies its own narrower policy on top — the
// dispatcher's inline-image MIME allowlist accepts four types, while a
// HEIC still classifies as an image so it hands over rather than being
// mistaken for a document.
var extKinds = map[string]Kind{
	// Images
	"jpg": KindImage, "jpeg": KindImage, "png": KindImage, "gif": KindImage,
	"webp": KindImage, "bmp": KindImage, "tif": KindImage, "tiff": KindImage,
	"heic": KindImage, "heif": KindImage, "avif": KindImage, "svg": KindImage,
	// Audio
	"mp3": KindAudio, "wav": KindAudio, "ogg": KindAudio, "oga": KindAudio,
	"opus": KindAudio, "m4a": KindAudio, "flac": KindAudio, "aac": KindAudio,
	"wma": KindAudio, "amr": KindAudio,
	// Video
	"mp4": KindVideo, "m4v": KindVideo, "mov": KindVideo, "mkv": KindVideo,
	"webm": KindVideo, "avi": KindVideo, "mpeg": KindVideo, "mpg": KindVideo,
	"wmv": KindVideo, "flv": KindVideo,
	// Documents
	"pdf": KindDocument, "epub": KindDocument, "txt": KindDocument,
	"md": KindDocument, "markdown": KindDocument, "html": KindDocument,
	"htm": KindDocument, "csv": KindDocument, "tsv": KindDocument,
	"doc": KindDocument, "docx": KindDocument, "xls": KindDocument,
	"xlsx": KindDocument, "rtf": KindDocument, "odt": KindDocument,
	"json": KindDocument, "xml": KindDocument, "yaml": KindDocument,
	"yml": KindDocument, "log": KindDocument,
}

// Classify determines an attachment's Kind from its declared MIME type,
// falling back to the filename extension when the MIME type is absent or
// generic.
//
// MIME wins on disagreement: a channel that declares a concrete type read
// it from the wire envelope, whereas the filename is whatever the sender
// typed. A .jpg that arrives as audio/mpeg is treated as audio, which is
// the safe direction — it will not be handed to a vision model as pixels.
func Classify(name, mimeType string) Kind {
	mime := strings.ToLower(strings.TrimSpace(mimeType))
	// Strip any parameters ("text/plain; charset=utf-8").
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}

	if !genericMIMEs[mime] {
		switch {
		case strings.HasPrefix(mime, "image/"):
			return KindImage
		case strings.HasPrefix(mime, "audio/"):
			return KindAudio
		case strings.HasPrefix(mime, "video/"):
			return KindVideo
		case strings.HasPrefix(mime, "text/"):
			return KindDocument
		case documentMIMEs[mime]:
			return KindDocument
		}
		// A concrete but unrecognised MIME type (application/x-wat)
		// still gets an extension pass — the extension may be
		// recognisable even when the type isn't.
	}

	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	if k, ok := extKinds[ext]; ok {
		return k
	}
	return KindUnknown
}

// visionPatterns are model-id substrings that indicate a multimodal
// model. Lifted from the Ollama-compat proxy's isVisionModel, which used
// them only to advertise capabilities on /api/show; the routing gate now
// shares the list so what the daemon advertises and what it acts on
// cannot drift.
//
// Version-sensitive where a family changed: Gemma gained vision at 3, so
// "gemma-2" must not match while "gemma-3" and "gemma4" must — the
// deployment's own vision role runs google.gemma-3-27b-it with a
// gemma4:31b fallback, and a family-wide "gemma" pattern would wrongly
// call Gemma 2 sighted.
var visionPatterns = []string{
	"vision", "-vl", "llava", "pixtral", "minicpm-v",
	"gemini", "claude", "gpt-4o", "gpt-5",
	"gemma-3", "gemma3", "gemma-4", "gemma4",
}

// Capabilities reports what a model can perceive.
//
// Resolution order:
//
//  1. an explicit operator declaration in declared (case-insensitive),
//  2. built-in model-id patterns,
//  3. text-only.
//
// Step 3 is the fail-closed default, and the reason the whole function
// exists rather than a bare pattern match: an unrecognised model is
// assumed blind, so the caller hands over to a specialist instead of
// sending pixels to a model that will ignore them and answer anyway.
// Every model can read text, so ModalityText is always present.
//
// declared may be nil.
func Capabilities(modelID string, declared map[string][]Modality) Set {
	id := strings.ToLower(strings.TrimSpace(modelID))

	// Explicit declaration wins outright — including a declaration that
	// a pattern-matching model is text-only, which is how an operator
	// records "this provider's path for this model does not actually
	// accept images".
	for k, ms := range declared {
		if strings.ToLower(strings.TrimSpace(k)) == id {
			return NewSet(ms...) | NewSet(ModalityText)
		}
	}

	s := NewSet(ModalityText)
	if id == "" {
		return s
	}
	for _, p := range visionPatterns {
		if strings.Contains(id, p) {
			s |= NewSet(ModalityVision)
			break
		}
	}
	return s
}
