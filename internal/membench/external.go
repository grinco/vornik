package membench

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// The external adapter (design §5.3). Drives a generic external HTTP
// agent-memory service — one call to retain a document, one to recall against a
// namespace — over the same client shape the vornik adapter uses, so the fairness
// story is a property of the harness rather than a promise about two code paths.
//
// It is deliberately SOURCE-AGNOSTIC. The service's real route names cannot be
// verified until a live run, so they are configuration with a conventional REST
// default rather than a hardcoded guess: a wrong constant compiled into the
// adapter would present as total retrieval failure and would be a lie in the code
// until someone chased it down. The request BODY field names are likewise our
// convention and not a verified contract; the recall decoder therefore accepts
// the obvious alternates for the two fields that matter (the document identity
// and the hit text) rather than silently returning nothing.

const (
	// defaultExternalIngestPath and defaultExternalRecallPath are a conventional
	// REST shape, not a claim about any particular service.
	defaultExternalIngestPath = "/v1/retain"
	defaultExternalRecallPath = "/v1/recall"
	// defaultExternalBankPrefix namespaces every bank the harness creates, so a
	// bank belonging to a benchmark run is recognisable as ours before anything
	// deletes it.
	defaultExternalBankPrefix = "membench"
	// bankPlaceholder is substituted in a path template for a service that
	// namespaces in the URL rather than in the body.
	bankPlaceholder = "{bank}"
	// bankScopeTokenMax bounds the readable part of a bank id. Truncation is safe
	// because the digest suffix — not the readable part — carries uniqueness.
	bankScopeTokenMax = 48
)

// ExternalConfig configures the adapter.
type ExternalConfig struct {
	// BaseURL is the service root, e.g. https://api.example.com.
	BaseURL string
	// Token is the API credential, sent as a bearer token.
	Token string
	// Client allows a test server's client to be injected. Nil uses a default with
	// a generous timeout — ingest of a large haystack is not fast.
	Client *http.Client

	// IngestPath and RecallPath are path templates. Empty uses the conventional
	// defaults above. A {bank} placeholder is substituted with the item's bank id.
	IngestPath string
	RecallPath string
	// BankCreatePath and BankDeletePath are optional lifecycle routes. Empty makes
	// Prepare and Teardown no-ops, which is the safe default: a bank that comes
	// into existence with its first retain needs no creation, exactly as one of our
	// repo_scopes does, and firing a request at a guessed route would fail every
	// run.
	BankCreatePath string
	BankDeletePath string
	// ConfigPath is an optional GET endpoint reporting the service's effective
	// configuration for the comparability key (§5.6). Empty means we cannot read
	// it, and Config reports that as an empty string rather than a guess.
	ConfigPath string

	// BankPrefix namespaces this run's banks. Empty uses defaultExternalBankPrefix.
	// Two concurrent runs against one service account MUST differ here or they
	// write into each other's haystacks.
	BankPrefix string

	// TopKOnly declares that the service accepts a result count rather than a
	// token budget. The conversion is then performed AND recorded, because a
	// silent unit change turns an unequal-budget comparison into one that looks
	// equal (§5.6).
	TopKOnly bool

	// ExtractionModel is the comparison system's extraction model as the operator
	// configured it, recorded in the comparability key. Distinct from Config: this
	// is what we were told, that is what the service reports.
	ExtractionModel string
}

// ExternalSystem implements MemorySystem against an external HTTP memory service.
type ExternalSystem struct {
	cfg    ExternalConfig
	client *http.Client

	// notes accumulates methodological differences for the manifest — currently
	// the token-budget conversion. Deduplicated, because a 500-item run must not
	// bury one real difference under 500 copies of it.
	//
	// Guarded: §5.10 gives the runner a --parallel N flag and one adapter instance
	// is shared across items, so an unguarded slice here is a data race that either
	// crashes the run or drops a note the manifest should have carried.
	notesMu sync.Mutex
	notes   []string
}

// NewExternalSystem builds the adapter.
func NewExternalSystem(cfg ExternalConfig) *ExternalSystem {
	c := cfg.Client
	if c == nil {
		c = &http.Client{Timeout: 5 * time.Minute}
	}
	return &ExternalSystem{cfg: cfg, client: c}
}

// Name identifies the system in results and manifests.
func (e *ExternalSystem) Name() string { return "external" }

// BankID is the service-side namespace for one benchmark item's scope.
//
// This is the external counterpart of the vornik adapter's strict_scope pinning
// and it is the invariant that matters most: two items sharing a bank means item
// A's haystack can answer item B's question, and the run scores
// cross-contaminated recall while looking healthy (§5.5).
//
// The readable scope token is sanitised for URL and path safety, which is NOT
// injective on its own — "lme/q1" and "lme-q1" sanitise alike — so a digest of
// the raw scope is appended. Without it, a dataset whose ids mix the two
// separators would silently share a haystack.
func (e *ExternalSystem) BankID(scope string) string {
	prefix := strings.TrimSpace(e.cfg.BankPrefix)
	if prefix == "" {
		prefix = defaultExternalBankPrefix
	}
	sum := sha256.Sum256([]byte(scope))
	return prefix + "-" + sanitizeBankToken(scope) + "-" + hex.EncodeToString(sum[:4])
}

// Notes returns the methodological differences recorded so far, for the manifest.
// Reported rather than hidden, per §5.6.
func (e *ExternalSystem) Notes() []string {
	e.notesMu.Lock()
	defer e.notesMu.Unlock()
	out := make([]string, len(e.notes))
	copy(out, e.notes)
	return out
}

// Prepare creates the item's bank where the service offers a creation route.
//
// Unlike Teardown this is not best-effort: an unprepared bank means the haystack
// lands in a namespace nobody chose, and every subsequent number for the item is
// meaningless.
func (e *ExternalSystem) Prepare(ctx context.Context, scope string) error {
	if strings.TrimSpace(e.cfg.BankCreatePath) == "" {
		return nil
	}
	bank := e.BankID(scope)
	path := e.resolvePath(e.cfg.BankCreatePath, "", bank)
	if err := e.doJSON(ctx, http.MethodPost, path, map[string]any{"bank": bank}, nil); err != nil {
		return fmt.Errorf("create bank %s: %w", bank, err)
	}
	return nil
}

// Teardown deletes the item's bank where the service offers a deletion route.
//
// The error is returned rather than swallowed: teardown is best-effort at the
// RUNNER, which logs it, and a leaked bank costs the operator money the runner
// can only report if it is told.
func (e *ExternalSystem) Teardown(ctx context.Context, scope string) error {
	if strings.TrimSpace(e.cfg.BankDeletePath) == "" {
		return nil
	}
	bank := e.BankID(scope)
	path := e.resolvePath(e.cfg.BankDeletePath, "", bank)
	if err := e.doJSON(ctx, http.MethodDelete, path, nil, nil); err != nil {
		return fmt.Errorf("delete bank %s: %w", bank, err)
	}
	return nil
}

// Config reports the service's effective configuration for the comparability key.
//
// Returns an EMPTY string whenever it cannot be read — no endpoint configured, a
// failed request, an undecodable reply. Empty marks the key partial (§5.6), which
// is the honest answer: "could not verify" is not "unchanged", and substituting a
// plausible value such as the configured ExtractionModel would let the service
// swap its embedding model between runs while the key still matched. That
// distinction was a specific round-2 review finding.
//
// The reply is re-marshalled from a decoded map rather than passed through
// verbatim, so key ordering and whitespace on the service's side cannot make two
// identical configurations hash differently and render every run incomparable.
func (e *ExternalSystem) Config(ctx context.Context) (string, error) {
	if strings.TrimSpace(e.cfg.ConfigPath) == "" {
		return "", nil
	}
	var reported map[string]any
	path := e.resolvePath(e.cfg.ConfigPath, "", "")
	if err := e.doJSON(ctx, http.MethodGet, path, nil, &reported); err != nil {
		return "", fmt.Errorf("read external config: %w", err)
	}
	if len(reported) == 0 {
		return "", fmt.Errorf("read external config: %s returned no configuration", path)
	}
	canonical, err := json.Marshal(reported)
	if err != nil {
		return "", fmt.Errorf("canonicalise external config: %w", err)
	}
	return string(canonical), nil
}

// Ingest retains one item's haystack in the item's bank.
//
// Each Item is one retain of a whole document. That is the asymmetry §5.6 names
// and reports: our remember path caps a deposit at 64 KiB and splits above it,
// which chunks differently — so Splits stays zero here rather than borrowing our
// cap and misattributing it to the other system.
//
// The Context line is prepended to the body exactly as the vornik adapter
// prepends it. Identical provenance framing on both sides is what makes the
// head-to-head fair; a difference here would be measured as a retrieval
// difference.
func (e *ExternalSystem) Ingest(ctx context.Context, scope string, items []Item) (IngestStats, error) {
	stats := IngestStats{ChunksStored: -1} // -1 until the service reports a count
	bank := e.BankID(scope)
	path := e.resolvePath(e.cfg.IngestPath, defaultExternalIngestPath, bank)
	chunksReported := false
	start := time.Now()

	for _, item := range items {
		body := item.Content
		if item.Context != "" {
			body = item.Context + "\n\n" + item.Content
		}
		payload := map[string]any{
			"bank":        bank,
			"document_id": item.DocumentID,
			"content":     body,
		}
		if !item.EventTime.IsZero() {
			// Omitted when unknown rather than sent as year 0001: a fabricated date
			// would place the document outside every temporal window instead of
			// leaving it unfiltered.
			payload["event_time"] = item.EventTime.UTC().Format(time.RFC3339)
		}

		var reply externalRetainReply
		if err := e.doJSON(ctx, http.MethodPost, path, payload, &reply); err != nil {
			// Stop here. Pressing on would score the item against a haystack known to
			// be incomplete, which measures an easier task than the dataset poses.
			stats.Latency = time.Since(start)
			return stats, fmt.Errorf("retain %s: %w", item.DocumentID, err)
		}
		stats.Deposits++
		stats.Bytes += len(body)
		if reply.refused() {
			stats.Rejected++
			// Counted in bytes, not deposits: haystack loss is measured in content,
			// and one rejected 60 KiB session matters far more than a 200-byte one.
			stats.RejectedBytes += len(body)
		}
		if reply.ChunksStored != nil {
			if !chunksReported {
				chunksReported = true
				stats.ChunksStored = 0
			}
			stats.ChunksStored += *reply.ChunksStored
		}
	}
	stats.Latency = time.Since(start)
	return stats, nil
}

// Recall retrieves against the item's bank.
//
// The token budget is sent VERBATIM in its own units — equal budgets are a named
// fairness control. Where the service accepts only a result count, the conversion
// is sent alongside the budget and recorded in Notes, so the manifest shows the
// units changed instead of implying both systems were asked the same way (§5.3).
func (e *ExternalSystem) Recall(ctx context.Context, scope string, q Query) (Recalled, error) {
	bank := e.BankID(scope)
	payload := map[string]any{"bank": bank, "query": q.Text}
	if q.MaxTokens > 0 {
		payload["max_tokens"] = q.MaxTokens
		if e.cfg.TopKOnly {
			k := tokenBudgetToLimit(q.MaxTokens)
			payload["top_k"] = k
			e.note(fmt.Sprintf(
				"external recall: token budget %d converted to top_k=%d (service accepts top-k only)",
				q.MaxTokens, k))
		}
	}
	if !q.From.IsZero() {
		payload["from"] = q.From.UTC().Format(time.RFC3339)
	}
	if !q.To.IsZero() {
		payload["to"] = q.To.UTC().Format(time.RFC3339)
	}

	path := e.resolvePath(e.cfg.RecallPath, defaultExternalRecallPath, bank)
	start := time.Now()
	var reply externalRecallReply
	if err := e.doJSON(ctx, http.MethodPost, path, payload, &reply); err != nil {
		return Recalled{}, err
	}

	out := Recalled{Latency: time.Since(start), CostUSD: reply.CostUSD}
	var estimated int
	for _, h := range reply.hits() {
		out.Hits = append(out.Hits, Hit{
			// The DOCUMENT identity the dataset labels. Never the chunk id: tier-2
			// metrics score against gold document ids, so a chunk id here would make
			// recall 0 everywhere for a reason unrelated to retrieval.
			SourceID: h.documentID(),
			Text:     h.text(),
			Score:    h.Score,
		})
		estimated += approxTokens(h.text())
	}
	// The service's own count beats our estimate; the estimate stands in so budget
	// utilisation is never reported as zero merely because it was not returned.
	out.Tokens = reply.Tokens
	if out.Tokens <= 0 {
		out.Tokens = estimated
	}
	return out, nil
}

// note records a methodological difference once.
func (e *ExternalSystem) note(msg string) {
	e.notesMu.Lock()
	defer e.notesMu.Unlock()
	for _, n := range e.notes {
		if n == msg {
			return
		}
	}
	e.notes = append(e.notes, msg)
}

// externalRetainReply is one retain response. Every field is optional: a service
// that answers 204 with no body has accepted the document, and reading that as a
// refusal would invent haystack loss that did not happen.
type externalRetainReply struct {
	Accepted     *bool  `json:"accepted"`
	Rejected     int    `json:"rejected"`
	Status       string `json:"status"`
	ChunksStored *int   `json:"chunks_stored"`
}

// refused reports whether the service declined the document, over the three
// shapes it might say so in.
func (r externalRetainReply) refused() bool {
	if r.Rejected > 0 || strings.EqualFold(r.Status, "rejected") {
		return true
	}
	return r.Accepted != nil && !*r.Accepted
}

// externalHit is one retrieved chunk. The alternate field names are deliberate:
// the body contract is unverified, and returning nothing from a well-formed reply
// would be scored as a retrieval miss.
type externalHit struct {
	ChunkID    string  `json:"chunk_id"`
	DocumentID string  `json:"document_id"`
	SourceID   string  `json:"source_id"`
	Text       string  `json:"text"`
	Content    string  `json:"content"`
	Score      float64 `json:"score"`
}

// documentID is the identity compared against the item's gold document ids.
// ChunkID is read but never used as a fallback: an internal chunk id is not
// comparable to a gold document id, and falling back to it would produce a
// populated-looking result that scores zero.
func (h externalHit) documentID() string {
	if h.DocumentID != "" {
		return h.DocumentID
	}
	return h.SourceID
}

func (h externalHit) text() string {
	if h.Text != "" {
		return h.Text
	}
	return h.Content
}

// externalRecallReply is one recall response.
type externalRecallReply struct {
	Hits    []externalHit `json:"hits"`
	Results []externalHit `json:"results"`
	Tokens  int           `json:"tokens"`
	// CostUSD stays a pointer so "free" and "not reported" remain distinguishable
	// in tier-3 reporting.
	CostUSD *float64 `json:"cost_usd"`
}

func (r externalRecallReply) hits() []externalHit {
	if len(r.Hits) > 0 {
		return r.Hits
	}
	return r.Results
}

// resolvePath expands a path template. An empty template falls back to fallback,
// which is empty only for the optional routes — those are never reached with an
// empty template because their callers check first.
func (e *ExternalSystem) resolvePath(tmpl, fallback, bank string) string {
	p := strings.TrimSpace(tmpl)
	if p == "" {
		p = fallback
	}
	p = strings.ReplaceAll(p, bankPlaceholder, url.PathEscape(bank))
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
}

// doJSON performs one request against the service and decodes its JSON body into
// out. out may be nil for a call with no interesting reply.
func (e *ExternalSystem) doJSON(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal %s %s: %w", method, path, err)
		}
		reader = bytes.NewReader(raw)
	}

	target := strings.TrimRight(e.cfg.BaseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return fmt.Errorf("build %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	if e.cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+e.cfg.Token)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return externalStatusError(method, path, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read %s %s response: %w", method, path, err)
	}
	if len(bytes.TrimSpace(payload)) == 0 {
		// A bodiless success is legitimate for a retain or a bank create; the caller
		// decides whether an empty reply is acceptable for what it asked.
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("decode %s %s response: %w", method, path, err)
	}
	return nil
}

// externalStatusError classifies a non-2xx status.
//
// 429 and 402 are capacity refusals and join ErrQuotaExhausted so the runner
// treats them as terminal: abort, no retry, record it. Every OTHER non-2xx is a
// plain error, because conflating a transient 500 with quota exhaustion would
// abort a whole run over one blip — and continuing past a real quota refusal
// would score later items zero for a billing reason that reads as a retrieval
// result.
func externalStatusError(method, path string, code int) error {
	err := fmt.Errorf("%s %s: http %d", method, path, code)
	if code == http.StatusTooManyRequests || code == http.StatusPaymentRequired {
		return errors.Join(err, ErrQuotaExhausted)
	}
	return err
}

// sanitizeBankToken reduces a scope to characters safe in a URL path segment and
// in any plausible identifier field. Lossy by design — BankID pairs it with a
// digest of the raw scope, so the loss cannot merge two scopes.
func sanitizeBankToken(scope string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(scope) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
		if b.Len() >= bankScopeTokenMax {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}
