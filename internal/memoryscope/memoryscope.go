// Package memoryscope resolves the repo_scope stamped on memory chunks.
//
// The scope partitions one project's RAG so an operator's many repos don't
// cross-pollute each other's recall results (migration 75/76). NULL means
// uncategorized — visible under every scoped recall — and "*" means
// deliberately cross-cutting.
//
// WHY THIS PACKAGE EXISTS. The payload extraction lived in TWO places:
// executor.extractRepoScopeFromPayload and, byte-identical,
// rag.repoScopeFromPayload, whose comment said it was "duplicated across the
// package boundary to keep the rag package's dependencies narrow". A third
// caller needed it (the project-default fallback below), and three copies of a
// rule that decides where data lands is how the rule quietly stops agreeing with
// itself. This is a leaf with no dependencies beyond the standard library, so
// importing it costs neither package its narrow surface.
//
// WHAT IT FIXED, 2026-07-30. The scope was stamped ONLY from the task payload,
// which only the companion delegate handler populates. Every chunk produced by a
// task created any other way — chat, REST POST /tasks, an autonomy tick in any
// of its three modes, Telegram/Slack/email — landed NULL, because nothing
// supplied a default. A census of the live store found five projects at 100%
// NULL (assistant 6739 chunks, janka 2776, vornik-marketing 266, ibkr-trader 85,
// headmatch 3): none of them had ever been written to through a companion call.
package memoryscope

import (
	"encoding/json"
	"strings"
)

// FromPayload pulls repo_scope out of a task payload. Two accepted shapes:
//
//   - canonical: payload.context.repo_scope — the companion delegate handler
//     and REST POST /tasks both land here, because taskcreate.Creator wraps
//     RawContext under the "context" key when building the final payload.
//   - legacy: payload.repo_scope (unnested), kept for callers that bypass the
//     context wrapper.
//
// Returns "" when missing, malformed, or whitespace-only. That is the no-op
// signal: it means "the payload said nothing", NOT "scope this to nothing".
// Callers distinguish those by consulting Resolve rather than this directly.
func FromPayload(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var p struct {
		RepoScope string `json:"repo_scope"`
		Context   struct {
			RepoScope string `json:"repo_scope"`
		} `json:"context"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}
	if s := strings.TrimSpace(p.Context.RepoScope); s != "" {
		return s
	}
	return strings.TrimSpace(p.RepoScope)
}

// Resolve returns the scope to stamp, preferring the per-task value over the
// project default.
//
// The precedence is deliberate and one-directional: an explicit per-call scope
// always wins, because the caller knows which repo the work concerned and the
// project default is only a guess about the common case. The default fills the
// silence, and silence is what produced five entirely-NULL projects.
//
// An empty result still means "stamp nothing" (NULL / uncategorized). A project
// that genuinely is not repo-bound — a job hunt, a trading account, a marketing
// feed — sets no default and keeps NULL, which is CORRECT for it rather than a
// gap to be backfilled: NULL chunks surface under every scoped recall, so they
// are un-partitioned, not lost.
func Resolve(payload []byte, projectDefault string) string {
	if s := FromPayload(payload); s != "" {
		return s
	}
	return strings.TrimSpace(projectDefault)
}

// Ptr returns a pointer to scope for the indexer's optional-field contract, or
// nil when scope is empty. Callers were hand-rolling this three-line dance at
// each ingest site and one of them could have got the nil case wrong, which
// would stamp an empty string where the column expects NULL.
func Ptr(scope string) *string {
	if strings.TrimSpace(scope) == "" {
		return nil
	}
	s := scope
	return &s
}
