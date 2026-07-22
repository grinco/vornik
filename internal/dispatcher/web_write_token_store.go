package dispatcher

import "sync"

// WebWriteTokenStore is the operator-chat-driven v1 delivery channel for
// web-write approval tokens (LLD 2026-07-21-supervised-web-write-actions,
// Components 5). In the OPERATOR-CHAT-DRIVEN model there is no autonomous agent
// paused at AWAITING_APPROVAL to route a resume signal to: the operator-chat
// assistant issues web_submit(mode=preview), the operator approves the pending
// row in the authenticated /inbox surface, and the assistant later issues
// web_submit(mode=submit) WITHOUT holding the approval token. The raw token is
// therefore never rendered in the UI nor handed to the LLM; instead the inbox
// approve handler deposits it here (Put), keyed by submission_id, and the submit
// path retrieves it (Take) daemon-side.
//
// It is deliberately tiny and concurrency-safe: the inbox HTTP handler and the
// dispatcher tool loop run on independent goroutines. Take is single-use — it
// removes the token so a token can be redeemed by exactly one submit — which
// composes with the persistence-layer approved→submitting CAS (the real
// double-submit guard) as a cheap first line of defence. A plain map+mutex is
// sufficient at the expected cardinality (a handful of pending approvals at a
// time); a bounded/TTL-swept variant is a future refinement, not required here.
type WebWriteTokenStore struct {
	mu     sync.Mutex
	tokens map[string]string
}

// NewWebWriteTokenStore constructs an empty store. The service container builds
// exactly one and shares it between the dispatcher Agent (Take on submit) and
// the UI server (Put on approve).
func NewWebWriteTokenStore() *WebWriteTokenStore {
	return &WebWriteTokenStore{tokens: make(map[string]string)}
}

// Put stores (or replaces) the approval token for submissionID. A re-approval
// simply overwrites the prior token — the whole-row hash verification on submit
// still binds it to the current row content. Nil-safe / empty-id-safe no-op so
// callers need not guard.
func (s *WebWriteTokenStore) Put(submissionID, token string) {
	if s == nil || submissionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.tokens == nil {
		s.tokens = make(map[string]string)
	}
	s.tokens[submissionID] = token
}

// Take returns the token for submissionID and removes it (single-use). The
// second return is false when no token is held (never approved, already taken,
// or explicitly deleted). Nil-safe.
func (s *WebWriteTokenStore) Take(submissionID string) (string, bool) {
	if s == nil || submissionID == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tok, ok := s.tokens[submissionID]
	if ok {
		delete(s.tokens, submissionID)
	}
	return tok, ok
}

// Delete drops any token held for submissionID without redeeming it — used to
// evict a token whose submission was rejected/expired out of band. Nil-safe.
func (s *WebWriteTokenStore) Delete(submissionID string) {
	if s == nil || submissionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tokens, submissionID)
}
