package mediahandles

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const publishTool = "mcp__pagedrop__pagedrop_publish_page"
const encodeTool = "mcp__scraper__encode_image"

func testStore() *Store {
	return New(Options{
		Sources:          []string{encodeTool},
		Sinks:            []Sink{{Tool: publishTool, HTMLArg: "html", ImagesArg: "images"}},
		MaxBytesPerImage: 1000,
		MaxImagesPerTask: 3,
	})
}

func encodeResult(dataURI string, bytes int64) string {
	b, _ := json.Marshal(map[string]any{
		"data_uri":     dataURI,
		"content_type": "image/jpeg",
		"width":        800,
		"height":       600,
		"bytes":        bytes,
	})
	return string(b)
}

func TestNewInertWhenUnconfigured(t *testing.T) {
	if New(Options{}) != nil {
		t.Fatal("expected nil (inert) store when no sources/sinks configured")
	}
}

func TestNilStoreIsSafe(t *testing.T) {
	var s *Store
	if got := s.ExtractSourceResult("t1", encodeTool, "x"); got != "x" {
		t.Fatalf("nil ExtractSourceResult = %q, want passthrough", got)
	}
	out, err := s.ExpandSinkArgs("t1", publishTool, `{"html":"x"}`)
	if err != nil || out != `{"html":"x"}` {
		t.Fatalf("nil ExpandSinkArgs = %q, %v; want passthrough", out, err)
	}
	s.Purge("t1") // must not panic
}

func TestExtractStashesAndReturnsHandle(t *testing.T) {
	s := testStore()
	in := encodeResult("data:image/jpeg;base64,AAAA", 500)
	out := s.ExtractSourceResult("t1", encodeTool, in)

	var hr handleResult
	if err := json.Unmarshal([]byte(out), &hr); err != nil {
		t.Fatalf("handle result not JSON: %v (%s)", err, out)
	}
	if hr.MediaHandle == "" {
		t.Fatal("expected a media_handle")
	}
	if strings.Contains(out, "AAAA") {
		t.Fatal("data URI must not leak into the agent-facing result")
	}
	if hr.Bytes != 500 || hr.ContentType != "image/jpeg" {
		t.Fatalf("metadata not preserved: %+v", hr)
	}
}

func TestExtractPassesThroughNonSourceAndNonImage(t *testing.T) {
	s := testStore()
	if got := s.ExtractSourceResult("t1", "mcp__scraper__web_fetch", `{"data_uri":"data:image/jpeg;base64,AAAA"}`); !strings.Contains(got, "data_uri") {
		t.Fatal("non-source tool result must pass through unchanged")
	}
	if got := s.ExtractSourceResult("t1", encodeTool, "MCP error: HTTP 404"); got != "MCP error: HTTP 404" {
		t.Fatalf("error string must pass through, got %q", got)
	}
	if got := s.ExtractSourceResult("t1", encodeTool, `{"other":"field"}`); !strings.Contains(got, "other") {
		t.Fatal("result without data_uri must pass through")
	}
}

func TestExtractRejectsOversizeAndOverCount(t *testing.T) {
	s := testStore()
	over := s.ExtractSourceResult("t1", encodeTool, encodeResult("data:image/jpeg;base64,AAAA", 2000))
	if !strings.Contains(over, "media_handle_error") || !strings.Contains(over, "over the") {
		t.Fatalf("expected over-cap error result, got %q", over)
	}
	// Fill to the count cap (3), then the 4th is rejected.
	for i := 0; i < 3; i++ {
		s.ExtractSourceResult("t2", encodeTool, encodeResult("data:image/jpeg;base64,AAAA", 100))
	}
	fourth := s.ExtractSourceResult("t2", encodeTool, encodeResult("data:image/jpeg;base64,AAAA", 100))
	if !strings.Contains(fourth, "too many images") {
		t.Fatalf("expected count-cap error, got %q", fourth)
	}
}

func TestExtractComputesBytesWhenAbsent(t *testing.T) {
	s := testStore()
	// 4 base64 chars (no padding) -> 3 decoded bytes, under the 1000 cap.
	out := s.ExtractSourceResult("t1", encodeTool, `{"data_uri":"data:image/png;base64,AAAA","content_type":"image/png"}`)
	if !strings.Contains(out, "media_handle") {
		t.Fatalf("expected stash with computed bytes, got %q", out)
	}
}

func TestExpandInjectsReferencedImages(t *testing.T) {
	s := testStore()
	h := mustHandle(t, s, "data:image/jpeg;base64,ZZZZ")

	args := `{"title":"P","html":"<h1>x</h1><img src=\"cid:` + h + `\">"}`
	out, err := s.ExpandSinkArgs("t1", publishTool, args)
	if err != nil {
		t.Fatalf("expand error: %v", err)
	}
	var parsed struct {
		Images []outImage `json:"images"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("expanded args not JSON: %v (%s)", err, out)
	}
	if len(parsed.Images) != 1 || parsed.Images[0].ID != h || parsed.Images[0].DataURI != "data:image/jpeg;base64,ZZZZ" {
		t.Fatalf("images not injected correctly: %+v", parsed.Images)
	}
}

func TestExpandOnlyIncludesReferencedHandles(t *testing.T) {
	s := testStore()
	h1 := mustHandle(t, s, "data:image/jpeg;base64,AAAA")
	_ = mustHandle(t, s, "data:image/jpeg;base64,BBBB") // stashed but not referenced

	args := `{"html":"<img src=\"cid:` + h1 + `\">"}`
	out, err := s.ExpandSinkArgs("t1", publishTool, args)
	if err != nil {
		t.Fatalf("expand error: %v", err)
	}
	var parsed struct {
		Images []outImage `json:"images"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("expanded args not JSON: %v", err)
	}
	if len(parsed.Images) != 1 {
		t.Fatalf("expected only the referenced image, got %d", len(parsed.Images))
	}
}

func TestExpandDanglingHandleErrors(t *testing.T) {
	s := testStore()
	args := `{"html":"<img src=\"cid:deadbeef\">"}`
	_, err := s.ExpandSinkArgs("t1", publishTool, args)
	if err == nil || !strings.Contains(err.Error(), "deadbeef") {
		t.Fatalf("expected dangling-handle error naming deadbeef, got %v", err)
	}
}

func TestExpandPassthroughs(t *testing.T) {
	s := testStore()
	// Non-sink tool.
	if out, _ := s.ExpandSinkArgs("t1", "mcp__scraper__web_fetch", `{"html":"<img src=\"cid:x\">"}`); strings.Contains(out, "images") {
		t.Fatal("non-sink tool must not be expanded")
	}
	// Sink but no cid refs.
	args := `{"html":"<p>no images</p>"}`
	if out, err := s.ExpandSinkArgs("t1", publishTool, args); err != nil || out != args {
		t.Fatalf("no-cid html should pass through unchanged, got %q %v", out, err)
	}
}

func TestPurgeDropsTaskMedia(t *testing.T) {
	s := testStore()
	h := mustHandle(t, s, "data:image/jpeg;base64,AAAA")
	s.Purge("t1")
	_, err := s.ExpandSinkArgs("t1", publishTool, `{"html":"<img src=\"cid:`+h+`\">"}`)
	if err == nil {
		t.Fatal("expected dangling error after purge")
	}
}

func TestIdleEviction(t *testing.T) {
	s := testStore()
	s.idleTTL = time.Minute
	base := time.Unix(1_000_000, 0)
	s.now = func() time.Time { return base }
	h := mustHandle(t, s, "data:image/jpeg;base64,AAAA")

	// Advance past the idle TTL, then a new stash on another task triggers eviction.
	s.now = func() time.Time { return base.Add(2 * time.Minute) }
	s.ExtractSourceResult("t2", encodeTool, encodeResult("data:image/jpeg;base64,AAAA", 100))

	_, err := s.ExpandSinkArgs("t1", publishTool, `{"html":"<img src=\"cid:`+h+`\">"}`)
	if err == nil {
		t.Fatal("expected t1 media to be evicted after idle TTL")
	}
}

// --- SanitizeSinkHTML gate (publish-time image integrity) ---

// Regression for task_20260705152851: the publisher shipped a PageDrop page with
// six external <img src="https://picsum.photos/..."> (random placeholder) tags,
// bypassing the encode_image→cid path and every prompt rule. The gate strips any
// <img> whose src is not a cid: handle so no external/placeholder image can ship.
func TestSanitizeStripsExternalPlaceholderImg(t *testing.T) {
	s := testStore()
	h := mustHandle(t, s, "data:image/jpeg;base64,ZZZZ")
	html := `<h1>Lakes</h1>` +
		`<img src="https://picsum.photos/500/300?random=1" alt="H">` +
		`<img src='cid:` + h + `'>`
	args, _ := json.Marshal(map[string]string{"title": "P", "html": html})

	out, stripped := s.SanitizeSinkHTML(publishTool, string(args))
	if len(stripped) != 1 || stripped[0] != "https://picsum.photos/500/300?random=1" {
		t.Fatalf("expected the picsum src stripped, got %v", stripped)
	}
	if strings.Contains(out, "picsum.photos") {
		t.Fatalf("picsum src must not survive sanitize: %s", out)
	}
	if !strings.Contains(out, "cid:"+h) {
		t.Fatalf("cid handle must be preserved: %s", out)
	}
}

func TestSanitizeStripsDataAndRelativeAndSrcless(t *testing.T) {
	s := testStore()
	html := `<img src="data:image/png;base64,AAAA"><img src="/local/x.jpg"><img alt="no src">`
	args, _ := json.Marshal(map[string]string{"html": html})
	out, stripped := s.SanitizeSinkHTML(publishTool, string(args))
	if len(stripped) != 3 {
		t.Fatalf("expected all 3 non-cid imgs stripped, got %v", stripped)
	}
	if strings.Contains(out, "<img") {
		t.Fatalf("no <img> should remain: %s", out)
	}
}

func TestSanitizeKeepsCidOnlyUnchanged(t *testing.T) {
	s := testStore()
	h := mustHandle(t, s, "data:image/jpeg;base64,ZZZZ")
	args := `{"html":"<img src=\"cid:` + h + `\">"}`
	out, stripped := s.SanitizeSinkHTML(publishTool, args)
	if stripped != nil {
		t.Fatalf("cid-only html must strip nothing, got %v", stripped)
	}
	if out != args {
		t.Fatalf("cid-only html must be returned unchanged, got %s", out)
	}
}

func TestSanitizePassthroughNonSinkAndNil(t *testing.T) {
	s := testStore()
	ext := `{"html":"<img src=\"https://picsum.photos/1\">"}`
	if out, stripped := s.SanitizeSinkHTML("mcp__scraper__web_fetch", ext); out != ext || stripped != nil {
		t.Fatalf("non-sink tool must pass through untouched, got %s %v", out, stripped)
	}
	var nilStore *Store
	if out, stripped := nilStore.SanitizeSinkHTML(publishTool, ext); out != ext || stripped != nil {
		t.Fatalf("nil store must pass through untouched, got %s %v", out, stripped)
	}
}

func mustHandle(t *testing.T, s *Store, dataURI string) string {
	t.Helper()
	const taskID = "t1"
	out := s.ExtractSourceResult(taskID, encodeTool, encodeResult(dataURI, 100))
	var hr handleResult
	if err := json.Unmarshal([]byte(out), &hr); err != nil || hr.MediaHandle == "" {
		t.Fatalf("failed to stash a handle: %v (%s)", err, out)
	}
	return hr.MediaHandle
}
