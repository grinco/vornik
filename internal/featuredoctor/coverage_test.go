package featuredoctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// stubInstinctRepo stubs persistence.InstinctRepository for verify tests.
type stubInstinctRepo struct {
	counts []persistence.InstinctDomainStatusCount
	err    error
}

func (s *stubInstinctRepo) CountByDomainStatus(ctx context.Context) ([]persistence.InstinctDomainStatusCount, error) {
	return s.counts, s.err
}

// Unused methods — satisfy interface.
func (s *stubInstinctRepo) Upsert(ctx context.Context, in *persistence.Instinct) (string, error) {
	return "", nil
}
func (s *stubInstinctRepo) AddEvidence(ctx context.Context, ev *persistence.InstinctEvidence) (bool, error) {
	return false, nil
}

func (s *stubInstinctRepo) RecordActionVersion(context.Context, *persistence.InstinctActionVersion) error {
	return nil
}

func (s *stubInstinctRepo) ListActionHistory(context.Context, string, int) ([]*persistence.InstinctActionVersion, error) {
	return nil, nil
}
func (s *stubInstinctRepo) RecomputeConfidence(ctx context.Context, id string, scorer persistence.InstinctScorer) error {
	return nil
}
func (s *stubInstinctRepo) Get(ctx context.Context, id string) (*persistence.Instinct, error) {
	return nil, nil
}
func (s *stubInstinctRepo) List(ctx context.Context, f persistence.InstinctFilter) ([]*persistence.Instinct, error) {
	return nil, nil
}
func (s *stubInstinctRepo) CountActiveProjects(ctx context.Context, triggerKey string) (int, error) {
	return 0, nil
}
func (s *stubInstinctRepo) Retire(ctx context.Context, id string) error { return nil }
func (s *stubInstinctRepo) RecordApplication(ctx context.Context, app *persistence.InstinctApplication) error {
	return nil
}
func (s *stubInstinctRepo) ListApplications(ctx context.Context, id string, limit int) ([]*persistence.InstinctApplication, error) {
	return nil, nil
}
func (s *stubInstinctRepo) ListPendingRecoveryApplications(ctx context.Context, limit int) ([]*persistence.InstinctApplication, error) {
	return nil, nil
}
func (s *stubInstinctRepo) ResolveApplication(ctx context.Context, id string, result string) error {
	return nil
}
func (s *stubInstinctRepo) ListApplicationCounts(ctx context.Context, ids []string) (map[string]*persistence.InstinctApplicationCounts, error) {
	return nil, nil
}

// --- instinct Verify coverage ---

func TestInstinctVerify_NilRepo(t *testing.T) {
	f := instinctFeature()
	deps := Deps{Instincts: nil}
	res := f.Verify(context.Background(), deps)
	if res.OK {
		t.Fatal("nil instinct repo must be unmet")
	}
}

func TestInstinctVerify_QueryError(t *testing.T) {
	f := instinctFeature()
	deps := Deps{Instincts: &stubInstinctRepo{err: errors.New("db down")}}
	res := f.Verify(context.Background(), deps)
	if res.OK {
		t.Fatal("query error must be unmet")
	}
}

func TestInstinctVerify_NoInstincts(t *testing.T) {
	f := instinctFeature()
	deps := Deps{Instincts: &stubInstinctRepo{counts: nil}}
	res := f.Verify(context.Background(), deps)
	if res.OK {
		t.Fatal("empty counts must be unmet")
	}
}

func TestInstinctVerify_HasInstincts(t *testing.T) {
	f := instinctFeature()
	deps := Deps{Instincts: &stubInstinctRepo{counts: []persistence.InstinctDomainStatusCount{{Domain: "test", Status: "active", Count: 1}}}}
	res := f.Verify(context.Background(), deps)
	if !res.OK {
		t.Fatalf("non-empty counts must be met, got %+v", res)
	}
}

func TestInstinctPrereq_ModelReachable_OK(t *testing.T) {
	f := instinctFeature()
	deps := Deps{
		Config: stubConfig{vals: map[string]any{"instinct.model": "qwen3.6:35b"}},
		Models: stubModels{reachable: true},
	}
	var modelCheck *Prereq
	for i := range f.Prereqs {
		if f.Prereqs[i].Name == "distill model reachable" {
			modelCheck = &f.Prereqs[i]
		}
	}
	res := modelCheck.Check(context.Background(), deps)
	if !res.OK {
		t.Fatalf("reachable model must be met, got %+v", res)
	}
}

func TestInstinctPrereq_NoModel(t *testing.T) {
	f := instinctFeature()
	deps := Deps{
		Config: stubConfig{vals: map[string]any{}},
	}
	var modelCheck *Prereq
	for i := range f.Prereqs {
		if f.Prereqs[i].Name == "distill model reachable" {
			modelCheck = &f.Prereqs[i]
		}
	}
	res := modelCheck.Check(context.Background(), deps)
	if !res.OK {
		t.Fatalf("no model set must be met (falls back to chat.model), got %+v", res)
	}
}

func TestInstinctPrereq_ModelSetButPingerNil(t *testing.T) {
	// When a model id is set but Models pinger is nil, Detail must say "not wired"
	// (distinct from "not reachable") and the result must be not-OK, not-fixable.
	f := instinctFeature()
	deps := Deps{
		Config: stubConfig{vals: map[string]any{"instinct.model": "qwen3.6:35b"}},
		Models: nil,
	}
	var modelCheck *Prereq
	for i := range f.Prereqs {
		if f.Prereqs[i].Name == "distill model reachable" {
			modelCheck = &f.Prereqs[i]
		}
	}
	res := modelCheck.Check(context.Background(), deps)
	if res.OK {
		t.Fatal("nil pinger + model id must be unmet")
	}
	if res.Fixable {
		t.Fatal("nil pinger must be unfixable")
	}
	if res.Detail == "" || res.Detail == "qwen3.6:35b not reachable" {
		t.Fatalf("nil pinger Detail must say 'not wired', got %q", res.Detail)
	}
}

func TestInstinctPrereq_EnabledTrue(t *testing.T) {
	f := instinctFeature()
	deps := Deps{Config: stubConfig{vals: map[string]any{"instinct.enabled": true}}}
	var dep *Prereq
	for i := range f.Prereqs {
		if f.Prereqs[i].Name == "instinct.enabled set" {
			dep = &f.Prereqs[i]
		}
	}
	res := dep.Check(context.Background(), deps)
	if !res.OK {
		t.Fatalf("instinct.enabled=true must be met, got %+v", res)
	}
}

// --- auth Verify coverage ---

func TestAuthVerify_KeyPresent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "admin-key.txt"), []byte("VORNIK_ADMIN_KEY=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := authFeature()
	res := f.Verify(context.Background(), Deps{SecretsDir: dir})
	if !res.OK {
		t.Fatalf("key present must be met, got %+v", res)
	}
}

func TestAuthVerify_KeyMissing(t *testing.T) {
	dir := t.TempDir()
	f := authFeature()
	res := f.Verify(context.Background(), Deps{SecretsDir: dir})
	if res.OK {
		t.Fatal("missing key must be unmet")
	}
}

// --- memory-rag Verify coverage ---

func TestMemoryRAGVerify_GatesCoherent(t *testing.T) {
	f := memoryRAGFeature()
	res := f.Verify(context.Background(), Deps{})
	if !res.OK {
		t.Fatalf("memory-rag verify always ok, got %+v", res)
	}
}

func TestMemoryRAGPrereq_EmbeddingModelUnset(t *testing.T) {
	f := memoryRAGFeature()
	deps := Deps{
		Config: stubConfig{vals: map[string]any{}},
	}
	var emb *Prereq
	for i := range f.Prereqs {
		if f.Prereqs[i].Name == "embedding model reachable" {
			emb = &f.Prereqs[i]
		}
	}
	res := emb.Check(context.Background(), deps)
	if !res.OK {
		t.Fatalf("unset embedding model must be met (uses default), got %+v", res)
	}
}

func TestMemoryRAGPrereq_EmbeddingModelReachable(t *testing.T) {
	f := memoryRAGFeature()
	deps := Deps{
		Config: stubConfig{vals: map[string]any{"memory.embedding_model": "nomic-embed"}},
		Models: stubModels{reachable: true},
	}
	var emb *Prereq
	for i := range f.Prereqs {
		if f.Prereqs[i].Name == "embedding model reachable" {
			emb = &f.Prereqs[i]
		}
	}
	res := emb.Check(context.Background(), deps)
	if !res.OK {
		t.Fatalf("reachable embedding model must be met, got %+v", res)
	}
}

func TestMemoryRAGPrereq_ModelSetButPingerNil(t *testing.T) {
	// When a model id is set but Models pinger is nil, Detail must say "not wired"
	// (distinct from "not reachable") and the result must be not-OK, not-fixable.
	f := memoryRAGFeature()
	deps := Deps{
		Config: stubConfig{vals: map[string]any{"memory.embedding_model": "nomic-embed"}},
		Models: nil,
	}
	var emb *Prereq
	for i := range f.Prereqs {
		if f.Prereqs[i].Name == "embedding model reachable" {
			emb = &f.Prereqs[i]
		}
	}
	res := emb.Check(context.Background(), deps)
	if res.OK {
		t.Fatal("nil pinger + model id must be unmet")
	}
	if res.Fixable {
		t.Fatal("nil pinger must be unfixable")
	}
	if res.Detail == "" || res.Detail == "nomic-embed not reachable" {
		t.Fatalf("nil pinger Detail must say 'not wired', got %q", res.Detail)
	}
}

// --- gatesOn coverage ---

func TestGatesOn_AllMatch(t *testing.T) {
	f := authFeature()
	cfg := stubConfig{vals: map[string]any{"api.auth_enabled": true}}
	if !gatesOn(f, cfg) {
		t.Fatal("all gates matching must return true")
	}
}

func TestGatesOn_KeyMissing(t *testing.T) {
	f := authFeature()
	cfg := stubConfig{vals: map[string]any{}}
	if gatesOn(f, cfg) {
		t.Fatal("missing gate key must return false")
	}
}

// --- Diagnose with gates on ---

func TestDiagnose_OKWhenGatesOnAndVerifyPasses(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "admin-key.txt"), []byte("VORNIK_ADMIN_KEY=x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := authFeature()
	deps := Deps{
		Config:     stubConfig{vals: map[string]any{"api.auth_enabled": true}},
		SecretsDir: dir,
	}
	d := Diagnose(context.Background(), f, deps)
	if d.Status != StatusOK {
		t.Fatalf("auth on + key present => ok, got %q", d.Status)
	}
}
