package chat

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// frecProvider is an inner Provider that records calls so the
// free-only guard tests can assert the network was (or wasn't) reached.
// It optionally implements Pinger / ModelLister via the wrapper types
// below.
type frecProvider struct {
	model        string
	completeHits int
	pingHits     int
	listHits     int
}

func (r *frecProvider) Complete(_ context.Context, _ []Message) (*ChatResponse, error) {
	r.completeHits++
	return &ChatResponse{Model: r.model}, nil
}

func (r *frecProvider) CompleteWithTools(_ context.Context, _ []Message, _ []Tool) (*ChatResponse, error) {
	r.completeHits++
	return &ChatResponse{Model: r.model}, nil
}

func (r *frecProvider) CompleteWithToolsStream(_ context.Context, _ []Message, _ []Tool, _ StreamCallback) (*ChatResponse, error) {
	r.completeHits++
	return &ChatResponse{Model: r.model}, nil
}

func (r *frecProvider) Model() string         { return r.model }
func (r *frecProvider) SetMetrics(_ *Metrics) {}

func (r *frecProvider) WithModel(m string) Provider {
	clone := *r
	clone.model = m
	return &clone
}

func (r *frecProvider) Ping(_ context.Context) error {
	r.pingHits++
	return nil
}

func (r *frecProvider) ListModels(_ context.Context) ([]ModelInfo, error) {
	r.listHits++
	return []ModelInfo{{ID: r.model}}, nil
}

// TestFreeOnly_RejectsNonFreeWithoutNetwork is the core guard: a non-:free
// model is rejected with a typed error and the inner provider is never
// called.
func TestFreeOnly_RejectsNonFreeWithoutNetwork(t *testing.T) {
	inner := &frecProvider{model: "openai/gpt-4o"}
	p := NewFreeOnlyProvider(inner)

	for _, name := range []string{"Complete", "CompleteWithTools", "Stream"} {
		t.Run(name, func(t *testing.T) {
			var err error
			switch name {
			case "Complete":
				_, err = p.Complete(context.Background(), nil)
			case "CompleteWithTools":
				_, err = p.CompleteWithTools(context.Background(), nil, nil)
			case "Stream":
				_, err = p.CompleteWithToolsStream(context.Background(), nil, nil, nil)
			}
			require.Error(t, err)
			var nf *ErrNonFreeModel
			require.True(t, errors.As(err, &nf), "want *ErrNonFreeModel, got %T", err)
			assert.Equal(t, "openai/gpt-4o", nf.Model)
		})
	}
	assert.Zero(t, inner.completeHits, "inner provider must not be called for a non-free model")
}

// TestFreeOnly_AllowsFreeModel verifies a :free model passes straight
// through to the inner provider on all three completion entry points.
func TestFreeOnly_AllowsFreeModel(t *testing.T) {
	inner := &frecProvider{model: "deepseek/deepseek-r1:free"}
	p := NewFreeOnlyProvider(inner)

	_, err := p.Complete(context.Background(), nil)
	require.NoError(t, err)
	resp, err := p.CompleteWithTools(context.Background(), nil, nil)
	require.NoError(t, err)
	assert.Equal(t, "deepseek/deepseek-r1:free", resp.Model)
	_, err = p.CompleteWithToolsStream(context.Background(), nil, nil, nil)
	require.NoError(t, err)
	assert.Equal(t, 3, inner.completeHits)
}

// TestErrNonFreeModel_Error verifies the error message names the offending
// model and the :free convention.
func TestErrNonFreeModel_Error(t *testing.T) {
	err := &ErrNonFreeModel{Model: "openai/gpt-4o"}
	msg := err.Error()
	assert.Contains(t, msg, "openai/gpt-4o")
	assert.Contains(t, msg, ":free")
}

// TestFreeOnly_WithModel_PlainInner verifies WithModel on a wrapper whose
// inner is not ModelOverridable returns a guard around the unchanged
// inner (model can't move, but the guard survives).
func TestFreeOnly_WithModel_PlainInner(t *testing.T) {
	p := NewFreeOnlyProvider(&fplainProvider{model: "x:free"})
	pinned := p.(ModelOverridable).WithModel("anything")
	assert.Equal(t, "x:free", pinned.Model(), "non-overridable inner keeps its model")
	_, err := pinned.CompleteWithTools(context.Background(), nil, nil)
	require.NoError(t, err, ":free inner still allowed through the re-wrapped guard")
}

// TestFreeOnly_WithModelPropagatesGuard verifies the guard survives a
// per-request model override (the router's dispatch path): WithModel
// returns another guarded provider pinned to the requested model.
func TestFreeOnly_WithModelPropagatesGuard(t *testing.T) {
	inner := &frecProvider{model: "deepseek/deepseek-r1:free"}
	p := NewFreeOnlyProvider(inner)

	// Override to a non-free model — the returned provider must still
	// guard and reject at call time.
	pinned := p.(ModelOverridable).WithModel("openai/gpt-4o")
	assert.Equal(t, "openai/gpt-4o", pinned.Model())
	_, err := pinned.CompleteWithTools(context.Background(), nil, nil)
	var nf *ErrNonFreeModel
	require.True(t, errors.As(err, &nf))

	// Override to a free model — allowed.
	pinnedFree := p.(ModelOverridable).WithModel("qwen/qwq-32b:free")
	_, err = pinnedFree.CompleteWithTools(context.Background(), nil, nil)
	require.NoError(t, err)
}

// TestFreeOnly_PassThroughAccessors verifies Model / SetMetrics and the
// optional Pinger / ModelLister surfaces delegate to the inner provider
// (so router enumeration + readiness keep working through the wrapper).
func TestFreeOnly_PassThroughAccessors(t *testing.T) {
	inner := &frecProvider{model: "x:free"}
	p := NewFreeOnlyProvider(inner)

	assert.Equal(t, "x:free", p.Model())
	p.SetMetrics(nil) // no panic

	pg, ok := p.(Pinger)
	require.True(t, ok, "wrapper must expose Pinger when inner does")
	require.NoError(t, pg.Ping(context.Background()))
	assert.Equal(t, 1, inner.pingHits)

	ml, ok := p.(ModelLister)
	require.True(t, ok, "wrapper must expose ModelLister when inner does")
	models, err := ml.ListModels(context.Background())
	require.NoError(t, err)
	assert.Len(t, models, 1)
	assert.Equal(t, 1, inner.listHits)
}

// fplainProvider implements only the base Provider interface (no Pinger /
// ModelLister) — used to verify the wrapper's optional surfaces degrade
// gracefully when the inner lacks them.
type fplainProvider struct{ model string }

func (p *fplainProvider) Complete(context.Context, []Message) (*ChatResponse, error) {
	return &ChatResponse{Model: p.model}, nil
}
func (p *fplainProvider) CompleteWithTools(context.Context, []Message, []Tool) (*ChatResponse, error) {
	return &ChatResponse{Model: p.model}, nil
}
func (p *fplainProvider) CompleteWithToolsStream(context.Context, []Message, []Tool, StreamCallback) (*ChatResponse, error) {
	return &ChatResponse{Model: p.model}, nil
}
func (p *fplainProvider) Model() string       { return p.model }
func (p *fplainProvider) SetMetrics(*Metrics) {}

// TestFreeOnly_GracefulDegradationForPlainInner verifies that when the
// inner provider doesn't implement Pinger / ModelLister, the wrapper's
// own implementations no-op (ready by construction, empty catalogue)
// rather than panic. The wrapper always satisfies the optional
// interfaces — in production the inner is always *Client, but the router
// type-asserts and we must not break those assertions.
func TestFreeOnly_GracefulDegradationForPlainInner(t *testing.T) {
	p := NewFreeOnlyProvider(&fplainProvider{model: "x:free"})

	pg, ok := p.(Pinger)
	require.True(t, ok)
	assert.NoError(t, pg.Ping(context.Background()), "plain inner → ready by construction")

	ml, ok := p.(ModelLister)
	require.True(t, ok)
	models, err := ml.ListModels(context.Background())
	require.NoError(t, err)
	assert.Empty(t, models, "plain inner → no catalogue")
}
