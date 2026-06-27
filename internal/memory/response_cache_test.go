package memory

import (
	"context"
	"errors"
	"regexp"
	"sync"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"vornik.io/vornik/internal/chat"
)

func TestResponseCacheKey_Deterministic(t *testing.T) {
	msgs := []chat.Message{
		{Role: "system", Content: "be brief"},
		{Role: "user", Content: "hello"},
	}
	k1 := ResponseCacheKey("m1", "purpose-a", msgs)
	k2 := ResponseCacheKey("m1", "purpose-a", msgs)
	if k1 != k2 {
		t.Fatalf("expected deterministic keys, got %q vs %q", k1, k2)
	}
	if len(k1) != 64 {
		t.Errorf("expected 64-char hex sha256, got %d", len(k1))
	}
}

func TestResponseCacheKey_SensitiveToInputs(t *testing.T) {
	base := []chat.Message{{Role: "user", Content: "hello"}}
	baseKey := ResponseCacheKey("m1", "p", base)

	cases := []struct {
		name    string
		mutated string
	}{
		{"different model", ResponseCacheKey("m2", "p", base)},
		{"different purpose", ResponseCacheKey("m1", "q", base)},
		{"different role", ResponseCacheKey("m1", "p", []chat.Message{{Role: "system", Content: "hello"}})},
		{"different content", ResponseCacheKey("m1", "p", []chat.Message{{Role: "user", Content: "world"}})},
		{"extra message", ResponseCacheKey("m1", "p", []chat.Message{
			{Role: "user", Content: "hello"},
			{Role: "user", Content: "world"},
		})},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.mutated == baseKey {
				t.Errorf("expected key to differ from base for %s", c.name)
			}
		})
	}
}

func TestResponseCacheRepo_GetMissReturnsFalseNoError(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	cache := &responseCacheRepo{db: db}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT response_content")).
		WithArgs("k").
		WillReturnRows(sqlmock.NewRows([]string{"response_content", "prompt_tokens", "completion_tokens"}))
	content, pt, ct, hit, err := cache.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if hit || content != "" || pt != 0 || ct != 0 {
		t.Errorf("miss returned hit=%v content=%q pt=%d ct=%d", hit, content, pt, ct)
	}
}

func TestResponseCacheRepo_GetEmptyKeyShortCircuits(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	cache := &responseCacheRepo{db: db}
	if _, _, _, hit, _ := cache.Get(context.Background(), ""); hit {
		t.Error("empty key should miss")
	}
}

func TestResponseCacheRepo_GetHitTouchesLastHit(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	cache := &responseCacheRepo{db: db}
	rows := sqlmock.NewRows([]string{"response_content", "prompt_tokens", "completion_tokens"}).
		AddRow("cached body", 100, 25)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT response_content")).
		WithArgs("k").WillReturnRows(rows)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE llm_response_cache")).
		WithArgs("k").WillReturnResult(sqlmock.NewResult(0, 1))

	content, pt, ct, hit, err := cache.Get(context.Background(), "k")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !hit {
		t.Fatal("expected hit")
	}
	if content != "cached body" || pt != 100 || ct != 25 {
		t.Errorf("unexpected hit shape: %q %d %d", content, pt, ct)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sql: %v", err)
	}
}

func TestResponseCacheRepo_PutEmptyContentNoop(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	cache := &responseCacheRepo{db: db}
	if err := cache.Put(context.Background(), "k", "m", "p", "", 0, 0); err != nil {
		t.Errorf("err: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected SQL: %v", err)
	}
}

func TestResponseCacheRepo_PutEmptyKeyNoop(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	cache := &responseCacheRepo{db: db}
	if err := cache.Put(context.Background(), "", "m", "p", "body", 0, 0); err != nil {
		t.Errorf("err: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected SQL: %v", err)
	}
}

func TestResponseCacheRepo_PutUpserts(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	cache := &responseCacheRepo{db: db}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO llm_response_cache")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := cache.Put(context.Background(), "k", "m", "p", "body", 100, 30); err != nil {
		t.Fatalf("err: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sql: %v", err)
	}
}

func TestResponseCacheRepo_PutErrorPropagates(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	cache := &responseCacheRepo{db: db}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO llm_response_cache")).
		WillReturnError(errors.New("db down"))
	if err := cache.Put(context.Background(), "k", "m", "p", "body", 0, 0); err == nil {
		t.Fatal("expected error to propagate")
	}
}

func TestResponseCacheRepo_NilSafe(t *testing.T) {
	var c *responseCacheRepo
	if _, _, _, hit, err := c.Get(context.Background(), "k"); err != nil || hit {
		t.Errorf("nil receiver: hit=%v err=%v", hit, err)
	}
	if err := c.Put(context.Background(), "k", "m", "p", "body", 0, 0); err != nil {
		t.Errorf("nil receiver put: %v", err)
	}
}

func TestResponseCacheRepo_StatsTableAbsent(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	cache := &responseCacheRepo{db: db}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(false))
	stats, err := cache.CacheStats(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if stats != (ResponseCacheStats{}) {
		t.Errorf("expected zero stats for absent table, got %+v", stats)
	}
}

func TestResponseCacheRepo_StatsPopulated(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	cache := &responseCacheRepo{db: db}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*), COUNT(DISTINCT purpose)")).
		WillReturnRows(sqlmock.NewRows([]string{"rows", "purposes", "hits"}).AddRow(42, 3, 117))
	mock.ExpectQuery(regexp.QuoteMeta("pg_total_relation_size")).
		WillReturnRows(sqlmock.NewRows([]string{"bytes"}).AddRow(8192))
	stats, err := cache.CacheStats(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if stats.RowCount != 42 || stats.DistinctPurposes != 3 || stats.TotalHits != 117 || stats.ApproxBytes != 8192 {
		t.Errorf("unexpected stats: %+v", stats)
	}
	if stats.TotalSavingsUSD != 0 {
		t.Errorf("expected zero savings without costFn, got %f", stats.TotalSavingsUSD)
	}
}

func TestResponseCacheRepo_StatsComputesSavings(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	// Cost function: $1 per 1k prompt tokens + $2 per 1k completion.
	costFn := func(_ string, prompt, completion int) float64 {
		return float64(prompt)/1000 + 2*float64(completion)/1000
	}
	cache := &responseCacheRepo{db: db, costFn: costFn}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*), COUNT(DISTINCT purpose)")).
		WillReturnRows(sqlmock.NewRows([]string{"rows", "purposes", "hits"}).AddRow(2, 1, 10))
	mock.ExpectQuery(regexp.QuoteMeta("pg_total_relation_size")).
		WillReturnRows(sqlmock.NewRows([]string{"bytes"}).AddRow(1024))
	// Two rows: (1000p, 500c) hit 3×, (2000p, 1000c) hit 7×.
	// Per-hit cost row 1: 1 + 2*0.5 = $2; total row 1 = $6.
	// Per-hit cost row 2: 2 + 2*1 = $4; total row 2 = $28.
	// Lifetime savings: $34.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT model, prompt_tokens, completion_tokens, hit_count")).
		WillReturnRows(sqlmock.NewRows([]string{"model", "prompt_tokens", "completion_tokens", "hit_count"}).
			AddRow("m1", 1000, 500, 3).
			AddRow("m1", 2000, 1000, 7))

	stats, err := cache.CacheStats(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if stats.TotalSavingsUSD != 34.0 {
		t.Errorf("expected $34.00, got $%.2f", stats.TotalSavingsUSD)
	}
}

func TestResponseCacheRepo_StatsSavingsSkippedWhenEmpty(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	costFn := func(_ string, _, _ int) float64 { return 1.0 }
	cache := &responseCacheRepo{db: db, costFn: costFn}

	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass")).
		WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(true))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COUNT(*), COUNT(DISTINCT purpose)")).
		WillReturnRows(sqlmock.NewRows([]string{"rows", "purposes", "hits"}).AddRow(0, 0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("pg_total_relation_size")).
		WillReturnRows(sqlmock.NewRows([]string{"bytes"}).AddRow(0))
	// No fourth query — RowCount=0 short-circuits computeSavings.

	stats, err := cache.CacheStats(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if stats.TotalSavingsUSD != 0 {
		t.Errorf("expected $0 on empty table, got $%.2f", stats.TotalSavingsUSD)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sql: %v", err)
	}
}

func TestNewResponseCacheWithPricing_NilDBReturnsNil(t *testing.T) {
	if c := NewResponseCacheWithPricing(nil, func(string, int, int) float64 { return 0 }); c != nil {
		t.Errorf("expected nil for nil db, got %T", c)
	}
}

func TestNewResponseCacheWithPricing_WiresCostFn(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	costFn := func(string, int, int) float64 { return 1.5 }
	c := NewResponseCacheWithPricing(db, costFn)
	repo, ok := c.(*responseCacheRepo)
	if !ok {
		t.Fatalf("expected *responseCacheRepo, got %T", c)
	}
	if repo.costFn == nil {
		t.Error("costFn not wired")
	}
}

func TestNewResponseCache_NilDBReturnsNil(t *testing.T) {
	if c := NewResponseCache(nil); c != nil {
		t.Errorf("expected nil for nil db, got %T", c)
	}
}

func TestNewResponseCache_ReturnsRepoForNonNilDB(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	c := NewResponseCache(db)
	if c == nil {
		t.Fatal("expected non-nil ResponseCache for valid db")
	}
	if _, ok := c.(*responseCacheRepo); !ok {
		t.Errorf("expected *responseCacheRepo, got %T", c)
	}
}

func TestResponseCacheRepo_StatsPropagatesQueryError(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	cache := &responseCacheRepo{db: db}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT to_regclass")).
		WillReturnError(errors.New("connection refused"))
	_, err := cache.CacheStats(context.Background())
	if err == nil {
		t.Fatal("expected probe error to propagate")
	}
}

func TestResponseCacheRepo_StatsNilReceiverSafe(t *testing.T) {
	var c *responseCacheRepo
	stats, err := c.CacheStats(context.Background())
	if err != nil {
		t.Errorf("nil receiver should be safe, got err: %v", err)
	}
	if stats != (ResponseCacheStats{}) {
		t.Errorf("expected zero stats, got %+v", stats)
	}
}

// fakeResponseCache is the in-memory test double used by the
// Titler / Classifier / Extractor cache-hit tests below. Keyed on
// the cache_key argument; ignores the (model, purpose) cols since
// hits are by-key and Put just upserts.
type fakeResponseCache struct {
	mu     sync.Mutex
	rows   map[string]fakeResponseRow
	gets   int
	puts   int
	getErr error
}

type fakeResponseRow struct {
	content    string
	promptTok  int
	completion int
}

func newFakeResponseCache() *fakeResponseCache {
	return &fakeResponseCache{rows: map[string]fakeResponseRow{}}
}

func (f *fakeResponseCache) Get(_ context.Context, key string) (string, int, int, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.getErr != nil {
		return "", 0, 0, false, f.getErr
	}
	r, ok := f.rows[key]
	if !ok {
		return "", 0, 0, false, nil
	}
	return r.content, r.promptTok, r.completion, true, nil
}

func (f *fakeResponseCache) Put(_ context.Context, key, _, _, content string, pt, ct int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts++
	f.rows[key] = fakeResponseRow{content: content, promptTok: pt, completion: ct}
	return nil
}
