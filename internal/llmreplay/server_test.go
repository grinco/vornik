package llmreplay

import (
	"encoding/json"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"vornik.io/vornik/internal/chat"
)

func recordingOf(t *testing.T, lines ...string) *Recording {
	t.Helper()
	rec, err := Load(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func line(t *testing.T, seq int, req, resp string, redactions int) string {
	t.Helper()
	_, hash, err := canonicalFromRaw(req)
	if err != nil {
		t.Fatal(err)
	}
	if redactions > 0 {
		hash = ""
	}
	return `{"seq":` + itoa(seq) + `,"iteration":` + itoa(seq) + `,"request_hash":"` + hash + `","redactions":` + itoa(redactions) + `,"request":` + req + `,"response":` + resp + `,"usage":{"prompt_tokens":10,"completion_tokens":4}}`
}

func itoa(n int) string { return strconv.Itoa(n) }

func canonicalFromRaw(raw string) ([]byte, string, error) {
	var req chat.ChatRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		return nil, "", err
	}
	return Canonical(req)
}

func post(t *testing.T, srv *httptest.Server, path, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp.StatusCode, b.String()
}

const (
	reqA  = `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[]}`
	reqB  = `{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"more"}],"tools":[]}`
	respA = `{"id":"a","choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`
	respB = `{"id":"b","choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`
)

func TestServer_ServesByHashOnBothPathsAndCountsUsage(t *testing.T) {
	rec := recordingOf(t, line(t, 1, reqA, respA, 0), line(t, 2, reqB, respB, 0))
	s := NewServer(rec)
	srv := httptest.NewServer(s)
	defer srv.Close()

	// A different spelling of reqA — other model, key order, whitespace — is the same exchange.
	code, body := post(t, srv, "/chat/completions", `{ "tools": [], "messages": [ {"content":"hi","role":"user"} ], "model": "other" }`)
	if code != http.StatusOK || !strings.Contains(body, `"id":"a"`) {
		t.Fatalf("first: %d %s", code, body)
	}
	code, body = post(t, srv, "/v1/chat/completions", reqB)
	if code != http.StatusOK || !strings.Contains(body, `"id":"b"`) {
		t.Fatalf("second on /v1: %d %s", code, body)
	}
	// The identical request twice serves the identical response twice.
	if code, _ = post(t, srv, "/chat/completions", reqA); code != http.StatusOK {
		t.Fatalf("repeat: %d", code)
	}
	st := s.Stats()
	if st.Served != 3 || st.Missed != 0 || st.PromptTokens != 30 || st.CompletionTokens != 12 {
		t.Errorf("stats %+v", st)
	}
	if code, _ = post(t, srv, "/other", reqA); code != http.StatusNotFound {
		t.Errorf("unknown path: %d", code)
	}
}

func TestServer_MissIsLoudAndNamesTheDivergence(t *testing.T) {
	rec := recordingOf(t, line(t, 1, reqA, respA, 0), line(t, 2, reqB, respB, 2))
	s := NewServer(rec)
	srv := httptest.NewServer(s)
	defer srv.Close()

	diverged := `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"DIFFERENT"}],"tools":[]}`
	code, body := post(t, srv, "/chat/completions", diverged)
	if code != http.StatusConflict {
		t.Fatalf("miss must be 409, got %d %s", code, body)
	}
	var mb missBody
	if err := json.Unmarshal([]byte(body), &mb); err != nil {
		t.Fatal(err)
	}
	if mb.Error.Type != "replay_miss" || mb.Error.ClosestSeq != 2 || mb.Error.DivergesAt != "messages[2]" || mb.Error.RecordingRedactions != 2 || mb.Error.ReceivedSHA == "" {
		t.Errorf("miss body %+v", mb.Error)
	}
	if s.Stats().Missed != 1 {
		t.Errorf("missed %d", s.Stats().Missed)
	}
}

func TestLoad_RefusesAnEditedRecording(t *testing.T) {
	edited := strings.Replace(line(t, 1, reqA, respA, 0), `"request_hash":"`, `"request_hash":"deadbeef`, 1)
	if _, err := Load(strings.NewReader(edited + "\n")); err == nil || !strings.Contains(err.Error(), "does not match the canonical form") {
		t.Errorf("an unredacted line whose hash disagrees must be refused: %v", err)
	}
}

// The design's "no upstream" is asserted, not promised: the package imports
// no HTTP client and holds no URL.
func TestServer_HasNoUpstream(t *testing.T) {
	fset := token.NewFileSet()
	entries, _ := os.ReadDir(".")
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", e.Name()), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if p == "net/http/httputil" || strings.HasPrefix(p, "vornik.io/vornik/internal/chat/") {
				t.Errorf("%s imports %s — the replay server must have no way to reach a provider", e.Name(), p)
			}
		}
		src, _ := os.ReadFile(e.Name())
		if strings.Contains(string(src), "http.Client") || strings.Contains(string(src), "http.Post") || strings.Contains(string(src), "http.Get") {
			t.Errorf("%s uses an HTTP client", e.Name())
		}
	}
}
