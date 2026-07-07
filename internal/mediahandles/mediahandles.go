// Package mediahandles keeps large MCP tool outputs (e.g. base64 image data
// URIs from encode_image) out of the agent's LLM context and output budget.
//
// A "source" tool's oversized result is stashed under a short handle in a
// per-task store, and the agent receives only the handle plus metadata. When
// the agent later calls a "sink" tool (e.g. PageDrop's publish), the stashed
// payloads referenced by <img src="cid:HANDLE"> in the sink's HTML argument
// are injected into the sink's images argument — so the base64 travels
// daemon→daemon and never crosses into the agent, which would otherwise
// truncate it (50 KB tool-result cap) and be unable to re-emit it (16 K output
// token cap).
//
// The package has no dependency on the mcp or config packages; the API layer
// constructs a Store from config and calls it around the MCP proxy Execute.
package mediahandles

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Sink describes a tool whose HTML argument carries cid: references that must
// be expanded into an images argument before the call is forwarded upstream.
type Sink struct {
	Tool      string // fully-qualified, e.g. mcp__pagedrop__pagedrop_publish_page
	HTMLArg   string // arg holding the HTML to scan, e.g. "html"
	ImagesArg string // arg to inject [{id,dataUri}] into, e.g. "images"
}

// Options configures a Store.
type Options struct {
	Sources          []string // fully-qualified source tool names
	Sinks            []Sink
	MaxBytesPerImage int64
	MaxImagesPerTask int
	IdleTTL          time.Duration // evict a task's media after this idle period
}

const (
	defaultMaxBytesPerImage = 2_000_000
	defaultMaxImagesPerTask = 20
	defaultIdleTTL          = 30 * time.Minute
)

// RE2 has no backreferences, so we accept either quote on each side. We only
// extract the handle (never rewrite the HTML), so quote-matching precision is
// irrelevant; the {1,64} bound matches PageDrop's accepted id shape.
var cidRefRE = regexp.MustCompile(`(?i)\bsrc\s*=\s*["']cid:([a-z0-9-]{1,64})["']`)

// imgTagRE matches a whole <img …> tag; imgSrcRE pulls its src value (either
// quote). Used by SanitizeSinkHTML to keep only cid: images in published HTML.
var (
	imgTagRE = regexp.MustCompile(`(?i)<img\b[^>]*>`)
	imgSrcRE = regexp.MustCompile(`(?i)\bsrc\s*=\s*(?:"([^"]*)"|'([^']*)')`)
)

type image struct {
	dataURI     string
	contentType string
	width       int
	height      int
	bytes       int64
}

type taskMedia struct {
	byHandle  map[string]image
	lastTouch time.Time
}

// Store is a per-task media stash. Construct with New. A nil *Store is inert
// (every method is a safe no-op / pass-through), so callers hold a nil when the
// feature is unconfigured.
type Store struct {
	sources   map[string]bool
	sinks     map[string]Sink
	maxBytes  int64
	maxImages int
	idleTTL   time.Duration

	mu    sync.Mutex
	tasks map[string]*taskMedia
	now   func() time.Time
}

// New returns a configured Store, or nil (inert) when neither sources nor sinks
// are configured.
func New(opts Options) *Store {
	if len(opts.Sources) == 0 && len(opts.Sinks) == 0 {
		return nil
	}
	s := &Store{
		sources:   map[string]bool{},
		sinks:     map[string]Sink{},
		maxBytes:  opts.MaxBytesPerImage,
		maxImages: opts.MaxImagesPerTask,
		idleTTL:   opts.IdleTTL,
		tasks:     map[string]*taskMedia{},
		now:       time.Now,
	}
	for _, src := range opts.Sources {
		s.sources[src] = true
	}
	for _, sink := range opts.Sinks {
		s.sinks[sink.Tool] = sink
	}
	if s.maxBytes <= 0 {
		s.maxBytes = defaultMaxBytesPerImage
	}
	if s.maxImages <= 0 {
		s.maxImages = defaultMaxImagesPerTask
	}
	if s.idleTTL <= 0 {
		s.idleTTL = defaultIdleTTL
	}
	return s
}

// Enabled reports whether the store is active (non-nil).
func (s *Store) Enabled() bool { return s != nil }

// sourceResult is the subset of a source tool's JSON result we consume.
type sourceResult struct {
	DataURI     string `json:"data_uri"`
	ContentType string `json:"content_type"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Bytes       int64  `json:"bytes"`
}

// handleResult is what the agent receives in place of a stashed source payload.
type handleResult struct {
	MediaHandle string `json:"media_handle"`
	ContentType string `json:"content_type,omitempty"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
	Bytes       int64  `json:"bytes,omitempty"`
	Note        string `json:"note,omitempty"`
}

// ExtractSourceResult stashes a source tool's data:image result under a fresh
// handle and returns a small handle result for the agent. When tool is not a
// configured source, resultText is not such a result, or the store is nil, it
// returns resultText unchanged. It never fails the HTTP call: an over-cap
// payload yields an error-shaped tool result the agent can read and react to.
func (s *Store) ExtractSourceResult(taskID, tool, resultText string) string {
	if s == nil || taskID == "" || !s.sources[tool] {
		return resultText
	}
	var sr sourceResult
	if err := json.Unmarshal([]byte(resultText), &sr); err != nil || !strings.HasPrefix(sr.DataURI, "data:image/") {
		// Not a stashable result: an "MCP error: ..." string, an error object,
		// or a tool output without a data_uri. Leave it for the agent.
		return resultText
	}
	bytes := decodedBytes(sr.DataURI)
	if bytes <= 0 {
		bytes = sr.Bytes
	}
	if bytes > s.maxBytes {
		return toolError(fmt.Sprintf("image is %d bytes, over the %d-byte limit; not embedded", bytes, s.maxBytes))
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictExpiredLocked()
	tm := s.tasks[taskID]
	if tm == nil {
		tm = &taskMedia{byHandle: map[string]image{}}
		s.tasks[taskID] = tm
	}
	if len(tm.byHandle) >= s.maxImages {
		return toolError(fmt.Sprintf("too many images for this task (max %d); not embedded", s.maxImages))
	}
	h := newHandle(tm.byHandle)
	tm.byHandle[h] = image{
		dataURI:     sr.DataURI,
		contentType: sr.ContentType,
		width:       sr.Width,
		height:      sr.Height,
		bytes:       bytes,
	}
	tm.lastTouch = s.now()

	out, _ := json.Marshal(handleResult{
		MediaHandle: h,
		ContentType: sr.ContentType,
		Width:       sr.Width,
		Height:      sr.Height,
		Bytes:       bytes,
		Note:        `embed with <img src="cid:` + h + `"> in the HTML you publish`,
	})
	return string(out)
}

type outImage struct {
	ID      string `json:"id"`
	DataURI string `json:"dataUri"`
}

// ExpandSinkArgs scans a sink tool's HTML argument for <img src="cid:HANDLE">
// references, gathers the matching stashed payloads for the task, and injects
// them as [{id,dataUri}] into the images argument. The HTML is left unchanged
// (the downstream tool resolves cid: refs). When tool is not a configured sink
// or the store is nil it returns argsJSON unchanged. Returns an error when the
// HTML references a handle not in the task store, so the call fails fast rather
// than publishing broken references.
func (s *Store) ExpandSinkArgs(taskID, tool, argsJSON string) (string, error) {
	if s == nil || taskID == "" {
		return argsJSON, nil
	}
	sink, ok := s.sinks[tool]
	if !ok {
		return argsJSON, nil
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return argsJSON, nil // not an object; nothing to expand
	}
	raw, ok := args[sink.HTMLArg]
	if !ok {
		return argsJSON, nil
	}
	var html string
	if err := json.Unmarshal(raw, &html); err != nil {
		return argsJSON, nil
	}

	matches := cidRefRE.FindAllStringSubmatch(html, -1)
	if len(matches) == 0 {
		return argsJSON, nil
	}
	seen := map[string]bool{}
	var order []string
	for _, m := range matches {
		h := m[1]
		if !seen[h] {
			seen[h] = true
			order = append(order, h)
		}
	}

	s.mu.Lock()
	tm := s.tasks[taskID]
	images := make([]outImage, 0, len(order))
	var missing []string
	for _, h := range order {
		var img image
		found := false
		if tm != nil {
			img, found = tm.byHandle[h]
		}
		if !found {
			missing = append(missing, h)
			continue
		}
		images = append(images, outImage{ID: h, DataURI: img.dataURI})
	}
	if tm != nil {
		tm.lastTouch = s.now()
	}
	s.mu.Unlock()

	if len(missing) > 0 {
		sort.Strings(missing)
		return argsJSON, fmt.Errorf(
			"HTML references unknown media handles: %s — encode_image them in this task first",
			strings.Join(missing, ", "),
		)
	}

	imgJSON, err := json.Marshal(images)
	if err != nil {
		return argsJSON, fmt.Errorf("media handles: marshal images: %w", err)
	}
	args[sink.ImagesArg] = imgJSON
	outJSON, err := json.Marshal(args)
	if err != nil {
		return argsJSON, fmt.Errorf("media handles: marshal args: %w", err)
	}
	return string(outJSON), nil
}

// SanitizeSinkHTML strips every <img> tag whose src is not a cid: media handle
// from a sink tool's HTML argument, returning the rewritten args and the list of
// removed srcs (nil when nothing was stripped). Only images routed through
// encode_image→cid are self-contained and subject-verified; a raw external URL
// (e.g. https://picsum.photos/…), a data: URI, a relative path, or a src-less tag
// both breaks PageDrop's single-file self-containment and lets random/placeholder
// images slip past the prompt rules. This is the deterministic backstop for that
// (task_20260705152851): prompts alone did not hold, so the gate guarantees no
// non-cid image can ship regardless of what the agent emits.
//
// Non-sink tools, a nil store, or HTML already containing only cid: images are
// returned unchanged. Stripping runs before ExpandSinkArgs; the caller logs the
// returned srcs for observability.
func (s *Store) SanitizeSinkHTML(tool, argsJSON string) (string, []string) {
	if s == nil {
		return argsJSON, nil
	}
	sink, ok := s.sinks[tool]
	if !ok {
		return argsJSON, nil
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return argsJSON, nil // not an object; nothing to sanitize
	}
	raw, ok := args[sink.HTMLArg]
	if !ok {
		return argsJSON, nil
	}
	var html string
	if err := json.Unmarshal(raw, &html); err != nil {
		return argsJSON, nil
	}

	var stripped []string
	clean := imgTagRE.ReplaceAllStringFunc(html, func(tag string) string {
		src := ""
		if m := imgSrcRE.FindStringSubmatch(tag); m != nil {
			src = m[1]
			if src == "" {
				src = m[2]
			}
		}
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(src)), "cid:") {
			return tag // sanctioned: an encode_image handle
		}
		if src == "" {
			src = "(no src)"
		}
		stripped = append(stripped, src)
		return "<!-- image removed: non-cid src blocked by media-handle gate -->"
	})
	if len(stripped) == 0 {
		return argsJSON, nil
	}

	cleanRaw, err := json.Marshal(clean)
	if err != nil {
		return argsJSON, nil
	}
	args[sink.HTMLArg] = cleanRaw
	out, err := json.Marshal(args)
	if err != nil {
		return argsJSON, nil
	}
	return string(out), stripped
}

// Purge drops all stashed media for a task. Safe to call on task terminal; a
// nil Store or unknown task is a no-op.
func (s *Store) Purge(taskID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.tasks, taskID)
	s.mu.Unlock()
}

// decodedBytes returns the decoded byte length of a base64 data: URI from its
// base64 body length.
func decodedBytes(dataURI string) int64 {
	i := strings.IndexByte(dataURI, ',')
	if i < 0 {
		return 0
	}
	b64 := dataURI[i+1:]
	n := int64(len(b64))
	if n == 0 {
		return 0
	}
	switch {
	case strings.HasSuffix(b64, "=="):
		return n*3/4 - 2
	case strings.HasSuffix(b64, "="):
		return n*3/4 - 1
	default:
		return n * 3 / 4
	}
}

func toolError(msg string) string {
	b, _ := json.Marshal(map[string]string{"error": "media_handle_error", "message": msg})
	return string(b)
}

// newHandle returns a fresh 16-hex-char handle not present in existing. The
// alphabet ([0-9a-f]) satisfies PageDrop's accepted id shape.
func newHandle(existing map[string]image) string {
	buf := make([]byte, 8)
	for {
		_, _ = rand.Read(buf)
		h := fmt.Sprintf("%x", buf)
		if _, exists := existing[h]; !exists {
			return h
		}
	}
}

func (s *Store) evictExpiredLocked() {
	cutoff := s.now().Add(-s.idleTTL)
	for id, tm := range s.tasks {
		if tm.lastTouch.Before(cutoff) {
			delete(s.tasks, id)
		}
	}
}
