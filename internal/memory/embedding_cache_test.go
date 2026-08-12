package memory

import (
	"context"
	"errors"
	"regexp"
	"sync"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestContentHash_Deterministic(t *testing.T) {
	h1 := ContentHash("hello world")
	h2 := ContentHash("hello world")
	if h1 != h2 {
		t.Errorf("hash diverges: %q vs %q", h1, h2)
	}
	if h1 == ContentHash("hello world!") {
		t.Error("hash should differ on different input")
	}
	if len(h1) != 64 {
		t.Errorf("expected 64-char hex sha256, got %d chars", len(h1))
	}
}

func TestEmbeddingCacheRepo_GetMissReturnsFalseNoError(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	cache := &embeddingCacheRepo{db: db}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT embedding::text FROM embedding_cache")).
		WithArgs("h", "m").
		WillReturnRows(sqlmock.NewRows([]string{"embedding"}))
	vec, ok, err := cache.Get(context.Background(), "h", "m")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if ok || vec != nil {
		t.Errorf("miss returned ok=%v vec=%v", ok, vec)
	}
}

func TestEmbeddingCacheRepo_GetEmptyKeyShortCircuits(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	cache := &embeddingCacheRepo{db: db}
	if _, ok, _ := cache.Get(context.Background(), "", "m"); ok {
		t.Error("empty hash should miss")
	}
	if _, ok, _ := cache.Get(context.Background(), "h", ""); ok {
		t.Error("empty model should miss")
	}
}

func TestEmbeddingCacheRepo_PutEmptyVecNoop(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	cache := &embeddingCacheRepo{db: db}
	if err := cache.Put(context.Background(), "h", "m", nil); err != nil {
		t.Errorf("err: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected SQL calls: %v", err)
	}
}

func TestEmbeddingCacheRepo_PutUpserts(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	cache := &embeddingCacheRepo{db: db}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO embedding_cache")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := cache.Put(context.Background(), "h", "m", []float32{0.1, 0.2}); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql: %v", err)
	}
}

func TestEmbeddingCacheRepo_PutErrorPropagates(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	cache := &embeddingCacheRepo{db: db}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO embedding_cache")).
		WillReturnError(errors.New("db down"))
	if err := cache.Put(context.Background(), "h", "m", []float32{1}); err == nil {
		t.Fatal("expected error to propagate")
	}
}

// --- in-memory test fake exercising the Embedder's cache flow ----

type fakeEmbedCache struct {
	evicted []string
	mu      sync.Mutex
	data    map[string][]float32
	gets    int
	puts    int
}

func newFakeEmbedCache() *fakeEmbedCache {
	return &fakeEmbedCache{data: map[string][]float32{}}
}

func (f *fakeEmbedCache) Get(_ context.Context, hash, model string) ([]float32, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	v, ok := f.data[hash+":"+model]
	if !ok {
		return nil, false, nil
	}
	return v, true, nil
}

func (f *fakeEmbedCache) Put(_ context.Context, hash, model string, vec []float32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts++
	f.data[hash+":"+model] = append([]float32(nil), vec...)
	return nil
}

func TestEmbedder_CacheHitsShortCircuit(t *testing.T) {
	// Embedder with cache pre-populated for one input. The other
	// input is a miss; only one upstream request should fire (for
	// the miss). The HTTP endpoint is set to "" so embedBatch
	// short-circuits to nil — we want to assert the cache lookup
	// happened without standing up a fake server.
	//
	// Actually, with EmbeddingEndpoint="" the outer Embed returns
	// early before cache logic. To exercise the cache path we need
	// EmbeddingEndpoint non-empty. We can't easily stand up an
	// HTTP server here; instead we verify the cache short-circuit
	// at the "all hits" boundary: every input is cached, so Embed
	// must return results WITHOUT touching the endpoint.
	cache := newFakeEmbedCache()
	cache.data["a:test-model"] = []float32{1, 2, 3}
	cache.data["b:test-model"] = []float32{4, 5, 6}
	// Make sure ContentHash matches what the embedder will compute.
	cache.data[ContentHash("a")+":test-model"] = []float32{1, 2, 3}
	cache.data[ContentHash("b")+":test-model"] = []float32{4, 5, 6}

	e := &Embedder{
		cfg: Config{
			EmbeddingEndpoint: "http://localhost:0", // would fail if reached
			EmbeddingModel:    "test-model",
		},
		Cache: cache,
	}
	out, err := e.Embed(context.Background(), EmbedScope{ProjectID: "p1", CallSite: EmbedCallSiteIngest}, []string{"a", "b"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d vecs, want 2", len(out))
	}
	if len(out[0]) != 3 || out[0][0] != 1 {
		t.Errorf("vec[0] = %v, want [1 2 3]", out[0])
	}
	if len(out[1]) != 3 || out[1][0] != 4 {
		t.Errorf("vec[1] = %v, want [4 5 6]", out[1])
	}
	if cache.gets < 2 {
		t.Errorf("expected at least 2 cache lookups, got %d", cache.gets)
	}
	// All hits → no upstream calls → no Puts.
	if cache.puts != 0 {
		t.Errorf("expected 0 cache puts (all hits), got %d", cache.puts)
	}
}

func TestEmbedder_NoCacheNoCacheCall(t *testing.T) {
	// Cache=nil + endpoint="" → Embed returns nil, nil; no panic.
	e := &Embedder{cfg: Config{EmbeddingModel: "test-model"}}
	out, err := e.Embed(context.Background(), EmbedScope{ProjectID: "p1", CallSite: EmbedCallSiteIngest}, []string{"x"})
	if err != nil {
		t.Errorf("err: %v", err)
	}
	if out != nil {
		t.Errorf("expected nil result with empty endpoint, got %v", out)
	}
}

func TestEmbedder_CacheRequiresModel(t *testing.T) {
	// Without an embedding model, hashes can't be keyed against a
	// model namespace; the cache layer must be skipped. Use
	// NewEmbedder so the http.Client is initialised (the test
	// won't ever reach the network because EmbeddingEndpoint is
	// empty too — the outer Embed short-circuits).
	cache := newFakeEmbedCache()
	e := NewEmbedder(Config{})
	e.Cache = cache
	if _, err := e.Embed(context.Background(), EmbedScope{ProjectID: "p1", CallSite: EmbedCallSiteIngest}, []string{"x"}); err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if cache.gets != 0 {
		t.Errorf("expected zero cache lookups when EmbeddingModel is empty, got %d", cache.gets)
	}
}

// Smoke test that NewEmbeddingCache returns nil when given nil db.
func TestNewEmbeddingCache_NilDB(t *testing.T) {
	if NewEmbeddingCache(nil) != nil {
		t.Error("nil db should yield nil cache (no-op behaviour)")
	}
}

// --- eviction on erasure (GDPR Art 17) ---
//
// Found while verifying the 5c redaction design: EmbedCache had Get and Put only,
// so the vector derived from a subject's pre-erasure text survived every erasure
// path — 5b's DeleteRow, the artifact cascade, and redaction. The cache holds no
// plaintext and is not similarity-searchable, but it is data DERIVED from the
// subject's personal data, keyed by a hash that confirms the exact source text.
// The parent design already treats a surviving derived embedding as the thing to
// fix; that is why the artifact cascade exists.
//
// The retention sweeper prunes this table by last_hit_at age and only when
// retention.embedding_cache_days is set. Time-based cold-entry pruning is not
// compliance with an erasure request.

func TestEvictAll_RemovesEveryModelsVectorForOneContent(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM embedding_cache WHERE content_hash = $1")).
		WithArgs("abc123").
		WillReturnResult(sqlmock.NewResult(0, 2))

	repo := NewEmbeddingCache(db)
	if err := repo.EvictAll(context.Background(), "abc123"); err != nil {
		t.Fatalf("EvictAll: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// Model-scoped eviction, for the case where only one model's vector must go.
func TestEvict_RemovesOneModelsVector(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectExec(regexp.QuoteMeta(
		"DELETE FROM embedding_cache WHERE content_hash = $1 AND model = $2")).
		WithArgs("abc123", "bge-m3").
		WillReturnResult(sqlmock.NewResult(0, 1))

	repo := NewEmbeddingCache(db)
	if err := repo.Evict(context.Background(), "abc123", "bge-m3"); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

// An empty hash must not become "delete the whole cache". Same reasoning as
// DeleteProjectArtifacts refusing an empty project id.
func TestEvict_RefusesAnEmptyHash(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	repo := NewEmbeddingCache(db)

	if err := repo.EvictAll(context.Background(), "  "); err == nil {
		t.Error("an empty content hash must be refused — it would wipe the cache")
	}
	if err := repo.Evict(context.Background(), "", "bge-m3"); err == nil {
		t.Error("an empty content hash must be refused")
	}
}

// Eviction must be idempotent: erasure paths retry, and a hash that is already
// absent is the expected steady state on a second run.
func TestEvictAll_AbsentHashIsNotAnError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM embedding_cache WHERE content_hash = $1")).
		WithArgs("never-cached").
		WillReturnResult(sqlmock.NewResult(0, 0))

	repo := NewEmbeddingCache(db)
	if err := repo.EvictAll(context.Background(), "never-cached"); err != nil {
		t.Fatalf("an absent hash must not error: %v", err)
	}
}

// Evict / EvictAll satisfy EmbedCache for the Art 17 erasure path. The fakes
// record the call so a test can assert eviction happened; the embedder itself
// never evicts.
func (f *fakeEmbedCache) Evict(_ context.Context, hash, model string) error {
	f.evicted = append(f.evicted, hash+"/"+model)
	return nil
}

func (f *fakeEmbedCache) EvictAll(_ context.Context, hash string) error {
	f.evicted = append(f.evicted, hash)
	return nil
}
