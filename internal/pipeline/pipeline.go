// Package pipeline is the ONE declaration of the named points at which the
// daemon's agent pipelines accept participants, and the three dispatch shapes
// a point can have. The gates that already existed — the intent judge, the
// output guard, the hallucination retry, the executor's step-outcome checks —
// were written in call order; registering them against a named point makes
// the order a declaration a test pins rather than a side effect of where a
// block sits in a function. Design of record:
// https://docs.vornik.io
//
// The package is a dependency-free leaf (stdlib only), so any layer may hold
// a chain. There is deliberately NO package-level registry of participants: a
// chain belongs to the owner that constructs it, a global would make test
// isolation depend on registration order across packages, and an
// agent-invocable registration path is what LLD 09 §13.7 forbids.
//
// Points are declared here and nowhere else. internal/contractreg lints every
// construction site against this list: a point invented at a call site, a
// constructor whose mode disagrees with the point's, a declared point nobody
// constructs, or a point constructed in two packages are all findings.
package pipeline

import (
	"context"
	"fmt"
	"log/slog"
)

// Mode says how a point dispatches to its participants. The type system holds
// a point to its mode: a Decide point cannot be given an observer, and a
// participant at an Around point cannot forget that it owns the continuation.
type Mode string

const (
	// ModeObserve — every participant runs; a participant's error is logged and
	// never returned, a panic is recovered. Telemetry must not take a step down.
	// Declared so a point can be given the mode; the Observe chain type itself
	// arrives with the first point that needs it — shipping it unreferenced
	// would fail the reachability gate, which is the right call.
	ModeObserve Mode = "observe"
	// ModeDecide — participants run in registration order; the FIRST refusal
	// stops the chain and is the chain's answer.
	ModeDecide Mode = "decide"
	// ModeAround — each participant receives the call and the continuation.
	// Participants nest in registration order (first registered is outermost);
	// returning without invoking the continuation refuses.
	ModeAround Mode = "around"
)

// Point is a named place in a pipeline. The name is the contract: it is what
// the lint checks, what a participant registers against, and what an operator
// would eventually select on.
type Point struct {
	Name string
	Mode Mode
}

// The closed set of points. Adding one is a design change: it is recorded in
// the pipeline-points design, appears here, and is constructed by exactly one
// owner. Points is the list the lint reads.
var (
	// DispatcherPreTool runs before the chat agent executes a tool call. A
	// refusal replaces the execution with a tool message carrying the reason.
	DispatcherPreTool = Point{Name: "dispatcher.pre_tool", Mode: ModeDecide}
	// DispatcherPostTool wraps the tool execution; a participant can rewrite
	// what the model sees without touching what the audit records.
	DispatcherPostTool = Point{Name: "dispatcher.post_tool", Mode: ModeAround}
	// DispatcherContinuation runs on a final (tool-less) reply and decides
	// whether the loop continues (Verdict.Retry), accepts with a banner
	// (Verdict.Banner), or stops (Refused).
	DispatcherContinuation = Point{Name: "dispatcher.continuation", Mode: ModeDecide}
	// ExecutorStepOutcome runs over an agent step's result before it is
	// accepted; the first refusal fails the step with its reason.
	ExecutorStepOutcome = Point{Name: "executor.step_outcome", Mode: ModeDecide}
)

// Points is the closed list, in the order the design documents them.
var Points = []Point{DispatcherPreTool, DispatcherPostTool, DispatcherContinuation, ExecutorStepOutcome}

// Lookup returns the declared point with the given name.
func Lookup(name string) (Point, bool) {
	for _, p := range Points {
		if p.Name == name {
			return p, true
		}
	}
	return Point{}, false
}

// Verdict is a Decide participant's answer.
type Verdict struct {
	// Refused stops the chain; Reason is the text the caller reports, and
	// participants own their wording.
	Refused bool
	Reason  string
	// Participant is filled in by the chain from the registered name;
	// participants never set it.
	Participant string
	// Retry and Banner are meaningful at DispatcherContinuation only: Retry is
	// a user turn to append before running the loop again, Banner is text to
	// prepend to an accepted reply. A Decide chain at any other point treats a
	// non-empty value as a participant bug — logged with the participant's
	// name, and the verdict read as "no refusal".
	Retry  string
	Banner string
}

// continuation is the only point whose verdicts may carry Retry or Banner.
func (p Point) allowsContinuationFields() bool { return p == DispatcherContinuation }

// Logger is what a chain reports observer failures and participant bugs to.
// nil means slog.Default().
type Logger interface {
	Warn(msg string, args ...any)
}

type slogLogger struct{}

func (slogLogger) Warn(msg string, args ...any) { slog.Warn(msg, args...) }

func pick(l Logger) Logger {
	if l == nil {
		return slogLogger{}
	}
	return l
}

func mustMode(p Point, want Mode, ctor string) {
	if p.Mode != want {
		panic(fmt.Sprintf("pipeline: %s constructs point %q, which is declared %s, not %s", ctor, p.Name, p.Mode, want))
	}
	if _, ok := Lookup(p.Name); !ok {
		panic(fmt.Sprintf("pipeline: %s constructs point %q, which is not declared in pipeline.Points", ctor, p.Name))
	}
}

func checkUnique(point Point, names []string, name string) {
	if name == "" {
		panic(fmt.Sprintf("pipeline: %s: a participant needs a name", point.Name))
	}
	for _, n := range names {
		if n == name {
			panic(fmt.Sprintf("pipeline: %s: participant %q registered twice", point.Name, name))
		}
	}
}

// ------------------------------------------------------------------ Decide

// Decide is an ordered decision chain over In.
type Decide[In any] struct {
	point  Point
	logger Logger
	names  []string
	fns    []func(ctx context.Context, in In) Verdict
}

// NewDecide constructs the chain for p. Panics unless p is declared with
// ModeDecide — the lint says this before a daemon boots; the panic is the
// fallback for a tree built without it.
func NewDecide[In any](p Point, logger Logger) *Decide[In] {
	mustMode(p, ModeDecide, "NewDecide")
	return &Decide[In]{point: p, logger: pick(logger)}
}

// Register appends a participant. Registration order is dispatch order; a
// second participant under an existing name panics at construction time.
func (c *Decide[In]) Register(name string, fn func(ctx context.Context, in In) Verdict) {
	checkUnique(c.point, c.names, name)
	c.names = append(c.names, name)
	c.fns = append(c.fns, fn)
}

// Run dispatches in registration order and returns the first refusal, or —
// at the continuation point — the first verdict carrying Retry or Banner.
// A verdict with none of those from every participant is Verdict{}.
func (c *Decide[In]) Run(ctx context.Context, in In) Verdict {
	for i, fn := range c.fns {
		v := fn(ctx, in)
		v.Participant = c.names[i]
		if !c.point.allowsContinuationFields() && (v.Retry != "" || v.Banner != "") {
			c.logger.Warn("pipeline: participant returned a continuation-only field at a non-continuation point; reading it as no refusal",
				"point", c.point.Name, "participant", c.names[i])
			v.Retry, v.Banner = "", ""
		}
		if v.Refused || v.Retry != "" || v.Banner != "" {
			return v
		}
	}
	return Verdict{}
}

// Participants returns the registered names in dispatch order. Tests pin it.
func (c *Decide[In]) Participants() []string { return append([]string(nil), c.names...) }

// ------------------------------------------------------------------ Around

// Next is the continuation an Around participant receives.
type Next[In, Out any] func(ctx context.Context, in In) (Out, error)

// Around wraps a call: each participant receives the input and the
// continuation, and owns whether the continuation runs.
type Around[In, Out any] struct {
	point  Point
	logger Logger
	names  []string
	fns    []func(ctx context.Context, in In, next Next[In, Out]) (Out, error)
}

// NewAround constructs the chain for p. Panics unless p is declared ModeAround.
func NewAround[In, Out any](p Point, logger Logger) *Around[In, Out] {
	mustMode(p, ModeAround, "NewAround")
	return &Around[In, Out]{point: p, logger: pick(logger)}
}

// Register appends a participant. The first registered is outermost.
func (c *Around[In, Out]) Register(name string, fn func(ctx context.Context, in In, next Next[In, Out]) (Out, error)) {
	checkUnique(c.point, c.names, name)
	c.names = append(c.names, name)
	c.fns = append(c.fns, fn)
}

// Run nests the participants around terminal, first registered outermost,
// and returns what the outermost participant returns. With no participants
// it is terminal itself.
func (c *Around[In, Out]) Run(ctx context.Context, in In, terminal Next[In, Out]) (Out, error) {
	next := terminal
	for i := len(c.fns) - 1; i >= 0; i-- {
		fn, inner := c.fns[i], next
		next = func(ctx context.Context, in In) (Out, error) { return fn(ctx, in, inner) }
	}
	return next(ctx, in)
}

// Participants returns the registered names, outermost first.
func (c *Around[In, Out]) Participants() []string { return append([]string(nil), c.names...) }
