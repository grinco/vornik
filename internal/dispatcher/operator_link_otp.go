package dispatcher

// In-memory OTP store backing the cross-channel `/link` flow.
// One operator types /link on channel A; the dispatcher
// generates a short code keyed on (canonical operator id at A).
// Within the TTL window the operator types `/link <code>` on
// channel B; the dispatcher matches the code, links B's speaker
// id to A's canonical operator id, and (if B had its own
// profile) merges the two profiles.
//
// Store characteristics:
//   - Single in-memory map. A daemon restart drops pending OTPs
//     by design (5-minute window; restart-survivability isn't
//     worth the new migration).
//   - Codes are 8 hex chars (~32 bits) from crypto/rand. With
//     a 5-min TTL and no enumeration endpoint AND a per-claimant
//     failed-attempt lockout (operatorLinkMaxFailures within the
//     TTL window), online brute force is contained: a claiming
//     speaker that burns through the failure budget is locked
//     out for the rest of the window, even if it then guesses
//     the right code. Linking another speaker to your canonical
//     operator id is an identity-takeover primitive, so the
//     lockout matters despite the short window.
//   - Generated codes are uppercased + dashed (XXXX-YYYY) for
//     legibility in chat. The Claim path accepts case-insensitive
//     match + tolerates the operator typing or omitting the dash.
//
// Lifecycle: cleanupExpired runs lazily on every Issue+Claim
// call. No goroutine needed — small map, small TTL.

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

// operatorLinkOTPTTL bounds how long an issued code is valid.
// 5 minutes is the design value; long enough for an operator
// to alt-tab + paste, short enough that a leaked code is moot.
const operatorLinkOTPTTL = 5 * time.Minute

// operatorLinkMaxFailures is how many wrong (well-formed, unmatched)
// codes a single claiming speaker may submit within a lockout window
// before further Claim calls are refused for the rest of the window —
// even if a later guess is correct. Bounds online brute force of the
// 32-bit code space.
const operatorLinkMaxFailures = 5

// operatorLinkLockoutWindow is how long a claimant's failure count
// accumulates (and how long a lockout lasts). Tied to the OTP TTL: a
// code can't outlive the window anyway, so a locked-out attacker has
// nothing left to guess by the time it resets.
const operatorLinkLockoutWindow = operatorLinkOTPTTL

// operatorLinkOTPEntry is one outstanding link request.
type operatorLinkOTPEntry struct {
	// Issuer is the canonical operator id the code resolves to
	// at Claim time. New links from claiming speakers point at
	// this id.
	Issuer string
	// IssuedAt is the wall-clock moment the code was minted.
	// Compared against operatorLinkOTPTTL on every Claim +
	// during lazy cleanup.
	IssuedAt time.Time
}

// OperatorLinkOTPStore is the singleton facade exposed to the
// handlers. Methods are safe to call concurrently.
//
// One process, one store: package-level instance below. A future
// horizontal-scaling slice would swap this for a postgres-backed
// store keyed on (code) so the OTP survives leader failover.
type OperatorLinkOTPStore struct {
	mu      sync.Mutex
	entries map[string]operatorLinkOTPEntry
	// attempts tracks per-claimant failed Claim counts for the
	// brute-force lockout, keyed on the claiming speaker id.
	attempts map[string]operatorLinkAttempts
}

// operatorLinkAttempts is one claimant's running failure tally within
// the current lockout window.
type operatorLinkAttempts struct {
	failures    int
	windowStart time.Time
}

// NewOperatorLinkOTPStore allocates an empty store.
func NewOperatorLinkOTPStore() *OperatorLinkOTPStore {
	return &OperatorLinkOTPStore{
		entries:  make(map[string]operatorLinkOTPEntry),
		attempts: make(map[string]operatorLinkAttempts),
	}
}

// defaultOperatorLinkOTPStore is the package-level singleton
// the Telegram /link handler (and future webchat /link) reaches
// into. Lazily initialised on first use via sync.Once so a
// daemon that never sees a /link command pays no allocation
// cost.
var (
	defaultOperatorLinkOTPStore *OperatorLinkOTPStore
	defaultOperatorLinkOTPOnce  sync.Once
)

// DefaultOperatorLinkOTPStore returns the package-level
// singleton, initialising it on first call.
func DefaultOperatorLinkOTPStore() *OperatorLinkOTPStore {
	defaultOperatorLinkOTPOnce.Do(func() {
		defaultOperatorLinkOTPStore = NewOperatorLinkOTPStore()
	})
	return defaultOperatorLinkOTPStore
}

// Issue mints a new code for the issuer's canonical operator
// id. Returns the formatted code (XXXX-YYYY) the chat surface
// shows to the operator.
func (s *OperatorLinkOTPStore) Issue(issuer string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupExpiredLocked(time.Now())
	for {
		code := generateOperatorLinkCode()
		if _, exists := s.entries[code]; exists {
			continue
		}
		s.entries[code] = operatorLinkOTPEntry{
			Issuer:   issuer,
			IssuedAt: time.Now(),
		}
		return formatOperatorLinkCode(code)
	}
}

// Claim resolves a code to its issuer and removes the entry
// (single-use), on behalf of the claiming speaker `claimant`.
// Returns the issuer + ok=true on success. ok=false means the
// code was unknown / expired / malformed; locked=true means the
// claimant has exhausted its failed-attempt budget for this
// window and the call was refused WITHOUT consulting the code (so
// a correct guess can't escape an active lockout). A successful
// claim resets the claimant's failure tally.
func (s *OperatorLinkOTPStore) Claim(claimant, code string) (issuer string, ok bool, locked bool) {
	normalised := normaliseOperatorLinkCode(code)
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupExpiredLocked(now)

	// Lockout check first — an attacker that has burned the budget
	// must not be able to claim even with a correct code.
	if claimant != "" && s.isLockedLocked(claimant, now) {
		return "", false, true
	}
	// A malformed code can never match; treat it as a no-op (don't
	// burn the budget — the brute-force surface is well-formed
	// guesses, and a malformed string is almost always operator typo).
	if normalised == "" {
		return "", false, false
	}
	entry, found := s.entries[normalised]
	if !found {
		// Well-formed but unmatched: a guess. Count it, and report
		// locked=true if this failure tripped the threshold.
		nowLocked := s.recordFailureLocked(claimant, now)
		return "", false, nowLocked
	}
	delete(s.entries, normalised)
	delete(s.attempts, claimant) // success clears the failure tally
	return entry.Issuer, true, false
}

// isLockedLocked reports whether the claimant is currently locked out.
// Caller holds s.mu.
func (s *OperatorLinkOTPStore) isLockedLocked(claimant string, now time.Time) bool {
	a, ok := s.attempts[claimant]
	if !ok {
		return false
	}
	if now.Sub(a.windowStart) > operatorLinkLockoutWindow {
		return false // window elapsed; cleanup will prune it
	}
	return a.failures >= operatorLinkMaxFailures
}

// recordFailureLocked increments the claimant's failure tally (starting
// a fresh window when none is active) and returns whether the claimant
// is now locked. Caller holds s.mu. A blank claimant is not tracked
// (no key to lock on) so blank-claimant guesses are not throttled —
// acceptable because every real channel supplies a speaker id.
func (s *OperatorLinkOTPStore) recordFailureLocked(claimant string, now time.Time) bool {
	if claimant == "" {
		return false
	}
	a, ok := s.attempts[claimant]
	if !ok || now.Sub(a.windowStart) > operatorLinkLockoutWindow {
		a = operatorLinkAttempts{windowStart: now}
	}
	a.failures++
	s.attempts[claimant] = a
	return a.failures >= operatorLinkMaxFailures
}

// pending reports the number of outstanding entries. Exported
// for test inspection only; production code shouldn't depend on
// it.
func (s *OperatorLinkOTPStore) pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupExpiredLocked(time.Now())
	return len(s.entries)
}

func (s *OperatorLinkOTPStore) cleanupExpiredLocked(now time.Time) {
	for code, e := range s.entries {
		if now.Sub(e.IssuedAt) > operatorLinkOTPTTL {
			delete(s.entries, code)
		}
	}
	for claimant, a := range s.attempts {
		if now.Sub(a.windowStart) > operatorLinkLockoutWindow {
			delete(s.attempts, claimant)
		}
	}
}

// generateOperatorLinkCode returns 8 uppercase hex chars.
// 32 bits of entropy in a 5-minute window is plenty against
// brute force (no enumeration endpoint, no rate limit needed
// because the store ages entries out automatically).
func generateOperatorLinkCode() string {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand errors are exceptional; fall back to a
		// time-derived code so the caller can still proceed.
		// Operator can re-issue if the entropy ever matters.
		return strings.ToUpper(time.Now().UTC().Format("150405") + "FF")[:8]
	}
	return strings.ToUpper(hex.EncodeToString(buf[:]))
}

// formatOperatorLinkCode inserts the visible "XXXX-YYYY" dash
// so the operator can read it back without dropping characters.
// Claim normalises the input before matching, so the dash is
// purely cosmetic.
func formatOperatorLinkCode(code string) string {
	if len(code) < 8 {
		return code
	}
	return code[:4] + "-" + code[4:]
}

// normaliseOperatorLinkCode strips dashes + whitespace and
// upper-cases the result so case + dash differences between
// what the operator typed and what was stored don't fail the
// match.
func normaliseOperatorLinkCode(raw string) string {
	out := strings.ToUpper(strings.NewReplacer("-", "", " ", "", "\t", "").Replace(strings.TrimSpace(raw)))
	if len(out) != 8 {
		return ""
	}
	return out
}
