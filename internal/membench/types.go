package membench

import (
	"context"
	"errors"
	"time"
)

// The system-under-test seam (design §5.3). This interface is the only thing in
// the harness that knows WHICH memory system is being measured, which is what
// makes the head-to-head fair: both sides are driven over HTTP through one code
// path, so no asymmetry can hide in the client.

// ErrQuotaExhausted reports that the system under test refused further work for
// capacity reasons — rate limit, credit exhaustion, hard quota.
//
// The runner treats it as TERMINAL, not transient: abort the run, do not retry,
// record it in the manifest. Retrying burns quota to no purpose, and continuing
// is worse — later items would score zero for a billing reason rather than a
// retrieval one, producing a number that reads as a retrieval result. The
// journal makes the abort cheap: --resume picks up where it stopped.
var ErrQuotaExhausted = errors.New("membench: system quota exhausted")

// MemorySystem is a memory system the harness can drive.
type MemorySystem interface {
	// Name identifies the system in results and manifests.
	Name() string
	// Prepare creates or clears the isolated scope for one benchmark item.
	Prepare(ctx context.Context, scope string) error
	// Ingest loads one item's haystack into that scope.
	Ingest(ctx context.Context, scope string, items []Item) (IngestStats, error)
	// Recall retrieves against the scope. Implementations MUST pin isolation
	// (for the vornik adapter, strict_scope) — a leak here silently scores
	// cross-contaminated recall.
	Recall(ctx context.Context, scope string, q Query) (Recalled, error)
	// Teardown releases the scope. Best-effort: a failure here is logged, never
	// fatal, since it cannot affect a score already computed.
	Teardown(ctx context.Context, scope string) error
	// Config returns the system's effective configuration as it reports it,
	// for the comparability key (§5.6). An empty string means "could not
	// determine", which marks the key partial rather than matching.
	Config(ctx context.Context) (string, error)
}

// Item is one unit of a benchmark item's haystack.
type Item struct {
	DocumentID string `json:"document_id"`
	Content    string `json:"content"`
	// Context is a free-text framing line, NOT structured — e.g.
	// "Session <id> — you are the assistant in this conversation — happened on
	// <date> UTC." It is prepended by the adapter so both systems receive the
	// same provenance framing.
	Context string `json:"context,omitempty"`
	// EventTime is when this content pertains to. Zero = unknown. The whole
	// reason Phase 0 shipped first: without it, a dated dataset's temporal
	// categories are unanswerable.
	// No omitempty: a time.Time is never omitted by encoding/json, so the tag
	// would mislead anyone serialising an Item directly. Adapters build their
	// payloads by hand and skip a zero event time explicitly.
	EventTime time.Time `json:"event_time"`
}

// Query is one recall request.
type Query struct {
	Text string `json:"text"`
	// MaxTokens is the context budget. Adapters over a system that only accepts
	// top-k convert, and record that they did so — the conversion is a
	// methodological difference worth reporting, not hiding.
	MaxTokens int `json:"max_tokens"`
	// From / To bound the content's event time. Zero = unbounded.
	From time.Time `json:"from,omitempty"`
	To   time.Time `json:"to,omitempty"`
}

// Hit is one retrieved chunk.
type Hit struct {
	// SourceID is what gets compared against the item's gold document IDs, so
	// it must be the DOCUMENT identity the dataset labels — not a chunk id.
	SourceID string  `json:"source_id"`
	Text     string  `json:"text"`
	Score    float64 `json:"score"`
}

// Recalled is one recall's result.
type Recalled struct {
	Hits    []Hit         `json:"hits"`
	Tokens  int           `json:"tokens"`
	Latency time.Duration `json:"latency"`
	// CostUSD is nil when the system reports no cost. Nil rather than 0 so
	// "free" and "unknown" stay distinguishable in tier-3 reporting.
	CostUSD *float64 `json:"cost_usd,omitempty"`
}

// SourceIDs is the retrieved document identities in rank order, which is the
// input the tier-2 metrics take.
func (r Recalled) SourceIDs() []string {
	ids := make([]string, 0, len(r.Hits))
	for _, h := range r.Hits {
		ids = append(ids, h.SourceID)
	}
	return ids
}

// IngestStats records what one item's ingest actually did, including the
// asymmetries between systems. Reported rather than hidden (design §5.6): our
// remember path caps a deposit at 64 KiB, so a larger session is split on turn
// boundaries, which changes chunking versus a single-document retain elsewhere.
type IngestStats struct {
	Deposits int `json:"deposits"`
	Splits   int `json:"splits"`
	Rejected int `json:"rejected"`
	Bytes    int `json:"bytes"`
	// RejectedBytes drives the haystack-loss trust check (§5.9).
	RejectedBytes int `json:"rejected_bytes"`
	// ChunksStored is -1 when the system does not report it.
	ChunksStored int           `json:"chunks_stored"`
	Latency      time.Duration `json:"latency"`
}

// HaystackLoss is the fraction of submitted bytes the system refused. Feeds
// AssessTrust: past MaxHaystackLoss the item is being scored against an easier
// task than the dataset poses.
func (s IngestStats) HaystackLoss() float64 {
	if s.Bytes <= 0 {
		return 0
	}
	return float64(s.RejectedBytes) / float64(s.Bytes)
}

// Dataset loads benchmark items.
type Dataset interface {
	Name() string
	Load(path string, lim Limits) ([]BenchItem, error)
}

// Limits bounds what a run loads.
type Limits struct {
	MaxItems            int
	MaxItemsPerCategory int
	Category            string
}

// BenchItem is one question with its own haystack. The per-item haystack is the
// point: the task is finding a needle among THAT item's distractors, which is
// why items are isolated from each other (§5.5).
type BenchItem struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	Haystack []Item `json:"haystack"`
	QAs      []QA   `json:"qas"`
}

// QA is one question and its ground truth.
type QA struct {
	Question   string `json:"question"`
	GoldAnswer string `json:"gold_answer"`
	// GoldDocumentIDs are the documents that contain the answer — the label the
	// judge-free tier-2 metrics score against. Empty means this item can only be
	// scored by the judge, and its tier-2 metrics come back NaN rather than 0.
	GoldDocumentIDs []string `json:"gold_document_ids,omitempty"`
	// Rubric replaces literal-answer matching for preference-style questions,
	// where "correct" means the response used the personal information rather
	// than reproducing a string.
	Rubric string `json:"rubric,omitempty"`
}
