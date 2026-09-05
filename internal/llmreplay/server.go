package llmreplay

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"

	"vornik.io/vornik/internal/chat"
)

// Entry is one line of a recording file (the API's ?format=jsonl export).
type Entry struct {
	Seq         int             `json:"seq"`
	Iteration   *int            `json:"iteration"`
	RequestHash string          `json:"request_hash"`
	Redactions  int             `json:"redactions"`
	Request     json.RawMessage `json:"request"`
	Response    json.RawMessage `json:"response"`
	Usage       struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// Recording is a loaded file, keyed by request hash. Identical requests
// under two seqs (a retried call) share a key and serve the same response
// twice, which is what happened.
type Recording struct {
	Entries []Entry
	byHash  map[string]*Entry
}

// Load reads a JSONL recording. Each line's request is re-canonicalised so
// the key is the same function's output the server will compute for the
// live request; a line whose stored hash disagrees with that is refused —
// the recording was edited or made by a different canonical form.
func Load(r io.Reader) (*Recording, error) {
	rec := &Recording{byHash: map[string]*Entry{}}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1024*1024), 64*1024*1024)
	line := 0
	for sc.Scan() {
		line++
		if len(sc.Bytes()) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("llmreplay: line %d: %w", line, err)
		}
		var req chat.ChatRequest
		if err := json.Unmarshal(e.Request, &req); err != nil {
			return nil, fmt.Errorf("llmreplay: line %d: request: %w", line, err)
		}
		_, hash, err := Canonical(req)
		if err != nil {
			return nil, fmt.Errorf("llmreplay: line %d: %w", line, err)
		}
		if e.Redactions == 0 && e.RequestHash != "" && e.RequestHash != hash {
			return nil, fmt.Errorf("llmreplay: line %d: stored hash %s does not match the canonical form (%s) — edited recording, or a different canonical function", line, e.RequestHash, hash)
		}
		if e.RequestHash == "" || e.Redactions > 0 {
			// A redacted body hashes as stored (design §4); key on that so a
			// live request that happens to match the placeholder text still
			// finds it, and a real secret misses loudly.
			e.RequestHash = hash
		}
		rec.Entries = append(rec.Entries, e)
		if _, dup := rec.byHash[e.RequestHash]; !dup {
			rec.byHash[e.RequestHash] = &rec.Entries[len(rec.Entries)-1]
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	// The slice may have reallocated while appending; rebuild the index.
	rec.byHash = map[string]*Entry{}
	for i := range rec.Entries {
		if _, dup := rec.byHash[rec.Entries[i].RequestHash]; !dup {
			rec.byHash[rec.Entries[i].RequestHash] = &rec.Entries[i]
		}
	}
	return rec, nil
}

// Stats is the replay's token meter and miss counter.
type Stats struct {
	Served, Missed                 int
	PromptTokens, CompletionTokens int
}

// Server is an OpenAI-compatible /chat/completions fed from a Recording. It
// has NO upstream: there is no client, no fallthrough, nothing to
// misconfigure into reaching a provider (design §5.2).
type Server struct {
	rec   *Recording
	mu    sync.Mutex
	stats Stats
}

// NewServer wraps a recording.
func NewServer(rec *Recording) *Server { return &Server{rec: rec} }

// Stats returns a copy of the counters.
func (s *Server) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

// missBody is the 409 a replay miss returns (design §5.3). closest_seq and
// diverges_at are advisory; the two hashes are the facts.
type missBody struct {
	Error struct {
		Type                string `json:"type"`
		Message             string `json:"message"`
		ClosestSeq          int    `json:"closest_seq,omitempty"`
		ClosestIteration    *int   `json:"closest_iteration,omitempty"`
		DivergesAt          string `json:"diverges_at,omitempty"`
		RecordedSHA         string `json:"recorded_sha,omitempty"`
		ReceivedSHA         string `json:"received_sha"`
		RecordingRedactions int    `json:"recording_redactions"`
	} `json:"error"`
}

// ServeHTTP serves POST /chat/completions and /v1/chat/completions — the two
// paths the container's vornik_resolve_url can produce.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || (r.URL.Path != "/chat/completions" && r.URL.Path != "/v1/chat/completions") {
		http.Error(w, "replay server: POST /chat/completions only", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req chat.ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "replay server: request is not valid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	_, hash, err := Canonical(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.rec.byHash[hash]; ok {
		s.stats.Served++
		s.stats.PromptTokens += e.Usage.PromptTokens
		s.stats.CompletionTokens += e.Usage.CompletionTokens
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(e.Response)
		return
	}
	s.stats.Missed++
	var mb missBody
	mb.Error.Type = "replay_miss"
	mb.Error.Message = "no recorded exchange matches this request"
	mb.Error.ReceivedSHA = hash
	if closest := s.rec.closest(req); closest != nil {
		mb.Error.ClosestSeq = closest.Seq
		mb.Error.ClosestIteration = closest.Iteration
		mb.Error.RecordedSHA = closest.RequestHash
		mb.Error.RecordingRedactions = closest.Redactions
		mb.Error.DivergesAt = divergesAt(closest.Request, req)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(mb)
}

// closest is the entry whose message list shares the longest common prefix
// with the request — advisory only (design §5.3).
func (r *Recording) closest(req chat.ChatRequest) *Entry {
	var best *Entry
	bestLen := -1
	for i := range r.Entries {
		var stored chat.ChatRequest
		if json.Unmarshal(r.Entries[i].Request, &stored) != nil {
			continue
		}
		n := commonMessagePrefix(stored.Messages, req.Messages)
		if n > bestLen {
			best, bestLen = &r.Entries[i], n
		}
	}
	return best
}

func commonMessagePrefix(a, b []chat.Message) int {
	n := 0
	for n < len(a) && n < len(b) {
		ja, _ := json.Marshal(a[n])
		jb, _ := json.Marshal(b[n])
		if string(ja) != string(jb) {
			break
		}
		n++
	}
	return n
}

// divergesAt names the first message that differs, as a JSON path.
func divergesAt(storedRaw json.RawMessage, req chat.ChatRequest) string {
	var stored chat.ChatRequest
	if json.Unmarshal(storedRaw, &stored) != nil {
		return ""
	}
	n := commonMessagePrefix(stored.Messages, req.Messages)
	switch {
	case n < len(stored.Messages) && n < len(req.Messages):
		return fmt.Sprintf("messages[%d]", n)
	case n < len(req.Messages):
		return fmt.Sprintf("messages[%d] (not in the recording)", n)
	case n < len(stored.Messages):
		return fmt.Sprintf("messages[%d] (missing from the request)", n)
	}
	return "tools or other fields"
}
