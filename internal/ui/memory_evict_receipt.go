package ui

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// The receipt shown after a hard eviction, and why it is signed.
//
// The counts ride the POST-redirect-GET because nothing else can carry them:
// memory_eviction_audit records the CHUNKS, and what those chunks derived —
// graph entities and edges, the quarantined pre-ingest copy, the cached
// embedding — is deleted and then recorded nowhere.
//
// A 2026-08-21 review pointed out that plain query parameters make this a
// FORGEABLE compliance receipt: mail an operator
// /ui/memory/p1?notice=evicted&chunks=500 and they see a professional-looking
// confirmation that an erasure happened. The banner even tells them the derived
// counts are "recorded nowhere else", which discourages the one check that would
// disprove it. The original reasoning — "wrong numbers are cosmetic and
// disprovable from the audit table below" — was too weak for a surface whose own
// help names Article 17: a fabricated evidence trail is worse than no evidence
// trail, because the operator stops looking.
//
// So the counts are signed, and the signature covers the project and a
// timestamp. A crafted link no longer renders anything, and a real receipt stops
// rendering after receiptTTL, which also ends the replay the same review raised
// — a bookmarked or back-buttoned URL showing "Erased 500 chunks" months later,
// indistinguishable from a fresh one.
//
// The key is per-process and random. A daemon restart invalidates outstanding
// receipts, which costs an operator a banner they have already read and keeps
// this free of key management — the receipt is a convenience with a short life,
// not the durable record. (The durable record is filed separately: the derived
// counts belong in the tombstone table.)

// receiptTTL bounds how long a receipt renders. Long enough to survive a reload
// or a detour into another tab; far too short to be a link worth keeping.
const receiptTTL = 15 * time.Minute

// evictionNotice is the receipt. Every field is an integer, and the template
// supplies all the wording — no operator-supplied text reaches the page, the
// same discipline cpFlashMessages applies to the control-plane flashes.
type evictionNotice struct {
	Chunks     int
	Entities   int
	Edges      int
	Quarantine int
	Cached     int
	// At is when the eviction completed. Shown so a receipt cannot be mistaken
	// for a fresher one than it is.
	At time.Time
}

// Derived reports whether anything BEYOND the chunks went, so the template can
// stay quiet about derived rows when there were none rather than print zeroes.
func (n *evictionNotice) Derived() bool {
	if n == nil {
		return false
	}
	return n.Entities > 0 || n.Edges > 0 || n.Quarantine > 0 || n.Cached > 0
}

// receiptKey returns the per-process signing key, generating it on first use.
func (s *Server) receiptKey() []byte {
	s.receiptKeyOnce.Do(func() {
		k := make([]byte, 32)
		if _, err := rand.Read(k); err != nil {
			// Cannot sign, so nothing will ever verify: the banner disappears
			// rather than becoming forgeable. Failing closed here costs a
			// convenience; failing open would restore the defect.
			s.receiptSigningKey = nil
			return
		}
		s.receiptSigningKey = k
	})
	return s.receiptSigningKey
}

// receiptPayload is the exact string the signature covers. The project is in it
// so a receipt cannot be replayed onto a different project's page.
func receiptPayload(projectID string, n evictionNotice, unix int64) string {
	return fmt.Sprintf("evicted|%s|%d|%d|%d|%d|%d|%d",
		projectID, n.Chunks, n.Entities, n.Edges, n.Quarantine, n.Cached, unix)
}

func (s *Server) signReceipt(projectID string, n evictionNotice, unix int64) string {
	key := s.receiptKey()
	if len(key) == 0 {
		return ""
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(receiptPayload(projectID, n, unix)))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// evictionRedirect builds the post-eviction URL, counts and signature included.
func (s *Server) evictionRedirect(projectID string, n evictionNotice, now time.Time) string {
	unix := now.Unix()
	q := url.Values{}
	q.Set("notice", "evicted")
	q.Set("chunks", strconv.Itoa(n.Chunks))
	q.Set("entities", strconv.Itoa(n.Entities))
	q.Set("edges", strconv.Itoa(n.Edges))
	q.Set("quarantined", strconv.Itoa(n.Quarantine))
	q.Set("cached", strconv.Itoa(n.Cached))
	q.Set("at", strconv.FormatInt(unix, 10))
	if sig := s.signReceipt(projectID, n, unix); sig != "" {
		q.Set("sig", sig)
	}
	return "/ui/memory/" + url.PathEscape(projectID) + "?" + q.Encode()
}

// parseEvictionNotice rebuilds the receipt from the redirect, or returns nil.
//
// nil on anything unexpected — wrong token, bad signature, missing or expired
// timestamp, unparseable counts. A receipt that does not verify is not shown as
// a receipt with zeroes; it is not shown at all, because a banner an operator
// cannot trust is worse than none.
func (s *Server) parseEvictionNotice(projectID string, q url.Values, now time.Time) *evictionNotice {
	if q.Get("notice") != "evicted" {
		return nil
	}
	atoi := func(key string) (int, bool) {
		raw := q.Get(key)
		if raw == "" {
			return 0, true // absent means zero, which is a real outcome
		}
		v, err := strconv.Atoi(raw)
		if err != nil || v < 0 {
			return 0, false
		}
		return v, true
	}
	var n evictionNotice
	var ok bool
	for _, f := range []struct {
		key string
		dst *int
	}{
		{"chunks", &n.Chunks}, {"entities", &n.Entities}, {"edges", &n.Edges},
		{"quarantined", &n.Quarantine}, {"cached", &n.Cached},
	} {
		if *f.dst, ok = atoi(f.key); !ok {
			return nil
		}
	}

	unix, err := strconv.ParseInt(q.Get("at"), 10, 64)
	if err != nil {
		return nil
	}
	at := time.Unix(unix, 0)
	// Both directions: an expired receipt is a replay, and one from the future
	// is a forgery attempt or a clock that cannot be reasoned about.
	if now.Sub(at) > receiptTTL || at.After(now.Add(time.Minute)) {
		return nil
	}

	want := s.signReceipt(projectID, n, unix)
	if want == "" || !hmac.Equal([]byte(want), []byte(q.Get("sig"))) {
		return nil
	}
	n.At = at
	return &n
}
