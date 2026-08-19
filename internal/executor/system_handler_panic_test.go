package executor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// panickingHandler stands in for any system-step handler with a latent bug.
type panickingHandler struct{}

func (panickingHandler) Name() string { return "rag.index" }

func (panickingHandler) Execute(context.Context, SystemStepInput) (SystemStepResult, error) {
	var doc *struct{ StoragePath string }
	_ = doc.StoragePath // the exact shape of the 2026-08-19 rag.index crash
	return SystemStepResult{}, nil
}

// Regression: 2026-08-19. A nil dereference inside rag.index escaped the step
// goroutine and killed the daemon — 28 crash-loops in ten minutes on bench, and
// it would have done the same in production had any project run that workflow.
//
// The specific nil is now guarded, but the structural fault is that ONE
// workflow handler's bug can take down every other project's work. A step
// handler is the right blast radius for a handler bug: the step fails, on_fail
// routes, and the daemon keeps serving.
//
// This is the barrier, not the specific fix. It must convert a panic into an
// ordinary step error carrying enough to find the bug.
func TestRunSystemHandler_PanicBecomesStepErrorNotDaemonDeath(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("the panic escaped the barrier (%v) — a handler bug must fail the "+
				"step, not the process", r)
		}
	}()

	_, err := runSystemHandlerSafely(context.Background(), panickingHandler{}, "rag.index",
		SystemStepInput{})
	if err == nil {
		t.Fatal("expected an error describing the panic, got nil")
	}
	if !strings.Contains(err.Error(), "rag.index") {
		t.Errorf("error must name the handler so the bug is findable; got: %v", err)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "panic") {
		t.Errorf("error must say it was a panic, so it is not mistaken for an ordinary "+
			"handler rejection; got: %v", err)
	}
}

// The barrier must be transparent when nothing panics: a handler's own error and
// result have to pass through unchanged, or it would mask real outcomes.
func TestRunSystemHandler_PassesThroughNormalOutcomes(t *testing.T) {
	want := errors.New("handler said no")
	_, err := runSystemHandlerSafely(context.Background(), errHandler{want}, "x", SystemStepInput{})
	if !errors.Is(err, want) {
		t.Errorf("handler error must pass through unwrapped-enough to match; got %v", err)
	}

	res, err := runSystemHandlerSafely(context.Background(),
		okHandler{SystemStepResult{Result: []byte(`{"ok":true}`)}}, "x", SystemStepInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(res.Result) != `{"ok":true}` {
		t.Errorf("result must pass through unchanged; got %q", res.Result)
	}
}

type errHandler struct{ err error }

func (h errHandler) Name() string { return "x" }

func (h errHandler) Execute(context.Context, SystemStepInput) (SystemStepResult, error) {
	return SystemStepResult{}, h.err
}

type okHandler struct{ res SystemStepResult }

func (h okHandler) Name() string { return "x" }

func (h okHandler) Execute(context.Context, SystemStepInput) (SystemStepResult, error) {
	return h.res, nil
}
