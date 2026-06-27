// Document-extraction pipeline wiring. See
// https://docs.vornik.io
//
// Builds:
//   - extractor.Registry with the bundled-default extractors registered
//   - extractor.Runner with the artifact-store base path + DB repo
//
// Both surfaces are nil-safe at the consumer (api.Server). Construction
// happens lazily on first access so the extractor list stays accurate
// after future config-reload work — for the slice today, lazy = init
// once on first lookup, no reload story.
package service

import (
	"sync"

	"vornik.io/vornik/internal/extractor"
	"vornik.io/vornik/internal/extractor/audio"
	"vornik.io/vornik/internal/extractor/epub"
	htmlx "vornik.io/vornik/internal/extractor/html"
	imagex "vornik.io/vornik/internal/extractor/image"
	"vornik.io/vornik/internal/extractor/pdf"
	"vornik.io/vornik/internal/extractor/textfile"
)

// extractorPipeline is a thin lazy-init holder. The fields are
// populated on first call to ExtractorRegistry / ExtractorRunner;
// initOnce ensures the construction runs exactly once even under
// concurrent api.Server boot + UI boot races.
type extractorPipeline struct {
	initOnce sync.Once
	registry *extractor.Registry
	runner   *extractor.Runner
}

// ExtractorRegistry returns the daemon's MIME-keyed extractor
// registry, constructing it on first call. Returns nil only when
// the artifact-store base path isn't configured — extraction
// requires somewhere to write extracted sections.
func (c *Container) ExtractorRegistry() *extractor.Registry {
	c.extractorPipeline.initOnce.Do(c.initExtractorPipeline)
	return c.extractorPipeline.registry
}

// ExtractorRunner returns the Runner shared by every extraction
// trigger (HTTP endpoint today; future workflow steps + email
// channel auto-trigger). Nil when ExtractorRegistry is nil.
func (c *Container) ExtractorRunner() *extractor.Runner {
	c.extractorPipeline.initOnce.Do(c.initExtractorPipeline)
	return c.extractorPipeline.runner
}

func (c *Container) initExtractorPipeline() {
	if c == nil || c.Config == nil {
		return
	}
	basePath := c.Config.Storage.ArtifactsPath
	if basePath == "" {
		c.Logger.Warn().Msg("extractor: ArtifactsPath unset — extraction disabled")
		return
	}
	if c.repos == nil || c.repos.ExtractedDocuments == nil {
		c.Logger.Warn().Msg("extractor: ExtractedDocuments repo unwired — extraction disabled")
		return
	}

	reg := extractor.NewRegistry()
	// IANA registers application/epub+zip; some senders (Gmail
	// included) strip the +zip and ship application/epub. Register
	// both so the email channel's verbatim Content-Type
	// pass-through lands on the right extractor regardless.
	if err := reg.Register(epub.New(), "application/epub+zip", "application/epub"); err != nil {
		c.Logger.Error().Err(err).Msg("extractor: failed to register EPUB extractor")
		return
	}
	// PDF — shells out to poppler's pdftotext. We register
	// unconditionally; if poppler is missing the binary check
	// happens at Extract time and surfaces "install poppler-utils"
	// to the operator. Pre-flight check (Phase 7) will warn at
	// boot.
	if err := reg.Register(pdf.New(), "application/pdf"); err != nil {
		c.Logger.Error().Err(err).Msg("extractor: failed to register PDF extractor")
		return
	}
	// HTML — pure Go. Some senders advertise application/xhtml+xml
	// for the same content shape; cover both.
	if err := reg.Register(htmlx.New(), "text/html", "application/xhtml+xml"); err != nil {
		c.Logger.Error().Err(err).Msg("extractor: failed to register HTML extractor")
		return
	}
	// Plain text + markdown — pure Go. Markdown variants in the
	// wild use either text/markdown or text/x-markdown; register
	// both bare-name + text/* style. text/plain is the most common
	// inbound emailed-notes shape.
	if err := reg.Register(textfile.New(),
		"text/plain", "text/markdown", "text/x-markdown"); err != nil {
		c.Logger.Error().Err(err).Msg("extractor: failed to register text extractor")
		return
	}
	// Audio — shells out to whisper (Python openai-whisper, on PATH).
	// Register against audio/* so any inbound audio MIME flows to
	// the same extractor; whisper accepts mp3 / wav / m4a / flac
	// natively. Like PDF: registration is unconditional; the
	// binary check happens at Extract time with a helpful "install
	// openai-whisper" message when missing.
	if err := reg.Register(audio.New(), "audio/*"); err != nil {
		c.Logger.Error().Err(err).Msg("extractor: failed to register audio extractor")
		return
	}
	// Images — pure-Go header decode for dimensions + optional
	// tesseract OCR (graceful degradation when missing on the
	// daemon host). image/* covers jpeg/png/gif/webp at the
	// dispatch layer; the stdlib decoder handles png/jpeg/gif
	// natively. Other image variants degrade to a metadata-only
	// section via the same error path.
	if err := reg.Register(imagex.New(), "image/*"); err != nil {
		c.Logger.Error().Err(err).Msg("extractor: failed to register image extractor")
		return
	}
	c.Logger.Info().
		Strs("mime_types", reg.SupportedMimeTypes()).
		Msg("extractor: registry constructed")

	c.extractorPipeline.registry = reg
	c.extractorPipeline.runner = &extractor.Runner{
		Repo:     c.repos.ExtractedDocuments,
		BasePath: basePath,
		// nil until wireComponentMetrics runs at boot; the Runner is
		// nil-safe so an early lazy-init simply doesn't emit.
		Metrics: c.extractorMetrics,
	}
}
