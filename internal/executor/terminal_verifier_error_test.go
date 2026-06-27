package executor

import (
	"errors"
	"testing"

	"vornik.io/vornik/internal/verifier"
)

func TestTerminalVerifierError_ErrorAndUnwrap(t *testing.T) {
	inner := errors.New("phase-2 verifier(s) failed: verifier \"no_rate_limit_blocks\" (no_status_429_in_audit): 429 detected")
	wrapped := &TerminalVerifierError{Err: inner}

	if wrapped.Error() != inner.Error() {
		t.Fatalf("Error() should match inner: got %q want %q", wrapped.Error(), inner.Error())
	}
	if !errors.Is(wrapped, inner) {
		t.Fatal("errors.Is should walk through Unwrap")
	}
	var asTerm *TerminalVerifierError
	if !errors.As(inner, &asTerm) {
		// Plain inner is NOT terminal.
		if asTerm != nil {
			t.Fatal("plain inner err should not match")
		}
	}
	if !errors.As(wrapped, &asTerm) || asTerm == nil {
		t.Fatal("errors.As on wrapped should populate the var")
	}
}

func TestTerminalVerifierError_NilSafe(t *testing.T) {
	var nilErr *TerminalVerifierError
	if got := nilErr.Error(); got != "terminal verifier failure" {
		t.Fatalf("nil receiver Error(): %q", got)
	}
	if got := nilErr.Unwrap(); got != nil {
		t.Fatalf("nil receiver Unwrap should be nil, got %v", got)
	}
	// Empty wrapper.
	empty := &TerminalVerifierError{}
	if got := empty.Error(); got != "terminal verifier failure" {
		t.Fatalf("empty Err: %q", got)
	}
}

func TestIsTerminalVerifierError(t *testing.T) {
	if isTerminalVerifierError(nil) {
		t.Fatal("nil err")
	}
	if isTerminalVerifierError(errors.New("plain")) {
		t.Fatal("plain err")
	}
	if !isTerminalVerifierError(&TerminalVerifierError{Err: errors.New("x")}) {
		t.Fatal("direct terminal")
	}
	// errors.As walks Unwrap chains; wrap a terminal in another error.
	wrapped := wrapErr(&TerminalVerifierError{Err: errors.New("inner")}, "outer")
	if !isTerminalVerifierError(wrapped) {
		t.Fatal("wrapped terminal")
	}
}

// wrapErr is a tiny test-local fmt.Errorf("%w") so we can verify the
// errors.As walk through Unwrap, not just the top-level type.
func wrapErr(inner error, msg string) error {
	return &wrappedErr{msg: msg, inner: inner}
}

type wrappedErr struct {
	msg   string
	inner error
}

func (w *wrappedErr) Error() string { return w.msg }
func (w *wrappedErr) Unwrap() error { return w.inner }

func TestJoinVerifierErrors(t *testing.T) {
	// Empty list → nil error.
	if got := joinVerifierErrors(nil, false); got != nil {
		t.Fatalf("empty: %v", got)
	}
	if got := joinVerifierErrors([]string{}, true); got != nil {
		t.Fatalf("empty with terminal: %v", got)
	}
	// Non-terminal → plain joined error.
	got := joinVerifierErrors([]string{"a", "b"}, false)
	if got == nil || !errors.Is(got, got) {
		t.Fatal("non-terminal: nil")
	}
	if isTerminalVerifierError(got) {
		t.Fatal("non-terminal must not be TerminalVerifierError")
	}
	if got.Error() != "phase-2 verifier(s) failed: a; b" {
		t.Fatalf("format: %q", got.Error())
	}
	// Terminal → wrapped.
	got = joinVerifierErrors([]string{"x"}, true)
	if !isTerminalVerifierError(got) {
		t.Fatal("terminal must wrap")
	}
	if got.Error() != "phase-2 verifier(s) failed: x" {
		t.Fatalf("terminal format: %q", got.Error())
	}
}

// TestJoinVerifierErrorsWithBlocks_NonTerminalWithBlocksWrapsRecoverable —
// when the failing violations carry permanent-block URLs and
// Terminal=false, the joined error wraps in *RecoverableVerifierError
// so the executor's on_fail handler can extract the structured
// signals and forward them to the recovery step.
func TestJoinVerifierErrorsWithBlocks_NonTerminalWithBlocksWrapsRecoverable(t *testing.T) {
	blocks := []verifier.BlockedURL{
		{URL: "https://www.reuters.com/world", Reason: "auth_required", Permanent: true},
	}
	got := joinVerifierErrorsWithBlocks(
		[]string{"verifier \"no_rate_limit\" (no_status_429_in_audit): 2/2 fetches blocked"},
		false, // Terminal=false: all blocks are permanent
		blocks,
	)
	if got == nil {
		t.Fatal("expected error; got nil")
	}
	if isTerminalVerifierError(got) {
		t.Fatal("permanent-only blocks must not wrap as Terminal")
	}
	var rve *RecoverableVerifierError
	if !errors.As(got, &rve) {
		t.Fatalf("expected *RecoverableVerifierError; got %T", got)
	}
	if len(rve.BlockedURLs) != 1 || rve.BlockedURLs[0].Reason != "auth_required" {
		t.Errorf("expected blocked URLs preserved on wrap; got %+v", rve.BlockedURLs)
	}
}

// TestJoinVerifierErrorsWithBlocks_TerminalTakesPrecedence — when
// Terminal=true wins, the Recoverable wrap is skipped: terminal
// short-circuits on_fail routing entirely, so the BlockedURLs would
// have no consumer.
func TestJoinVerifierErrorsWithBlocks_TerminalTakesPrecedence(t *testing.T) {
	blocks := []verifier.BlockedURL{
		{URL: "https://www.reuters.com/world", Reason: "auth_required", Permanent: true},
	}
	got := joinVerifierErrorsWithBlocks([]string{"v"}, true, blocks)
	if !isTerminalVerifierError(got) {
		t.Fatal("Terminal=true must produce TerminalVerifierError even with blocks")
	}
	var rve *RecoverableVerifierError
	if errors.As(got, &rve) {
		t.Fatal("Terminal must not also wrap as Recoverable")
	}
}

// TestJoinVerifierErrorsWithBlocks_NoBlocksFallsThrough — non-terminal
// + empty blocks returns the plain joined error (today's behaviour
// for verifiers that fail without producing structured blocks).
func TestJoinVerifierErrorsWithBlocks_NoBlocksFallsThrough(t *testing.T) {
	got := joinVerifierErrorsWithBlocks([]string{"v"}, false, nil)
	if got == nil {
		t.Fatal("nil")
	}
	if isTerminalVerifierError(got) {
		t.Fatal("must not be Terminal")
	}
	var rve *RecoverableVerifierError
	if errors.As(got, &rve) {
		t.Fatal("empty blocks must not wrap as Recoverable")
	}
}
