package dispatcher

import (
	"context"
	"errors"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// stubLinkRepo lets the resolver test drive each branch without
// a live DB. Returns a configurable answer per channel speaker
// id; absence of an entry returns ErrNotFound.
type stubLinkRepo struct {
	links map[string]string
	err   error
}

func (s *stubLinkRepo) Get(_ context.Context, channelSpeakerID string) (*persistence.OperatorIdentityLink, error) {
	if s.err != nil {
		return nil, s.err
	}
	id, ok := s.links[channelSpeakerID]
	if !ok {
		return nil, persistence.ErrNotFound
	}
	return &persistence.OperatorIdentityLink{ChannelSpeakerID: channelSpeakerID, OperatorID: id}, nil
}
func (s *stubLinkRepo) ListForOperator(_ context.Context, _ string) ([]*persistence.OperatorIdentityLink, error) {
	return nil, nil
}
func (s *stubLinkRepo) Upsert(_ context.Context, _ *persistence.OperatorIdentityLink) error {
	return nil
}
func (s *stubLinkRepo) Delete(_ context.Context, _ string) error               { return nil }
func (s *stubLinkRepo) DeleteAllForOperator(_ context.Context, _ string) error { return nil }

func TestAgentResolveCanonicalOperatorID_PassThrough_NoRepo(t *testing.T) {
	a := &Agent{logger: zerolog.Nop()}
	if got := a.resolveCanonicalOperatorID(context.Background(), "tg:42"); got != "tg:42" {
		t.Errorf("got %q, want %q (no repo → speaker id verbatim)", got, "tg:42")
	}
}

func TestAgentResolveCanonicalOperatorID_PassThrough_NotLinked(t *testing.T) {
	a := &Agent{
		logger:                zerolog.Nop(),
		operatorIdentityLinks: &stubLinkRepo{links: map[string]string{}},
	}
	if got := a.resolveCanonicalOperatorID(context.Background(), "tg:42"); got != "tg:42" {
		t.Errorf("got %q, want %q (no link → speaker id verbatim)", got, "tg:42")
	}
}

func TestAgentResolveCanonicalOperatorID_Resolves(t *testing.T) {
	a := &Agent{
		logger:                zerolog.Nop(),
		operatorIdentityLinks: &stubLinkRepo{links: map[string]string{"tg:42": "web:abc"}},
	}
	if got := a.resolveCanonicalOperatorID(context.Background(), "tg:42"); got != "web:abc" {
		t.Errorf("got %q, want %q", got, "web:abc")
	}
}

func TestAgentResolveCanonicalOperatorID_TolerantOfDBError(t *testing.T) {
	a := &Agent{
		logger:                zerolog.Nop(),
		operatorIdentityLinks: &stubLinkRepo{err: errors.New("db blip")},
	}
	if got := a.resolveCanonicalOperatorID(context.Background(), "tg:42"); got != "tg:42" {
		t.Errorf("got %q, want %q (DB error → speaker id verbatim)", got, "tg:42")
	}
}

func TestAgentResolveCanonicalOperatorID_EmptyInput(t *testing.T) {
	a := &Agent{
		logger:                zerolog.Nop(),
		operatorIdentityLinks: &stubLinkRepo{links: map[string]string{"": "x"}},
	}
	if got := a.resolveCanonicalOperatorID(context.Background(), ""); got != "" {
		t.Errorf("empty input must return empty without consulting repo, got %q", got)
	}
}

func TestToolExecutorResolveCanonicalOperatorID(t *testing.T) {
	te := &ToolExecutor{
		logger:                zerolog.Nop(),
		operatorIdentityLinks: &stubLinkRepo{links: map[string]string{"tg:42": "web:abc"}},
	}
	if got := te.resolveCanonicalOperatorID(context.Background(), "tg:42"); got != "web:abc" {
		t.Errorf("ToolExecutor resolver got %q, want %q", got, "web:abc")
	}
}
